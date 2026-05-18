# wasm3 M3 — Object-model breadth

Started: 2026-05-16
Branch: `wasm3-m2-cutover` (cumulative; will rename when M3 lands)
Builds on: M2 (see `doc/wasm3-m2-cutover-notes.md`)

## Goal

Per the plan, M3 lands the *full object model* — strings, slices,
arrays, interfaces, maps — represented as WasmGC heap objects rather
than the linear-memory layouts the bump-allocator stand-in uses
today. The deliverable: single-goroutine programs using strings,
slices, interfaces, maps, and the standard runtime helpers around
them run end-to-end on `wasmtime --features gc`.

By the end of M3 the bump allocator (`newobject_wasm3.go`), the
runtime-fork print shims (`printint_wasm3.go`, `gwrite_wasm3.go`,
etc.), and the linear-memory pointer convention should all be gone:
they were M2-cutover crutches that the actual ref-typed lowering
replaces.

## The architectural shift

M2 kept the Go pointer convention — `*T` is an i64 linear-memory
address — and worked around the lack of escape boxing by hoisting
scratch buffers to globals. M3 inverts this:

  - `*T` (and `unsafe.Pointer` for typed pointees) becomes
    `(ref null $T)` at the SSA level.
  - Heap allocation is `struct.new $T` directly, no runtime call.
  - Field access is `struct.get $T n` / `struct.set $T n`, no
    linear-memory load/store.
  - Strings are `(ref $go.string)` headers pointing at
    `(ref $go.bytes)` arrays — `string`'s in-register representation
    becomes one ref + two i32s (offset, length).
  - Slices are `(ref $go.backing<T>)` plus the three i32 metadata
    fields.
  - Interfaces are two refs (type descriptor + data).
  - Maps and channels keep their logic but the underlying buckets/
    queues become arrays of refs.

This is the cutover the M2 plan deferred. The encoder already
accepts the 0xFB GC opcodes; the linker already knows how to merge
per-package type tables. What's missing is the SSA-level plumbing:
ref-typed values flowing through registers, GC ops emitted from
lowering rules, and runtime helpers rewritten in terms of
struct.get/array.get rather than linear-memory loads.

## Sequencing — what lands when

**Stage A — encoder + linker plumbing for `struct.new`** (foundation)
- Encoder: handle `AStructNew` with a type-index operand
  (`0xFB 0x00 <typeIdx>`).
- A new `R_WASMTYPE` relocation: the compiler emits a per-package
  type index; the linker remaps to the module-global type index via
  the per-package wasmgc.Table merge.
- Verification: hand-written assembly that calls `struct.new`
  validates and runs.

**Stage B — `newobject` intrinsic** (kill the bump allocator)
- New SSA op `OpWasm3LoweredStructNew`.
- Intrinsify `runtime.newobject(typ)` calls at the SSA layer: if
  `typ` is statically known, replace the call with the new op.
- Drop `newobject_wasm3.go`; `new(T)` for known T emits a direct
  `struct.new`.

**Stage C — ref-typed SSA values** (the deep change)
- New SSA value class for refs (separate from i64 GP regs).
- New wasm3 register class for refs (`Ref0..Ref15`?).
- Wasm3Ops.go: ref-typed Op definitions.
- Wasm3.rules: lower `OpAddr`/`OpLoad`/`OpStore` on struct fields
  to `struct.get`/`struct.set`.
- wasm3Locals: declare wasm locals of `(ref null $T)` type.

**Stage D — string and `[]byte` as `(array i8)`**
- `string` in-register = `(ref $go.bytes, i32 offset, i32 length)`.
- `[]byte` ditto + capacity.
- Runtime helpers: `concatstring{2..7}`, `slicebytetostring`,
  `stringtoslicebyte`, etc. rewritten to use `array.new_data`,
  `array.copy`, `array.get_u`.
- Drop the printstring/printint runtime-fork shims — the standard
  print path works once `[]byte` and `string` are ref-backed.

**Stage E — slice generalised over element type**
- `[]T` for `T` a scalar: backing is `(array T)`, header carries
  `(ref $backing, i32 offset, i32 length, i32 cap)`.
- `[]T` for `T` composite: backing is `(array (ref $T))`.
- `make([]T, n)`, slicing, indexing, range — all via `array.new`/
  `array.get`/`array.set`/`array.len`.

**Stage F — interface (`type, data`) as two refs**
- Interface header: `(ref $go.type, ref $go.object)`.
- Type-assertion via `ref.cast`/`ref.test`.
- Method dispatch via the itab — itab itself becomes a struct of
  function refs (depends on **Stage G**).

**Stage G — indirect call via `call_ref`**
- Function values are `(ref $funcType)`.
- `Set CTXT` declares CTXT as a `(ref null $closureCtx)` local.
- Indirect call site emits `call_ref $funcType`.
- Closures: closure descriptor is a struct holding the function ref
  + captured-variable refs.

**Stage H — maps and channels (data structures only)**
- Map buckets become `(array (ref $bucketEntry))`.
- Channel buffer becomes `(array T)` or `(array (ref T))`.
- The logic in `internal/runtime/maps`, `chan.go`, `select.go`
  stays; the storage layer swaps from unsafe.Pointer arithmetic to
  WasmGC array ops.

**Stage I — wasmexport composite marshalling**
- The wasmexport wrapper for a `*Point` / `string` / `[]byte` param
  constructs the GC representation from the host's (ptr, len)
  primitive args. Conversely, returning a string copies the GC
  array contents into a host-visible linear-memory buffer.

**Stage J — runtime fork retirement**
- With ref types plumbed and write barriers eliminated, the
  `*_wasm3.go` runtime shims should compile their standard
  counterparts cleanly. Verify each one and retire it.

Stages B and onward each have their own bring-up ladder; expect
each stage to land as multiple commits with verification.

## Acceptance criteria for M3 done

A program like the following compiles and runs to completion on
`wasmtime --features gc`, producing the expected output:

```go
package main

import "strings"

type Counter struct{ name string; count int }

func (c *Counter) Inc() { c.count++ }

func main() {
    cs := []*Counter{
        {"apples", 0},
        {"bananas", 0},
    }
    for _, w := range strings.Fields("apples bananas apples apples bananas") {
        for _, c := range cs {
            if c.name == w {
                c.Inc()
                break
            }
        }
    }
    for _, c := range cs {
        println(c.name, c.count)
    }
}
```

