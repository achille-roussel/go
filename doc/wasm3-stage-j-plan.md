# wasm3 Stage J — wasmgc memory model end to end

> **SUPERSEDED by M3.5 (`.claude/plans/retire-go-runtime-wat-static-link.md`).**
> The Stage J plan calls for a dynamically-linked `go_runtime.wat` module
> hosting the three linear-memory bridge primitives. M3.5 took a
> different fork: the three primitives became compiler intrinsics that
> emit inline byte-copy loops over wasm `global 0` as the bump pointer,
> with `memory.grow` for on-demand growth. The `go_runtime.wat` module
> and its `//go:wasmimport` bridges (`stringEqual` / `strcmp` /
> `stringHash` / `stringConcat2`) are retired; equivalents live in plain
> Go using `s[i]` (which lowers via `OpWasm3StringByte` to
> `array.get_u $go.bytes` directly). Single-binary output, no dynamic
> linking, no host-side wat plumbing.
>
> This document is kept for the design rationale on the WasmGC-only
> memory model and the bridge primitives' shape (still valid), but the
> mechanism section below describing wat-module hosting is obsolete.


## Goal

Replace the residual linear-memory model in the wasm3 runtime with a
wasmgc-native one. After Stage J, no Go-level value lives in linear
memory: strings, slices, structs, arrays, and the heap behind
`new`/`make` are all wasmgc references. Linear memory survives only as
the wasip1 host-ABI scratch region — the few buffers WASI itself reads
and writes through (iovecs for `fd_write`, the argv block for
`args_get`, etc.) materialise on demand at the syscall boundary, and
nothing else touches it.

This is the cutover that retires the per-callsite shim regime the M3
bring-up has been accumulating. Each linear-memory shim added since M2
(memequal_wasm3, string_concat_wasm3, runtime_faststr_wasm3, the bump
heap, …) is throwaway against the wasmgc target; the more we add, the
more we revert. Stage J is the milestone where we stop paying that
interest, do the foundational rewrite, and **delete the shims** as part
of the cutover.

## Why now

The pattern across the last two weeks of M3 bring-up is identical
every time:

1. Pick a test (slice literal, string concat, map[string]int).
2. Watch it fail wasm validation at a place where an anyref-typed
   value meets an i64-typed value.
3. Bridge the mismatch with a wasm3-only Go shim that picks one side
   of the world (almost always linear memory, because that's what the
   runtime was written for).
4. The shim works. The next test fails at the next anyref/i64
   boundary one frame deeper. Repeat.

Each shim is *correct in isolation* and *wrong in aggregate* — it
delays the cutover by making the project's surface area look
healthier than it is. The shims accumulate, the runtime's two memory
worlds calcify, and the eventual wasmgc cutover gets bigger every
week because there's more linear-memory code referencing
linear-memory state.

The fast-forward is to admit that the wasmgc cutover is the *only*
real M3 work, and to stop doing anything else until it lands.

## Today (baseline at end of Stage I)

What lives in **linear memory** today on wasm3:

- **Strings.** `TSTRING` lowers to `(i64 data, i64 len)`. The data
  pointer is a linear-memory byte address; substring/slice-to-string
  paths assume linear-memory arithmetic.
- **The Go heap.** `mallocgc` is shimmed to `wasm3BumpAlloc`, a bump
  allocator carving from `var wasm3Heap [1 << 20]byte`. Every
  `mallocgc(n, nil, false)` call site (rawstring, growslice, convT,
  newobject, …) returns a linear-memory address.
- **Map storage.** The just-landed `runtime_faststr_wasm3.go` keeps
  keys and values in two `wasm3BumpAlloc`-allocated linear-memory
  buffers, scanned with `unsafe.Add` pointer arithmetic.
- **The wasip1 syscall buffers.** `fd_write`'s iovec, `args_get`'s
  argv/argv_buf blocks, the read/write data regions for stdio. The
  WASI ABI requires linear-memory addresses; this is the only
  legitimate use after Stage J.
- **`unsafe.Pointer` arithmetic** wherever the runtime does
  `unsafe.Add(p, offset)` over a heap-allocated buffer — string ops,
  slice ops, map element access. All assume linear-memory addressing.

What already lives in **wasmgc**:

