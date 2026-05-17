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

**Stage B — `newobject` intrinsic + ref-typed SSA values**: NEXT.
The SSA op `Wasm3LoweredStructNew` needs to land alongside an SSA
intrinsic that replaces `runtime.newobject(typ)` with a direct
struct.new at the call site (no runtime call). The harder pre-req
is plumbing ref-typed values through the SSA register allocator: a
struct.new returns a `(ref $T)`, which can't be stored in an i64
register. Either a new ref register class or a redesign of wasm3's
register model is needed. Once that lands, the bump-allocator
`runtime.newobject` shim can retire.

## Stretch — interfaces + closures

`fmt.Println` (interface dispatch) + a higher-order `func` value
(closure) running end-to-end is the M3 stretch goal. The plan groups
these under M3 but they could slip to M3.5 / early M4 depending on
how the ref-typed-call rung lands.

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