This exercises strings, slices, struct pointers, method calls (which
on a pointer receiver are direct, not interface dispatch — interface
dispatch is its own checkpoint).

## Progress

**Stage A — encoder + linker plumbing for `struct.new`: ✅ DONE**
(commit `250312fafb`). `objabi.R_WASMTYPE` relocation defined;
`encodeWasm3Body` handles `AStructNew`/`AStructNewDefault` with a
type-index operand; asm3.go's main loop merges the per-function
wasmgc.Table *before* writing the body, then passes the remap to
`writeWasm3FuncBody` so R_WASMTYPE relocations resolve to module-
global type indices. The plumbing is exercised the moment Stage B
emits the first AStructNew prog. Regression sweep still passes 14/14
— nothing was broken by adding the plumbing.

**Stage B — `newobject` intrinsic + ref-typed SSA values**: in
progress. The hard regalloc-class pre-req is already gone (Phase 4
of doc/wasm3-m3-no-regalloc.md skipped regalloc entirely; per-
value locals can be any wasm type). Foundation commits landed:

- `38c4ae1366`: per-function `typeCollector` stashed in a sync.Map
  keyed by `*obj.FuncInfo` so SSA codegen can register additional
  body-emitted types and reserialise `wt.Table`. The codegen path
  for `OpWasm3StructNew/Default` and `OpWasm3StructGet` now emits
  the R_WASMTYPE operand the linker needs.
- `8269aaec7e`: `wasm3ValueType` returns `(ref null any)` (0x6E)
  for SSA values produced by ref-creating Wasm3 ops
  (StructNew/NewDefault, ArrayNew/NewDefault, RefNull, RefCast).
  The per-value local for a struct.new result lands at the right
  wasm type without an R_WASMTYPE relocation in the locals
  section — the abstract heap type accepts every typed-ref subtype
  by wasmgc subtyping.
- `0d7f359e84`: obj encoder cases for `AStructGet[SU]` /
  `AStructSet` (two-operand: type index + field index) and
  `ARefCast` / `ARefTest` / `ARefNull` (single type-index
  operand). The codegen-to-bytes pipeline is now complete for
  the wasmgc op family.

Still needed for Stage B to actually trigger end-to-end:
- An SSA-level intrinsic (or rewrite rule) that replaces
  `runtime.newobject(typ)` with `OpWasm3StructNewDefault` carrying
  `v.Aux = typ`.
- Consumer-side ref handling — without it the StructNew result has
  no Go-level use and SSA DCE drops it. Stage C's
  `OpAddr`/`OpLoad`/`OpStore` lowering rules provide the consumers
  via struct.get / struct.set; until they land, the struct.new
  op is dead code.

Once both pieces are in place, the bump-allocator `runtime.newobject`
shim can retire.

**Stage D — strings/[]byte/arrays as `(ref (array T))`**: ✅
core landed.

- `670f621f91`: stack-allocated `var buf [N]T` lowers to a
  wasmgc `(ref (array T))`. New `OpWasm3StackArray` generic-
  pattern op + 14 lowering rules in Wasm3.rules covering
  const-offset and `base + idx * elemSize` loads/stores for
  i8/i16/i32/i64 element sizes (signed and unsigned). The
  ssagen.addr() hook caches per-name so subsequent &buf
  references share the allocation. Verified end-to-end:
  `var buf [4]int64; for i { buf[i] = i+1 }; for i { sum += buf[i] }`
  produces the expected sumBuf(N)=1+...+N.

What's still on the shim path (Stage J retirement scope):
runtime/printnum_wasm3, printstring_wasm3,
write1_wasip1_wasm3, printlock_wasm3.

The print shims hit the same root cause: WASI's fd_write takes
a linear-memory pointer. Once `var buf [N]byte` is a wasmgc
`(ref (array i8))`, `unsafe.Pointer(&buf[i])` can't materialise
that pointer — there's no way to get a raw pointer into a
wasmgc array. The shims sidestep by either hoisting the buffer
to a package global (lives in linear memory via the data
section) or computing only with refs that never need to cross
into linear memory. Retiring them needs a wasmgc ↔ linear-
memory bridge:

  - `array.copy` from the wasmgc buffer into a known scratch
    region of linear memory, call fd_write against that region,
    then drop. One alloc per write.
  - Or WASI 0.3 / component-model fd_write that takes an array
    ref directly (waiting on engine support).

Bridging cleanly is its own piece (sketched in Stage I —
wasmexport composite marshalling — which has the same
host-i32-pointer ↔ wasmgc-ref translation problem).

`printlock_wasm3` is the odd one out: not a buffer issue,
it's the goroutine lock subsystem. The standard `lock(&debuglock)`
reaches `gopark` on contention; wasm3's proc stub leaves
`gopark` as an `unreachable` trap, so even uncontested calls
that touch the parking path crash. M4's scheduler bring-up
is the retirement gate.

Smaller Stage J piece landed in this arc (commit `ed124f76fe`):
`runtime.bytes()` rewritten to use `unsafe.Slice(unsafe.StringData,
len)` instead of the reflection-based `(*slice)(unsafe.Pointer(
&ret))` shape. The `&ret` was the only thing keeping the
standard `gwrite(bytes(s))` path off-limits to wasm3; bytes()
no longer takes the address of a stack slice header.

### Bridge — wasmgc ↔ linear memory for fd_write

The blocker for retiring the buffer-shim trio (printnum_wasm3,
printstring_wasm3, write1_wasip1_wasm3) is structural: WASI
fd_write takes a linear-memory pointer, but Stage D made
`var buf [N]byte` a wasmgc `(ref (array i8))` from which
`unsafe.Pointer(&buf[i])` cannot materialise a linear-memory
pointer. wasmgc has no built-in opcode that copies from an array
ref to linear memory — `array.copy` is gc → gc only.

The bridge is the obvious shape: a package-global byte array
(which lives in the data section, i.e. linear memory, so
`&scratch[0]` is a real i32 pointer) plus a small Go loop that
copies element-by-element from the wasmgc array into the
scratch using `scratch[i] = buf[i]`. The load is `array.get_u`
(wasmgc), the store is `i32.store8` (linear), and Go bridges
them through an i64 register. The fd_write call then runs
against `&scratch[0]`.

Five compiler-side pieces had to land first to make even this
minimal bridge code generate:

1. `(array (mut i8))` backing for `[N]byte` stack arrays.
   `collectBacking`'s pre-bridge path went through `scalarPrim`,
   which maps TUINT8→i32 (correct for struct fields, since
   wasmgc field storage rounds up but wrong for array
   element storage where packed i8/i16 is both legal and
   tighter). A new `packedArrayPrim` shortcuts the byte/short
   array case to `PrimStorage(I8)` / `PrimStorage(I16)` so the
   `[N]byte` backing matches Stage D's expectation that the
   byte access lowers to `array.get_u`, not a 32-bit get.

2. Packed-aware ArraySet codegen. wasmgc `array.set` on a
   packed array takes i32 (the value is implicitly truncated
   to the storage width); the pre-bridge codegen pushed i64.
   Wrap to i32 with `i32.wrap_i64` when `elemSize < 8`.

3. Packed-aware ArrayGet codegen. wasmgc rejects plain
   `array.get` on packed storage — it cannot decide between
   sign-/zero-extension to i32. Emit `array.get_u` or
   `array.get_s` based on `v.Type.IsSigned()`, then
   `i64.extend_i32_{u,s}` so the result lands as i64 in the
   per-value local (every wasm3 GP local is i64). The Stage D
   rules in `Wasm3.rules` now annotate the SSA result type
   with `typ.UInt8`, `typ.Int8`, `typ.UInt16`, etc., so the
   codegen can read the signedness off `v.Type`.

4. Zero-on-StackArray is a no-op. `array.new_default`
   initialises every element to zero already, so ssagen's
   stock `var buf [N]byte` zeroing pattern (which expands a
   16-byte zero into two `i64.store` ops at offsets 0 and 8)
   would target a stack array with i64 stores — no Stage D
   rule covers an i64-store on an i8 array. The Stage D rule
   `(Zero [_] (StackArray _) mem) => mem` discards the redundant
   zeroing entirely.

5. Skip memcombine for StackArray destinations.
   `combineStores` (the generic memcombine pass) fused
   adjacent byte stores into wider `i64.store16` / 32 / 64
   ops to amortise the linear-memory store cost. On a wasmgc
   byte array there is no wide-store opcode — the storage is
   element-by-element. Bail in `combineStores` when
   `splitPtr`'s base is `OpWasm3StackArray` so the byte
   stores stay byte-wide and the Stage D rules can lower
   each one to `array.set`.

End-to-end demonstration (`/tmp/wasm3-bridge/main.go`):
`bridgeWriteHello` composes `"hello, wasmgc\n"` in a
`var src [14]byte`, copies through a package-global
`var bridgeScratch [64]byte`, hands `&bridgeScratch[0]` to
`fd_write(1, ...)`, and the host stdout prints the string.

First user landed: `runtime/printnum_wasm3.go` now uses a
stack-allocated wasmgc buf rather than the package-global
`printnumBuf` hoist that documented this exact blocker. The
copy loop is inlined into both `printuint` / `printint`
because the wasm3 backend does not yet bridge a wasmgc ref
across a function-call boundary — passing `*[N]byte` lowers
the parameter to i64 and the caller's anyref local then
mismatches the callee's i64 expectation. Cross-function ref
passing is Stage I-shaped (wasmexport composite marshalling).