- **Slice backing arrays.** `[]T` headers carry an anyref `.array`
  field per `flatPrimitiveFields(TSLICE)`; `OpWasm3MakeSlice` emits
  `array.new_default $arr_T_elem`.
- **Go arrays.** `[N]T` lowers to a single `(ref (array T))` per
  `lowerFields(TARRAY)`. The element backing is wasmgc, with the
  per-element stride encoded in the array type.
- **Closure contexts.** Per-package `closureCtx` structs are wasmgc
  via the Stage G captures-in-struct path.
- **The function table.** `ref.func` references and the
  call_ref/call_indirect dispatch path.

The fault line runs straight through `unsafe.Pointer`: a Go pointer
in wasm3 code is *either* an i64 linear-memory address *or* a
wasmgc ref, and the distinction is per-construction. Every helper
that takes `unsafe.Pointer` has to pick which world it operates in,
and every caller has to agree.

## The cutover

After Stage J:

- `TSTRING` lowers to `(struct (ref (array i8)) i32 i32)` — a
  wasmgc-backed byte array plus offset/length. Substrings share
  backing arrays by adjusting offset/length; `s[i:j]` is a
  `struct.new` allocating a new header that points at the same
  underlying `(array i8)`.
- `[]byte` and `string` share the same backing-array type
  `(array i8)`. `string([]byte)` and `[]byte(string)` become
  `struct.new` of the appropriate header type pointing at the same
  ref (or a fresh `array.copy` when mutation isolation requires it —
  per the existing Go semantics).
- `mallocgc` is **deleted** from the wasm3 build. Every
  former-mallocgc call site lowers to a typed wasmgc allocation
  intrinsic: `array.new` for slice/string backings, `struct.new`
  for objects (where the per-type struct is registered via the
  Stage E type collector). The bump heap (`var wasm3Heap`) is
  deleted.
- `unsafe.Pointer` in wasm3 runtime code becomes restricted. Its
  only legitimate use is interior pointers into wasmgc
  arrays/structs (the fat-pointer primitive; doc/wasm3-design.md §7
  and doc/wasm3-fat-pointers-design.md). The wasip1 boundary no
  longer uses pointers at all — it serializes through linear memory
  via the `runtime/wasm` intrinsics (see Piece 5).
- The runtime's map storage is a wasmgc array of key/value pairs,
  not a linear-memory bump-allocated buffer. The faststr
  linear-scan shim is **deleted**.

The cutover scope explicitly excludes:

- The wasip1 syscall trampolines themselves. `fd_write`,
  `args_get`, etc. continue to take linear-memory addresses
  because the WASI ABI requires them. A wasmgc → linear copy
  helper materialises iovec/buffer regions at the syscall
  boundary; everything else stays wasmgc.
- The `*_wasm.s` and `*_wasm3.s` files that we already gutted
  (memequal, indexbyte, equal); they stay empty stubs.
- The ABI-bridging wrapper skip and LinksymABI collapse
  (commit 6bd6e60a7b). Those are independent of the memory
  model.

## Pieces

The cutover is structured as five pieces. They land in order: 1, 2,
3 are sequential (each unblocks the next); 4 and 5 fan out from 3.

### Piece 1: string lowering to wasmgc

The keystone. Until strings are wasmgc, the runtime is forced into
a hybrid model and we can't delete any of the bridging code.

- `cmd/compile/internal/wasm3/wasmtype.go`: change
  `lowerFields(TSTRING)` from the current
  `(ref (array i8), i32, i32)` field shape (already present for
  the wasmgc embedded-struct lowering) to the universally-used
  shape.
- `cmd/compile/internal/wasm3/wasmabi.go`:
  `flatPrimitiveFields(TSTRING)` switches from `(i64, i64)` to
  `(WasmAnyref, i32, i32)`. The wasm signature emitter, call-site
  argument decomposer, and selectN classifier all follow.
- SSA: `OpStringMake` lowers to `struct.new $string` on wasm3.
  `OpStringPtr` becomes `struct.get $string 0` (returns the array
  ref) plus the offset, packaged as the interior-pointer fat
  pointer (`$go.box.scalar`) — this is what `unsafe.StringData(s)`
  returns to user code; see Piece 3 for why interior pointers are
  the canonical replacement for `unsafe.Pointer` arithmetic.
  `OpStringLen` becomes `struct.get $string 2` (the length;
  field 1 is the offset). String comparison and indexing rewrite
  to `array.get_u`/`array.compare`-based ops.
- String-literal data section: the existing `.rodata` strings
  become per-package wasmgc data segments via the
  passive-segment+`array.new_data` pattern.
- `runtime/string.go` rawstring/concatstrings split: the wasm3
  shadow becomes wasmgc-native (allocate `(array i8)` of the
  right size, `array.copy` into it). The current linear-memory
  shim (`string_concat_wasm3.go`) is **deleted** in the same
  change.

Validation: an empty `main` linking the runtime's string
constants ("ok\n", panic messages, type names) must round-trip
through `print` without invoking any linear-memory string op.
`wasm-tools validate` must show no `i64.store`/`i64.load` against
string data.

### Piece 2: allocator cutover

Once strings are wasmgc, the next-largest user of the bump heap
is the slice-of-string and map paths. We remove `mallocgc` from
the wasm3 build and replace every caller with a typed wasmgc
allocation.

- `runtime/mallocgc_wasm3.go`: **deleted**.
- `runtime/newobject_wasm3.go`: **deleted** (the bump heap goes
  with it).
- `runtime/makeslice_wasm3.go`: rewritten as a thin Go function
  whose body the SSA backend intrinsifies — `make([]T, n, c)`
  lowers directly to `array.new_default $arr_T_elem` (already
  done for `OpWasm3MakeSlice`; the runtime call goes away). For
  non-intrinsifiable variants (the typed `makeslice` reflect
  path), a wasm3 helper allocates the wasmgc array and returns a
  ref.
- `runtime.newobject(*_type) unsafe.Pointer` becomes
  `runtime.newobject_wasm3(typeIdx int) wasmgcRef` — the
  compiler-side intrinsifies it to `struct.new_default $T` when
  the type is statically known, falling back to a per-type
  dispatch table otherwise.
- `runtime.convT*` helpers (interface boxing) rewrite to
  `struct.new` of a per-type box struct (a Stage F type-collector
  entry already exists for boxed scalars; extend to composites).
- `growslice`: rewrites to `array.new_default` of the new size +
  `array.copy` from the old backing. The existing growslice in
  `runtime/slice.go` is gated `!wasm3` and a wasm3-only shadow
  takes over.

Validation: `make(map[string]int)` still works (Piece 4 keeps
maps green during the cutover by replacing the linear-memory map
storage at the same time). The `wasm3Heap` symbol is no longer
referenced anywhere in the linker's reachable set.

### Piece 3: `unsafe.Pointer` discipline

`unsafe.Pointer` is the API surface through which the linear-memory
world leaks into the rest of the runtime. After Stage J, the runtime
uses it for one narrow case only.

- **Interior pointers into wasmgc arrays/structs** — the fat-pointer
  primitive (`doc/wasm3-design.md` §7,
  `doc/wasm3-fat-pointers-design.md`): taking `&buf[i]` when `buf` is
  `[]byte` materialises an `(arrayref, index)` fat pointer. No raw
  linear-memory arithmetic is permitted — operations go through array
  indexing on the underlying ref. Every Go pointer the compiler sees
  is a gc ref; there is no second (linear) pointer representation.
- **The wasip1 boundary uses no pointers at all.** It serializes
  through linear memory via the `runtime/wasm` intrinsics (Piece 5),
  so there is no `LinearPtr` type and no `unsafe.Pointer`-to-linear
  conversion to police. Any `unsafe.Pointer` arithmetic on wasm3
  outside the interior-pointer case is a build error enforced by a
  vet check.

#### `unsafe.StringData` / `unsafe.SliceData` are the canonical API

WasmGC has no pointer arithmetic — `(ref (array i8))` is opaque and
the only way to address element *i* is `array.get` / `array.set` /
`array.copy` against the ref+index pair. So every runtime helper
that today says `unsafe.Pointer(&local) + offset` has to be rewritten
to take a typed reference plus an index. The two stdlib functions
that produce exactly that shape are `unsafe.StringData(s string)
*byte` and `unsafe.SliceData(s []T) *T`. After Stage J they become
the **only** supported way for runtime code to obtain a byte-level
handle to string / slice storage; every shim that today reaches into
a `string` or `[]byte` via `unsafe.Pointer(&local)` is rewritten to
go through them.

Concretely:

- `unsafe.StringData(s)` lowers (post-Stage-J string shape) to
  `struct.get $string 0` followed by an `(arrayref, offset)` interior
  pointer construction. The result is typed `*byte` to the Go user
  but represented as the same `$go.box.scalar` fat-pointer struct
  as `&buf[i]`. Element access goes through the boxed ref's array
  ops — `unsafe.Pointer(unsafe.StringData(s)) + i` is a compile-time
  error on wasm3 (pointer arithmetic is forbidden); the correct
  rewrite uses Go subscripting (`s[i]`) which the compiler lowers
  to `array.get_u`.
- `unsafe.SliceData(s)` lowers to `struct.get $slice 0` plus the
  same interior-pointer construction. Same rules.
- `unsafe.Slice(p *byte, n int)` and `unsafe.String(p *byte, n int)`
  reverse the operation: given the interior pointer (`(arrayref,
  offset)`) and a length, construct a new slice / string header
  pointing at the same backing array, with the offset/len adjusted.
  No backing copy.

Adoption pattern in runtime/internal code. Wherever a wasm3-affected
helper currently has

    p := unsafe.Pointer(&local)
    ... arithmetic on p ...

the Stage J rewrite is

    p := unsafe.StringData(local)   // or unsafe.SliceData(local)
    ... typed access through p (no arithmetic) ...

and the helper is then portable to wasm3 *without* a separate shim.
The bring-up already validated this pattern in commit ccaf2dcbf5
(`internal/runtime/maps: add StrHashByValue, avoid &local at faststr
call sites`); Stage J generalises it to every remaining helper.

Stdlib `unsafe.StringData` / `unsafe.SliceData` implementations are
compiler intrinsics (`OpStringPtr` / `OpSlicePtr` today); the wasm3
backend just needs to emit the wasmgc interior-pointer construction
instead of the i64 data-field load.

#### Per-helper rewrites

Helpers that took `unsafe.Pointer` and did arithmetic on it
(`memequal`, `memmove`, `memclr`, the map helpers) become typed:
`runtime.memequalArray(a, b (ref (array i8)), n i32) bool`,
`runtime.memmoveArray(...)`. The byte-by-byte fallbacks in the
current linear-memory shims (`memequal_wasm3.go`, etc.) are
**deleted** and replaced with `array.copy` / element-wise
comparison. Callers that today pass `unsafe.Pointer(&x)` switch
to `unsafe.SliceData` / `unsafe.StringData` as described above,
then pass the resulting interior pointer to the typed helper.

### Piece 4: map storage on wasmgc

Replace `runtime_faststr_wasm3.go`'s linear-memory bump buffers
with wasmgc-backed storage. The just-landed shim is **deleted**;
its replacement is a wasm3-only `internal/runtime/maps/map_wasm3.go`
that lays the map out as:

- `keys (ref (array $string))` — a wasmgc array of string-header
  refs, growing via `array.new` + `array.copy` at full.
- `values (ref (array i64))` for scalar value types, or
  `(ref (array $go.object))` for ref types — same growth pattern.
  For non-uniform value types (i.e. structs, multi-i64
  composites), the array element type is the value's wasmgc
  collector struct.
- `count`, `cap`: i32 fields on a wrapping struct.

The linear-scan logic itself is unchanged from the current shim;
only the storage representation moves. Other map key classes
(fast32, fast64, generic) get the same treatment in turn — each
is one small file.

### Piece 5: the wasip1 boundary

`fd_write`, `args_get`, `args_sizes_get`, `clock_time_get`,
`proc_exit`, the handful of WASI calls the runtime makes — these
are the only legitimate linear-memory consumers post-cutover.

**Model: linear memory is a serialization layer, opaque to the
compiler.** This supersedes the earlier `LinearPtr`-typed-parameter
sketch. The problem with making any pointer type mean "linear" — a
dedicated `LinearPtr`, or "a `structs.HostLayout` value lives in
linear memory" — is that such values flow everywhere (locals,
fields of gc structs, slice elements), so the compiler would have to
insert gc↔linear conversions wherever they flow, and for anything
with interior pointers (a linked list, a tree) there is no
well-defined automatic translation at all. The compiler must keep a
**single** pointer representation (gc ref) and never manufacture a
linear pointer.