Still on the shim path after this arc: `printstring_wasm3`
(blocked on gwrite/writeErr/writeErrData/write1 chain — the
slice header path needs Stage E), `write1_wasip1_wasm3` (its
`iovec` and `nwritten` autos need linear-memory addresses
for fd_write; the package-global hoist is the same trick the
old printnumBuf did and doesn't benefit from the bridge),
`printlock_wasm3` (gated on M4 scheduler bring-up — the
bridge does not apply).

- `2a3c74ca4b`: `typeCollector.lowerFields` lowers TARRAY as a
  single `(ref (array T_elem))` field via the existing
  collectBacking path (already in place for the TSLICE case). The
  type section becomes valid regardless of array length — the
  10000-field engine cap that blocked `go test -c` for any package
  linking `testing` is gone. /tmp/audit.test drops from 2.2MB to
  410KB and now passes type-section validation.
- `740e1964ce`: obj encoders for `AArrayNew/Default`,
  `AArrayGet[SU]`, `AArraySet`, `AArrayFill`, `AArrayLen`,
  `AArrayCopy` — the byte layouts for the 0xFB-prefixed array GC
  opcodes; R_WASMTYPE relocations on each type-index operand.
- `dc8b06568a`: `OpWasm3ArrayNew/Default/Get` codegen now sets
  `p.From.Offset` via a new `wasm3RegisterArrayAux` helper that
  resolves the array's element type to a `collectBacking` index in
  the function's per-package wasmgc.Table — same pattern as the
  struct codegen wired in `38c4ae1366`.
- `58751f7f28`: `TestCollectArrayField` locks in the array-field
  lowering for sizes 1 / 4 / 256 / 65504 (the size that produced
  the original blocker).
- `8d47cbf50d`: retire `runtime/gwrite_wasm3.go`. The standard
  gwrite's `recordForPanic` writes to a package-global byte array
  and the `gp.writebuf` check naturally takes the `writeErr →
  write1` path when wasm3's minimal proc stub leaves writebuf nil.
  One of Stage J's print-shim retirements landed early.

Still needed for Stage D to actually fix the test harness:
- SSA-side lowering for struct-field array accesses. The SSA
  backend still emits `i64Load(struct_base + array_offset + i *
  elem_size)` for `s.buf[i]`, which doesn't match the new
  ref-typed field. wasm validation now fails one step later —
  on the function body — with "expected i64, found (ref $type)".
- Stack-allocated arrays (`var buf [N]T`) need a separate
  treatment. The SSA backend addresses them via SP-relative
  `Get $auto-off(SP)`, an op the wasm3 obj backend doesn't
  encode; it bails out and the linker stubs the function.

### Implementation guide for the SSA-side stack-array work

Goal: a function like

    func sumBuf(n int32) int32 {
        var buf [4]int32
        for i := int32(0); i < n; i++ { buf[i] = i + 1 }
        var s int32
        for i := int32(0); i < n; i++ { s += buf[i] }
        return s
    }

compiles to:

    array.new_default $arr_i32; local.set $buf
    ... loop emitting array.set $arr_i32 $buf (i) (i+1) ...
    ... loop emitting array.get_s $arr_i32 $buf (i) ...

Recommended path (option 1 — cleanest, mirrors how decimal/strconv
already extends genericOps):

1. **New generic SSA op `OpWasm3StackArray`**. Aux: `*types.Type`
   (the Go `[N]T` array type). Result: a `(ref null any)`-typed
   SSA value (in Go-type terms: a `*[N]T` but flagged for the
   wasm3 backend to lower as `(ref (array T))`). argLength: 1
   (memory). Lives in genericOps.go but is only ever emitted by
   wasm3.

2. **ssagen patch in `Compile` (just before `genssa`)**: walk
   `fn.Dcl` for TARRAY autos that are address-taken. For each,
   replace the `s.decladdrs[n]` entry from `OpLocalAddr` →
   `OpWasm3StackArray`. The resulting SSA value flows through the
   existing call sites unchanged at the SSA level — Go's type
   system still sees `*[N]T`.

3. **Wasm3.rules pattern-match the `load(OffPtr(stackArrayRef))`
   shape**:

       (I64Load32U [off] (Wasm3StackArray <t> {sym}) _)
           && t.Elem().Elem().Size() == 4
           => (Wasm3ArrayGet {sym} (Wasm3StackArray <t> {sym}) (I32Const [int32(off/4)]))

   One rule per element-size variant (i8/i16/i32/i64) and signed
   vs unsigned. Symmetric rules for store → Wasm3ArraySet. The
   element index is `off / element_size`; the rule must reject
   non-aligned offsets (won't happen for legitimate Go array
   access).

4. **wasm3 backend codegen for `OpWasm3StackArray`**: emit
   `i32.const <NumElem>; array.new_default $T_elem; local.set
   <ref_local>` at first use, then `local.get <ref_local>` to push
   the ref. The "first use" lives in the entry block by design
   (step 2 puts it in `s.decladdrs`, which is materialised at
   function start). Per-value local is anyref-typed (already
   handled by `wasm3ValueType` since `8269aaec7e`).

Alternative path (option 3 — smallest SSA-side change, biggest
obj-backend change): keep the SSA flowing i64 pointers, but
thread a new FuncInfo aux `Wasm3AutoArrays []AutoArrayInfo`
populated by ssagen with `(NameOffset, ElemType, NumElems)`
tuples. The wasm3 obj backend reads it, emits array.new_default
at entry, and pattern-matches the `Get $auto(SP)` + arithmetic
+ load/store sequence to translate. Avoids generic-op churn but
the pattern matching is fragile (different optimisation levels
produce different prog shapes).

Option 1 is recommended once a session can dedicate the focused
SSA-rules + ssagen work.

## Stage E — slice progress

**Stage E phase 1: slice as Go-ABI value via the bump heap. ✅**

End-to-end Go slice operations work on wasm3:

- `make([]T, n)` — runtime call into the wasm3 fork's bump-heap
  makeslice (a new `runtime/makeslice_wasm3.go` shares the
  newobject heap; the original mallocgc path lives in
  `makeslice_default.go` behind `!wasm3` so the wasm3
  binary doesn't link mallocgc / the page allocator /
  the trace locker).
- `s[i] = v`, `v = s[i]` — standard SSA pointer arithmetic
  on the slice's i64 ptr field; the backing is linear
  memory carved from the bump heap, so existing
  load/store rules apply unchanged.
- `len(s)`, `cap(s)`, `s[a:b]`, ranging via `for i` — all
  through the existing slice-header SSA decomposition.
- Passing a slice across functions — the new
  `tryFlatPrimitiveAttach` fallback lowers a slice param
  / result to 3 i64 fields (ptr, len, cap) matching what
  the SSA caller pushes.

End-to-end (`/tmp/wasm3-slice/main.go`): sumSlice / sumPassed /
sumSub / sliceLen / sliceCap all return expected values for
sizes 1..100.

Commits this arc:

- `7b5825e8c2`: `tryFlatPrimitiveAttach` fallback for any
  function whose primitive- and collector-attach paths both
  bail. Walks each Go param / result recursively and emits
  one wasm field per Go-ABI register slot — pointer-shaped
  leaves go to i64 (matching SSA's push), multi-word headers
  (string=2, slice=3, interface=2) flatten to their natural
  shape. `collectorMatchesRegabi` is tightened to reject
  struct-with-pointer fields so traceLocker-shaped returns
  fall into the flat path with a regabi-matching signature
  instead of producing a ref-typed result the SSA call site
  can't satisfy. Two locking tests.
- `244aec5440`: `walkMakeSlice` skips the constant-size and
  tryStack array fast paths on wasm3 (they wrap a `[K]E` in
  a struct-with-padding auto the wasm3 backend can't address).
  `runtime.makeslice` / `.64` split into per-target files; the
  wasm3 version uses the bump heap and avoids mallocgc.

**Stage E phase 1 known gaps (carry into phase 2 or adjacent
work).**

These are the operations that still fail on phase 1's bump-heap
slice — each has its own blocker independent of the wasmgc
representation change:

- `append(s, v)` — the obj backend emits an `unreachable` stub
  for `main.sumAppend`. Traced via `GOWASM3DEBUG=bail:<sym>` to:

  `Get $main..autotmp_7-32(SP)` — a TYPE_ADDR / NAME_AUTO
  reference to an SP-relative compiler-internal temp at
  offset -32. The walk pass for `s = append(s, v)` materialises
  a small stack scratch (likely an `[8]int32` for the typical
  fast-path) and the SSA backend addresses it via SP. The
  wasm3 obj backend rejects this on principle (no Go stack
  frame); Stage D's `OpWasm3StackArray` hook in `ssagen.addr()`
  only intercepts named TARRAY autos that flow through `addr()`,
  not compiler-internal autotmps the walker creates via
  `typecheck.TempAt`.

  Fixing this means routing the autotmp path through the same
  hook — either by intercepting `typecheck.TempAt` for TARRAY
  types on wasm3, or by extending the SSA `addr()` PAUTO branch
  to also catch TSTRUCT autos wrapping a single TARRAY field
  (the shape walkMakeSlice used and the append fast-path also
  uses).

  Landed in `09f64d7dc9`: skip the append fast-path on wasm3
  (same shape as the walkMakeSlice change). `main.sumAppend`
  now compiles to a real wasm function instead of an
  `unreachable` stub. But the runtime chain it newly reaches
  surfaces the *next* blocker:

  - `runtime.growslice` panics on overflow → `panicmakeslicelen`
    → `gopanic` → `printlock`/`printhex`/... and crucially
    `runtime.printhexopts` (validation error at offset 0x8046:
    `i64.eqz` reading a local typed `anyref`).

    printhexopts' body is `var buf [100]byte; ... gwrite(buf[i:])`.
    Stage D made the stack `[100]byte` a wasmgc `(ref (array i8))`,
    so the slice expression `buf[i:]` extracts the array ref as
    the slice's data pointer. The slice's `OpSlicePtr` is i64-
    typed (the generic SSA layer treats slice ptrs as BytePtr),
    but the wasm local now holds an anyref. The first time the
    SSA layer compares the slice ptr to nil — `i64.eqz` after
    `local.get <anyref>` — wasm validation rejects the module.

    Same root cause as the memmove/copy chain: slice-from-stack-
    array doesn't work until Stage E phase 2 (the wasmgc slice
    header `(ref backing, off, len, cap)`) is implemented. Every
    runtime function that calls `gwrite(buf[i:])` — printhexopts,
    printfloat, printcomplex, printslice, printquoted — has the
    same shape.

  After phase 2 lands and slice ptrs are ref-typed at the SSA
  layer too, both the append-via-real-growslice path AND the
  memmove/copy path work transparently — they're the same
  underlying fix.

- `copy(dst, src)` slice-to-slice copy — reaches `runtime.memmove`
  which in `memmove_wasm3.s` reads its args via `MOVD .+N(FP)` on
  a Go stack frame the wasm3 runtime doesn't have. A wrapper the
  backend auto-generates stores the wasm-level params to address
  0 (uninitialised "FP" local) and then calls into an
  `unreachable` stub for the asm body.

  Investigations in two carry-on rounds:

  1. Rewrite the asm to `Get R0/R1/R2` directly + extend
     `enqueueFunc`'s bodyless branch to call `PrepareFunc` on the
     runtime forward decl. The forward-decl `*ir.Func` and the
     asm-defined LSym are distinct enough that the WasmType
     attachment doesn't reach `encodeWasm3Body`. Reverted.

  2. Synthesise a flat `(N i64)` `WasmType` in `assemble3` by
     scanning the prog stream's max `R{N}` reference (so memmove's
     `Get R0/R1/R2` infers 3 i64 params) + add memory-index
     immediates for `memory.copy` / `memory.fill` in
     `encodeWasm3Body`'s operand-less switch. The asm body then
     compiled to a correctly-typed wasm function. But the
     auto-generated ABIInternal→ABI0 wrapper still emits the
     standard frame-store sequence (param→spill, spill→`i64.store`
     at "FP+N") and then issues a typed call with nothing on the
     wasm stack — the wrapper's call lowering iterates
     `call.ABIInfo().InParams()`, which for an ABI0 callee returns
     empty (args go via the frame), so no register pushes happen.
     Skipping wrapper generation on wasm3 broke linker symbol
     resolution. Reverted.

  The cleanest fix needs the wasm3 SSA call lowering to push args
  for ABI0 callees too (since wasm3 has no Go-stack-frame ABI),
  *plus* the asm synthesis + memory-index immediate from round 2.
  Three pieces, one focused session — not fragile to attempt
  out-of-order.

  Same shape blocks `runtime.memclrNoHeapPointers`, used by
  growslice's zero-the-tail path. Once the call-lowering fix
  lands, both retire.

**Stage E phase 2 + 3 — wasmgc backing for slices (LANDED).**

Three commits this session landed phase 2.A / 2.B / 2.C / 2.D:

- `5296610763` (2.A / 2.B / 2.C): `OpWasm3MakeSlice` SSA op,
  intrinsic replacing `runtime.makeslice` on wasm3, and 26 new
  Wasm3.rules patterns lowering ref-backed slice element access
  to `array.set` / `array.get` on the wasmgc backing.
- `15084bbe0e` (2.D infra): `obj.WasmAnyref` field type +
  `wasmgc.Storage.AnyRef` variant + `0x6E` encoding, so the
  wasm function signature can declare a slice-ptr parameter as
  the abstract anyref heap type.
- `0dbbaa4a22` (2.D rules): `ssa.Wasm3SliceArgElemType` helper
  recovers the slice's element `*types.Type` for an
  `OpArgIntReg` of a TSLICE param's data field, and 26 more
  Wasm3.rules patterns lower the resulting
  `(I64Add OpArgIntReg ...)` shape to ArrayGet / ArraySet.
- `3b965d40c9` (Stage E phase 3): `OpWasm3SubSlice` op +
  ssagen.slice() hook that intercepts the OpAddPtr step on
  wasm3 to instead emit a deep-copy via array.new_default +
  array.copy. New 5-arg-with-aux NewValue5A in ssa/func.go.
  26 more Wasm3.rules patterns mirror the MakeSlice rules with
  SubSlice as base.

End-to-end verified:

  - `/tmp/wasm3-e2-min/main.go`: sumSlice(n) — local make() +
    in-function index — returns n*(n+1)/2 for n ∈ {1, 2, 5, 10,
    100} via wasmgc backing, no runtime call.
  - `/tmp/wasm3-passed/main.go`: sumPassed(n) — local make() +
    cross-function slice passing to sumOf — same expected
    values; sumOf's compiled body uses
    `ref.cast (ref $arr_i32); array.get $arr_i32` on its
    anyref slice-ptr parameter, no linear-memory load anywhere
    on the slice path.
  - `/tmp/wasm3-slice/main.go` sumSub(n,lo,hi) — sub-slicing
    via OpWasm3SubSlice: 25, 4040, 15, 40 for the four test
    cases. The sub-slice has a fresh backing populated via
    array.copy from the parent — semantically a deep copy.

SEMANTIC LIMITATION: writes to a sub-slice do not propagate to
the parent's backing under phase 3's deep-copy scheme. Reads of
sub-slices are exact. The design-doc's (ref backing, off, len,
cap) header would share backing and adjust offset to avoid the
copy; that's a generic SSA slice-representation rework
(broader blast radius across all arches) deferred for now.