So linear memory is reached only through three intrinsics, exported
from package **`runtime/wasm`** (the FFI-boundary analog of
`runtime/cgo` — runtime-owned state, compiler-intrinsified, usable by
third-party `//go:wasmimport`/`//go:wasmexport` authors):

    // copy out of linear memory into a fresh gc []byte
    func ReadLinearMemory(mem int32, off, len uint32) []byte
    // bump-allocate, copy a gc []byte into linear memory, return offset
    func WriteLinearMemory(mem int32, data []byte) uint32
    // rewind the bump allocator to off (frees everything above)
    func ResetLinearMemory(mem int32, off uint32)

These are the *only* code that knows linear memory exists, and they
live in a **separate hand-written wasm module — the "go runtime"
module — dynamically linked** to the compiled Go program. This was
chosen over two alternatives:

  - *compiler intrinsics*: rejected — an intrinsic means the compiler
    keeps SSA ops and codegen for linear load/store, exactly the
    dependency we are deleting.
  - *a `.s` file + a new wasm3 assembler rung*: `encodeWasm3Body`
    (`cmd/internal/obj/wasm/wasm3obj.go`) bails to the `unreachable`
    stub on any linear-memory access, so hand-written `.s` would need
    a from-scratch linear-memory body encoder. Large, and it puts
    linear-memory knowledge back into the toolchain.

The winning insight: **wat is already the assembler for wasm**, and a
wasm3 program is itself a WasmGC module, so the primitives can be
written in wat, validated with `wasm-tools`, and linked as an ordinary
wasm module. The runtime module declares the linear memory and the
bump-pointer global and exports the three functions; the program
imports them. Because the program never touches that memory directly
(it only passes WasmGC `[]byte`s and receives offsets / `[]byte`s
back), the linear memory is fully encapsulated in the runtime module —
the compiler emits WasmGC only, end to end.

The functions reach the program through the **existing import
mechanism**: `//go:wasmimport go_runtime WriteLinearMemory` declares an
import from module `go_runtime`, exactly as `//go:wasmimport
wasi_snapshot_preview1 fd_write` declares one from WASI. The embedder
wires it at instantiation — e.g. `wasmtime run --preload
go_runtime=runtime.wasm program.wasm`, or the WebAssembly JS API on
Node. So `runtime/wasm` collapses to three `//go:wasmimport go_runtime`
signatures; there is no Go body and no `.s`.

**Enabler — ref-typed `//go:wasmimport` (the successor "ref interop"
item, pulled forward).** To hand `WriteLinearMemory` a `[]byte`, the
import signature must carry a WasmGC slice ref (a single
`(ref $go.slice.byte)` once slices are boxed — see
**doc/wasm3-slice-boxing.md**, the prerequisite rep change), not an i32
offset. That is the module-boundary ref interop recorded under
*Successor milestones*; this approach makes it the foundation. It
reuses the wasm3 backend's existing ref-passing call machinery (an
import is a call whose body is elsewhere) rather than a new
linear-memory encoder. `ReadLinearMemory` additionally needs a ref
*result* (`[]byte`), so sequence it after `WriteLinearMemory`
(ref param, scalar result).

**De-risk (done, 2026-05-20).** A pure-wat experiment confirmed the
load-bearing assumption: `wasmtime --preload` links two modules that
*independently* declare `(array (mut i8))` — the `$go.bytes` shape — and
the cross-module `(ref $go.bytes)` import matches structurally (the
prelude emits each type as a singleton rec group, so structural
equivalence holds). `array.get_u` on the passed ref, `i32.store8` into
the runtime module's memory, and the bump global all work; a negative
control traps as expected.

**wasmimport/wasmexport signatures on wasm3 use only primitive
numeric types** (`int32/int64/uint32/uint64/float32/float64`). A WASI
"pointer" is just a `uint32` linear-memory offset — which is the
*faithful* representation, since the host has never seen a Go
pointer; `unsafe.Pointer` only ever worked on plain wasm because Go
pointers there already were i32 offsets. Per-call marshalling is
ordinary Go in `os_wasip1_wasm3.go`, e.g. `fd_write`:

    //go:wasmimport wasi_snapshot_preview1 fd_write
    func fd_write(fd, iovsOff int32, iovsLen, nwrittenOff int32) int32

    func write1(fd int32, b []byte) int32 {
        dataOff := wasm.WriteLinearMemory(mem, b)
        var iov [8]byte                       // iovec built in gc memory
        le.PutUint32(iov[0:], dataOff)
        le.PutUint32(iov[4:], uint32(len(b)))
        iovOff := wasm.WriteLinearMemory(mem, iov[:])
        nwOff := wasm.WriteLinearMemory(mem, make([]byte, 4))
        errno := fd_write(fd, int32(iovOff), 1, int32(nwOff))
        nw := le.Uint32(wasm.ReadLinearMemory(mem, nwOff, 4))
        wasm.ResetLinearMemory(mem, dataOff)
        _ = errno
        return int32(nw)
    }