What still breaks (the next session's gate):

  - **Sub-slice aliasing semantics.** Phase 3's deep-copy
    backing means a write to a sub-slice doesn't propagate to
    the parent. Fixing this needs the design's (ref backing,
    off, len, cap) header — a generic SSA slice representation
    change that ripples across all arches. Deferred until a
    test program demands shared-backing semantics.

  - **append(s, v).** The append-fast-path skip from
    `09f64d7dc9` still routes the call to `runtime.growslice` /
    `printhexopts` whose stack-array → slice-from-it shape
    needs the same offset-bearing slice header.

  - **String / interface ABI.** Same `flatPrimitiveFields` site
    now lowers TSTRING / TINTER to (i64, i64) but their data
    pointers want anyref too. The pattern from 2.D extends
    naturally; slice was the first user, the others retire when
    a string-handling test program demands it.

  - **`copy(dst, src)` slice-to-slice.** Lowers to
    `runtime.memmove(dst.ptr, src.ptr, n)`. memmove's signature
    is `(i64 dst, i64 src, i64 n)` — linear-memory pointers —
    but on wasm3 both ptrs are anyref. Calls fail wasm
    validation with "expected i64, found anyref" on every
    `copy()` of a wasmgc-backed slice.

    **Update (attempted, reverted):** the obvious SSA intrinsic
    on `runtime.memmove` blows up: there are THREE distinct
    walk-side memmove emission sites (walkCopy inline,
    assign.go's append-many path, builtin.go's makeslicecopy),
    plus inlining can clone the call into a different
    `*ir.CallExpr` than the one walkCopy stashed against.
    Stashing the elem type by CallExpr identity fails when
    the post-inline call is a clone. The right shape is
    probably a NEW dedicated runtime symbol
    (`runtime.wasm3SliceCopy`) that walkCopy on wasm3 emits
    instead of memmove — runtime-internal memmove callers
    don't trigger the intrinsic, and the elem-type can ride
    along as an explicit rtype arg (like makeslice's pattern).

    **Hunch: take the SSA-time intrinsification route.** Two
    paths considered:

    1. *Runtime-side wasm3 memmove.* Add a `memmove_wasm3.go`
       that takes `(anyref dst, anyref src, int)` and uses
       `array.copy` internally. The signature change cascades
       through every memmove caller (ABI-incompatible with
       linear-memory uses elsewhere in the runtime, e.g.
       packing scratch buffers, struct copies, gc bitmap moves).
       Would also need a parallel linear-memory memmove for
       the i64 cases.

    2. *SSA-time intrinsic for the `copy()` builtin.* The
       compiler already intrinsifies `runtime.makeslice` on
       wasm3 (see commit 5296610763); the same hook point
       applies to `copy()`. Add an entry to
       `ssagen.initIntrinsics` for the slice-to-slice copy
       lowering that emits `OpWasm3ArrayCopy` directly. No
       runtime ABI change, no parallel implementations, and
       it dovetails with how Stage E phase 3 already uses
       `array.copy` in `OpWasm3SubSlice`'s codegen.

    The intrinsic path is the recommended one — keeps the
    wasm3 deviation contained to the compiler, leaves the
    runtime fork's memmove footprint unchanged, and reuses
    the wasmgc `array.copy` emission we already exercise.

The original phase-2 work breakdown (kept for reference):

1. SSA representation change: `OpSliceMake`'s ptr arg
   becomes a wasmgc ref, not a BytePtr i64. The `OpSlicePtr`
   accessor returns a ref-typed value. The wasm3 per-value
   local for the slice's ptr component is anyref.

2. Lowering rules for indexing / slicing on ref-typed slice
   pointers. The pattern is the same shape as Stage D's
   stack-array rules but with a different base: `(I64Load
   [off] (I64Add (OpSlicePtr s) (I64Shl idx ...)) _) =>
   (Wasm3ArrayGet ...)`. The offset field needs to fold
   into the index calculation.

3. `make([]T, n)` intrinsic: at SSA-time replace the
   `runtime.makeslice` call with an `OpWasm3MakeSlice` that
   emits `array.new_default $T_elem` and constructs the
   slice header `(ref, 0, n, n)`. Retires the bump-heap
   path for slices.

4. Cross-function slice ABI in the typed signature:
   slices lower to 4 wasm fields `(ref, i32, i32, i32)`,
   not the current 3 i64. tryFlatPrimitiveAttach grows a
   TSLICE branch that produces the ref-typed header, and
   the SSA call site pushes the slice ref instead of an
   i64.

5. Sub-slicing `s2 := s[a:b]` adjusts the offset field:
   `s2 = (s.backing, s.offset + a, b - a, s.cap - a)`. No
   reallocation, no copy.

6. Runtime helpers (`growslice`, `slicecopy`,
   `slicebytetostring`, etc.) ported to ref-backed slices
   — most need the wasmgc <-> linear-memory bridge from the
   M3 Stage D notes, since they touch the backing through
   what was previously a linear-memory pointer.

7. Retire the bump-heap makeslice shim (the bump heap stays
   for newobject until Stage B's `OpWasm3LoweredStructNew`
   intrinsic lands).

Per-piece sizing: roughly two more sessions of focused SSA-
rules + ABI + runtime work, with regression sweeps after each
phase. Phase 2 unblocks two adjacent shim retirements —
`printstring_wasm3` (gwrite's slice path) and
`write1_wasip1_wasm3` (the iovec / nwritten autos can move
back to the standard stack pattern once slice/array autos
work cleanly).

## Stretch — interfaces + closures

`fmt.Println` (interface dispatch) + a higher-order `func` value
(closure) running end-to-end is the M3 stretch goal. The plan groups
these under M3 but they could slip to M3.5 / early M4 depending on
how the ref-typed-call rung lands.

## Stage G — function values + `call_ref` (survey)

Test program: `/tmp/wasm3-funcval/main.go` — `apply(add, 5, 3)`,
a bare top-level function passed as `func(int, int) int`. No
closures, no methods. Currently this **builds** but **traps** at
`unreachable` inside `main.apply`: the obj-encoder's wasm3 path
(`cmd/internal/obj/wasm/wasm3obj.go:507`) bails on indirect ACALL
(`p.To.Type != obj.TYPE_MEM`), falling through to the wasm1
fallback at `wasmobj.go:485`. That fallback expects the wasm1
runtime model — REG_SP-resident goroutine stack, PC_F/PC_B
encoding, ACallIndirect against the funcref table — none of which
wasm3 has set up.

What needs to change for `apply(add, 5, 3)` to work end-to-end:

1. **Funcvalue representation.** Today `staticdata.FuncLinksym`
   (consumed at `ssagen/ssa.go:3082`) produces a `*ptr` to the
   function's closure linksym. For wasm3 the equivalent must
   produce a `(ref $closureCtx)` — a struct whose first field
   is a `(ref $funcType)`. For top-level functions with no
   captures, the closure object is a one-field singleton
   struct `{ ref.func $name }`.
2. **Type collector.** Each function type used as a value (the
   `$funcType` referenced by `ref.func` and `call_ref`) gets
   a wasmgc `(func (param ...) (result ...))` type-section
   entry. Already partially in place for direct-call sigs;
   needs to cover indirect-call sigs and be reachable from the
   `OpAddr`-of-PFUNC site.
3. **flatPrimitiveFields TFUNC branch.** A `func(...)` slot in
   a param/result list lowers to a single `WasmAnyref` field
   (mirrors what the TSLICE branch from 2.D did for slices).
   The call ABI then passes the funcvalue as one ref.
4. **Indirect call lowering.** `OpWasm3LoweredClosureCall`
   currently emits `obj.ACALL` with TYPE_NONE. For wasm3 this
   needs to extract the funcref from the closure (`struct.get
   $closureCtx 0`), then emit `call_ref $funcType`. The
   `$funcType` typeidx needs to be pinned at SSA time from
   the call's ABIInfo.
5. **Obj-encoder support.** Add cases for `ARefFunc` (opcode
   0xD2, funcidx operand, R_CALL reloc) and `ACallRef` (opcode
   0x14, typeidx operand, R_WASMTYPE reloc) in the inner
   switch at `wasm3obj.go:545`. `AReturnCallRef` moves out of
   the bailout list to a real case for tail-call-via-funcref.
6. **Static-data linksym for closure singletons.** The
   per-function closure object (currently a 1-word linear-
   memory record at `staticdata.go`'s FuncLinksym path) needs
   a wasm3-specific producer that emits an init-section
   `(struct.new $closureCtx_<sig> (ref.func $name))`
   global, or it gets lazily materialised at first use. Init
   sections are simpler if the test only needs one shot.

The cleanest first vertical slice is the bare-function case
(no captures). Method values and closures with captured
variables then extend the same `$closureCtx` machinery — the
struct grows additional fields for each capture, but the
call-site lowering is unchanged.

**Stage G blocks Stage F.** Interface method dispatch builds
the itab as a `(struct (ref $funcType_method1) (ref
$funcType_method2) ...)`; without call_ref there's nothing to
invoke off the itab struct.

Sizing: roughly two sessions, plus a third for closures with
captures. Each piece (encoder, type-collector, ABI, lowering,
linksym producer) lands as its own commit.

### Stage G implementation status (2026-05-17)

Landed:

- **Piece 5 (obj-encoder support)** — `f77198200e`. `ARefFunc`
  emits opcode 0xD2 + reloc; `ACallRef` and `AReturnCallRef`
  emit opcode 0x14 / 0x15 + R_WASMTYPE-relocated typeidx.
- **Piece 2 (type collector)** — `bf7b5d071b`. `collectClosureCtx`
  returns the table index of a one-field struct
  `(struct (ref $funcType))` subtyping `$go.object`. Memoised by
  `*types.Type`.
- **Pieces 1 + 3 + 4 (end-to-end bare-func case)** — `1148ce0b08`.
  New SSA op `OpWasm3FuncValue` (aux=*obj.LSym, anyref-typed
  per-value local) emits `ref.func $sym; struct.new $closureCtx`.
  ssagen produces it for PFUNC on wasm3. TFUNC ABI lowers to one
  WasmAnyref field. `OpWasm3LoweredClosureCall` codegen, when
  the closure's *types.Type is func or *func, drops the SSA
  codeptr arg and emits `ref.cast (ref $closureCtx); struct.get
  $closureCtx 0; call_ref $funcType`. ssagen suppresses the
  rawLoad of the codeptr on wasm3 for func-typed closures (passes
  a dummy const 0 for the OpClosureLECall arg shape). The
  R_WASMREFFUNC reloc — distinct from R_CALL — marks `ref.func`
  targets so the linker can emit a passive-declared element
  segment, satisfying wasm 3.0's "every ref.func target must be
  declared" validation requirement.

End-to-end verification: the funcval test
(`/tmp/wasm3-funcval/main.go`) now prints

    add: 8
    sub: 2

on `wasmtime --wasm gc -W function-references=y`. M2 14/14
regression passes; hello / algos / sortMe audit programs produce
their expected output and wasm-tools validate clean.

Wasmtime invocation note: the audit programs now reference
`function-references` proposal opcodes even though their Go source
never uses function values directly. This is closure-call DCE
residue — the runtime contains closures-with-captures call paths
that fall through to the legacy ACALL TYPE_NONE → wasm1
trampoline; that path doesn't run today, but the code section
still encodes the ref.func references the linker generated. Will
revisit once closures-with-captures are also relooped onto
call_ref. Until then run with `-W function-references=y`.

Landed (2026-05-17 follow-up — closures with captures):

- **Closures with captures** — `fb847eee19`.
  `makeAdder(5)(10)` → 15 on wasmtime `--wasm gc -W
  function-references=y`. walkClosure on wasm3 wraps the
  &struct{F, captures...} literal in a runtime.wasm3WrapClosure
  call (closureType *byte, funcsym uintptr, captures
  unsafe.Pointer) returning unsafe.Pointer with the call's IR
  type overridden to clo.Type(); the SSA intrinsic lowers it to
  OpWasm3MakeClosureRef. The closureCtx grows a second i64 field
  for the captures pointer; the call site stores this in a new
  wasm `CTXT` global (index 1, added to writeGlobalSec3), and the
  closure body's existing CTXT-relative capture-load path keeps
  working. wasm3ValueType now recognises TFUNC and *TFUNC SSA
  values so per-value locals for func-typed values are anyref.

Landed (2026-05-17 follow-up — static closure singletons):

- **Piece 6 (static closure singletons)** — `774b646497`.
  OpWasm3FuncValue emits `global.get` of a per-(sym, closureCtx)
  wasm global. The linker (cmd/link/internal/wasm/asm3.go)
  collects singletons lazily via `m.getOrAllocSingleton` as
  R_WASMCLOSURESINGLETON relocations are processed, then
  writeGlobalSec3 emits one `(ref null $closureCtx)` immutable
  global per pair with init expression `(struct.new $closureCtx
  (ref.func $sym) (i64.const 0))`. Wasm 3.0 const-expr allows
  ref.func, numeric consts, and struct.new with const-expr
  operands, so the singleton materialises at module load. Saves
  one heap allocation per bare-function-value evaluation.

Not yet landed:

- **Method values, bound methods**. walkMethodValue uses the
  same `&struct{F, X0}` literal pattern as walkClosure. The
  obstacle is that the `&Counter{}` (and similar) struct literals
  in test programs that use method values preferentially
  SP-allocate via `OpAddr {auto} (SP)` slots that the wasm3 obj-
  encoder has no encoding for. Investigation attempts:
    1. Adding an SP-relative AGet branch in wasm3obj.go
       (`global.get 0; i64.extend; i64.const off; i64.add`) made
       method-value test programs compile but broke
       `strconv.AppendComplex` validation in unrelated stdlib
       code ("expected i64, found anyref") — some downstream
       pattern can't tolerate the SP encoding.
    2. Skipping `n.Prealloc` in walkMethodValue (same as the
       walkClosure fix from add2e0e83f) doesn't help: the
       receiver struct (`&Counter{}`, separate from the method-
       value captures) is allocated via OCOMPLIT escape analysis
       and stays SP-relative.
    3. Forcing `addr.SetEsc(ir.EscHeap)` on the captures struct
       broke `runtime.throw` validation. Some runtime internals
       depend on the no-escape path for closures.
  Real fix likely needs a coordinated wasm3-specific OCOMPLIT
  walk path that always heap-allocates struct literals on wasm3,
  plus the SP-relative AGet encoder. Investigation cost: 2-3
  focused sessions.

- **Captures inside the closureCtx struct**. Today captures live
  in a linear-memory &struct{} fetched via CTXT; could move into
  the wasmgc closureCtx itself once the body's capture-access
  codegen is taught to read struct.get on the closure ref
  parameter. Removes the linear-memory allocation per closure
  but requires changing the body's calling convention to receive
  the closure ref as a parameter (currently CTXT global), plus a
  per-(closure func type) closureCtx struct shape (today it's
  one struct per signature; captures-in-struct needs one per
  closure's distinct capture set). Pure optimisation.

Stage G summary: the function-value pipeline is complete for
bare top-level functions (with singleton-optimised
materialisation) and for closures with captures via the linear-
memory captures-struct + wasmgc closureCtx wrapper. The two
remaining items above are scoped follow-ups; the infrastructure
is in place for each.

## Blocker for the wasip1 test harness — `go test` produces invalid wasm

`go test -c` for any package that pulls in the standard `testing`
machinery produces a `.test` wasm binary that fails to parse on
both wasm-tools and wasmtime:

    error: struct fields size is out of bounds (at offset 0x16f9)

Investigation: at the offending offset the type section declares a
struct with 65508 fields (1 self-ref + 3 i64 + 65504 i32). The
field count itself is correct — there really are 65508 fields in
the encoded bytes — but wasmparser (used by both engines) caps
struct fields at ~10000.

Root cause: `typeCollector.lowerFields` in
`cmd/compile/internal/wasm3/wasmtype.go` lowers a Go `TARRAY` by
unrolling `[N]elem` into N×len(elem) struct fields. A runtime
type with a large fixed-size array field — e.g. some
testing-internal struct containing a `[65504]int32` buffer —
produces a struct that exceeds the engine limit. (The 65504
exactly matches what'd appear if a `[16376]struct{a,b,c,d int32}`
were flattened, or a similarly-sized i32 buffer.)

Why the naive fix doesn't land: short-circuiting the unrolling in
`lowerFields` (return a single `(ref null any)` once the count
exceeds a threshold) compiles and gets the type section under the
limit, but immediately surfaces a different validation error —
"type mismatch: expected i64, found (ref \$type)" — because the
SSA-side struct-arg/struct-result code still flows the array as a
sequence of i32 values per field. The wasm signature now says one
ref, the call site pushes many i32s; calls mismatch.

Real fix: Stage D/E. Lower **every** Go array — small or large —
to `(ref (array T))` with element accesses via `array.get` /
`array.set`, not just the ones that would exceed the engine's
struct-field limit. The ref-typed-value plumbing (Stage C) is the
pre-req.

Resisting the temptation to special-case: a threshold-based
"unroll if small, ref if large" lowering would force every call
site that crosses the threshold (or every consumer that depends
on the size statically) to handle two layouts. Picking one
representation — array-typed for all Go arrays — keeps the SSA
backend and the runtime helpers honest and removes a class of
"works for [4]byte, breaks for [4097]byte" failure modes before
they have a chance to appear. The encoded cost is one extra ref
indirection per array access, which the wasm engine is built to
optimize through.

Until then, the wasip1 test harness can't exercise wasm3 binaries
that link `testing`. The M2 14-case regression and the
/tmp/wasm3-audit programs work because they don't pull in testing.

## Future optimization — drop trivial `//go:wasmexport` trampolines

Every `//go:wasmexport` function gets a wasm wrapper LSym
(`GenWasmExportWrapper` in `cmd/compile/internal/ssagen/abi.go`)
whose body bridges the host-facing wasmexport ABI to the wrapped
Go function's internal ABI. For wasm3 the bridge is
`assembleWasm3ExportWrapper` (`cmd/internal/obj/wasm/wasm3obj.go:925`):
widen each `WasmPtr` param with `i64.extend_i32_u` before the
call, narrow each `WasmPtr` result with `i32.wrap_i64` after.

When the function has no pointer-shaped params/results (no
`WasmPtr`/`WasmBool`/`WasmI32` narrowing in either direction) the
wrapper body collapses to a pure forwarder:

    local.get 0
    local.get 1
    ...
    call $main.<wrapped>
    end

— and the wrapper and wrapped have identical wasm signatures.

In the M2 regression `test.wasm` (10 wasmexports), 8 wrappers are
trivial forwarders. Each costs ~10–15 bytes (function entry + type
ref + 3–6 byte body); eliminating them is ~100 bytes off the
binary, scaling linearly with wasmexport count.

Two approaches:

- **Linker change.** Detect trivial-forwarder wrappers at
  `cmd/link/internal/wasm/asm3.go` setup, skip them from
  `m.funcs`, re-index everything after, and emit the export
  with the call target's funcidx. Local to the wasm3 linker
  path; requires care because funcidx shifts ripple through
  every R_CALL reloc.
- **Compiler change.** Skip emitting the wrapper LSym entirely
  when the bridge would be a no-op. Either rename the wrapped
  function to the export name (clashes with Go-level callsites)
  or thread a separate "export name" field through the loader/
  linker so `writeName(ctxt.Out, ldr.SymName(s))` can emit
  "add32" while the symbol stays "main.add32". Structurally
  cleaner but touches the loader symbol struct.

Deferred — not worth the complexity right now, but a clean
~1% binary win when M3 stabilizes.