Everything except the three runtime-module primitives is plain Go on
gc `[]byte`. Two consequences fall out for free: the `uintptr32`/
`KeepAlive` dance (today in `os_wasip1.go`) disappears — no pointer
crosses, so the GC can't reclaim anything mid-call — and
`//go:noescape` becomes irrelevant on these signatures.

The arena is a bump allocator with strict per-call stack discipline
(`Write` advances, `Reset` rewinds). Fine for single-goroutine M2;
once goroutines land (M4) it needs a per-goroutine or guarded arena
— note it, defer it.

This is copy-based marshalling (like cgo or a syscall ABI), not
FlatBuffers' zero-copy: the two memory models are disjoint, so we
copy in and copy out. The deliberate limitation — **no shared live
views, no pointer graphs across the boundary** — is exactly what
makes the linked-list problem a non-problem: you serialize the flat
thing a given call needs, by hand, the way that call's ABI defines.

## Deprecation list

Files **deleted** by Stage J's cutover. Listed by commit to make
the rollback obvious if the cutover stalls:

| File | Added in | Reason |
|---|---|---|
| `runtime/memequal_wasm3.go` | a5528227c5 | Replaced by wasmgc `memequalArray` |
| `runtime/string_concat_wasm3.go` | cf47f33b9f | Replaced by wasmgc `array.copy` concat |
| `runtime/string_concat_default.go` | cf47f33b9f | Becomes the only `concatstrings` |
| `runtime/makeslice_wasm3.go` | (pre-Stage J) | Intrinsified at SSA, no shim needed |
| `runtime/newobject_wasm3.go` | (pre-Stage J) | Bump heap deleted |
| `runtime/mallocgc_wasm3.go` | (pre-Stage J) | Allocator deleted |
| `internal/runtime/maps/runtime_faststr_wasm3.go` | cfd70bf33d | Replaced by wasmgc map storage |
| `internal/runtime/maps/runtime_faststr.go` `!wasm3` tag | cfd70bf33d | Becomes the only `runtime_faststr` |
| `runtime/stubs_memequal_default.go` | a5528227c5 | Forward decl rejoins `stubs.go` |

Files that **stay** as `_wasm3` shadows after Stage J:

- The wasip1 trampolines (linear memory is unavoidable here).
- `rt0_wasip1_wasm3.s` (entry point).
- Anything in `runtime/` whose `_wasm3` shadow is about the *call
  convention* (typed-function ABI, no morestack) rather than the
  *memory model* — those changes survive the cutover.

## What stays unchanged

- The compiler's typed-function ABI work and the ABI wrapper skip
  / LinksymABI collapse (commit 6bd6e60a7b). Memory model and
  calling convention are orthogonal.
- The cross-package SelectN classifier (commit 7313b8911a). Still
  needed; wasmgc widens the set of anyref returns but doesn't
  remove them.
- The Stage G closure-singleton machinery, the Stage F
  call_ref/call_indirect dispatch, the Stage E type collector.
  All of these are wasmgc-first and Stage J extends them rather
  than supersedes them.
- The wasm3 obj backend's structured control flow, relooper
  plans, per-value locals. Memory-model independent.

## Verification

- `go build` of `cmd/...` and `std` packages succeeds for
  `GOOS=wasip1 GOARCH=wasm3`.
- The M3 test ladder (empty main, arithmetic, struct, pointer,
  slice, string, map[string]int, interface) runs end to end on
  wasmtime under `--wasm gc=y --wasm function-references=y`.
- `wasm-tools print` of the resulting binary shows zero
  references to a `wasm3Heap` data symbol, zero `i64.load`/
  `i64.store` against string-data offsets, and a linear-memory
  size dominated by the wasip1 scratch arena (kilobytes, not
  megabytes).
- Binary size measurement against the §1 audit of
  `doc/wasm3-design.md`: the post-Stage-J empty `main` should
  shrink by the size of the bump heap declaration plus the
  former mallocgc code paths (~20-50 KB ballpark, to be measured).

## Out of scope

- **String/byte conversion semantics under race detector.** Stage
  J leaves the `raceenabled` paths in the shared `string.go`
  unchanged; the wasm3 build doesn't enable the race detector
  yet, so this is invisible. A future stage will revisit if
  needed.
- **`reflect.StringHeader` / `reflect.SliceHeader` compatibility.**
  Already broken on wasm3 (they assume linear-memory layout);
  Stage J does not restore them. Documented as a casualty per
  `doc/wasm3-design.md` §1's restricted-subset compat target.
- **Goroutines (M4).** Stage J is single-goroutine. The
  scheduler cutover is its own milestone and depends on Stage J
  having retired the linear-memory g/m struct accesses that the
  current proc_wasm3.go stubs around.
- **Panic/recover (M5).** Same story — depends on Stage J for
  the panic-value payload representation.

## Risks

- **Compiler-intrinsic complexity.** `make([]T, n)` lowering to
  `array.new_default` requires per-call-site knowledge of `T`'s
  wasmgc array type index. Stage E's `wasm3RegisterArrayAux`
  machinery covers the slice path; Stage J extends it to the
  `newobject`/`growslice`/`convT*` call sites. Most of the work
  is plumbing rather than novel design.
- **String-literal data segments.** Existing wasm linker writes
  string-literal bytes into the linear-memory data section.
  Stage J needs the linker to instead emit a passive `(data)`
  segment plus `array.new_data` initialisation code on the
  function-entry path. This is doable (the wasm spec supports
  it) but is a new linker code path.
- **Reflect package.** `reflect.Value.Interface()` and friends
  cross the wasmgc/runtime boundary frequently. Stage J makes
  this concrete (interfaces hold `(ref $go.type, ref $go.object)`
  pairs; reflect operates on those). The casualties listed in
  M6 of `doc/wasm3-design.md` (`reflect.StructOf`/`FuncOf`/
  `ArrayOf`, `Value.Addr`/`Pointer`) become permanent for
  wasm3 — Stage J doesn't try to restore them.

## Successor milestones

After Stage J:

- **M4 (goroutines via stack-switching).** Now feasible because
  `g` and `m` are wasmgc structs rather than linear-memory ones.
- **M5 (panic/recover).** Now feasible because the panic payload
  representation matches the rest of the value model.
- **M6 (reflect).** Now feasible because type descriptors are
  wasmgc and `reflect.Value` can be a thin wasmgc wrapper.

- **wasmgc ref interop at the module boundary (future).** A step
  beyond the numeric-only serialization of Piece 5. Today Piece 5
  restricts `//go:wasmimport`/`//go:wasmexport` to primitive numeric
  params and serializes everything through linear memory. Once
  wasmgc-aware peer modules are a target, those boundaries should
  *also* be able to carry **wasmgc refs directly**: a Go pointer /
  slice / string at the boundary lowers to its wasmgc `(ref …)` and
  passes to/from the peer module with no linear-memory copy. This is
  a superset, not an exception:

    - The boundary's parameter *types* select the mechanism — numeric
      (i32/i64/f32/f64) → marshal through linear memory via the
      `runtime/wasm` intrinsics; ref-typed → pass the gc ref through.
    - It preserves the core invariant: the compiler still only ever
      sees gc pointers and never manufactures a linear pointer. Refs
      pass through; numbers serialize.
    - It stays compatible with the wasip1 numeric-only constraint —
      WASI hosts simply never use ref params, so they keep getting
      the serialization path.

  Net: full module-to-module wasmgc interop with high-level Go values
  (structs, slices, strings) as the currency, sharing the same
  compiler model as the numeric path.

The remaining *linear-memory* surface (wasip1 scratch arena) is
small enough that it doesn't grow as new functionality lands.

## Note

This plan supersedes the per-callsite bring-up strategy that M3
has been following since the M2 cutover. Once Stage J lands,
**no new linear-memory shim should be added** to the wasm3
build; bug reports against a wasm3 program should be triaged
against the wasmgc model and fixed there.
