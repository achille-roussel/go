# wasm3 Stage F — interfaces

## Goal

Implement interface values, type assertions, and method dispatch on
GOARCH=wasm3 using WebAssembly 3.0 GC types. With Stage G's
`call_ref` machinery in place, Stage F is the next step toward
broad stdlib compatibility — most Go code beyond the simplest
hello-world programs uses interfaces (io.Reader, error, fmt
formatters, etc.).

## Today (baseline)

- The TINTER lowering in `wasm3/wasmtype.go:176-183` is a
  placeholder: `{type-descriptor, data}` both as opaque
  `(ref null $go.object)` references — no structure.
- The runtime functions `runtime.ifaceeq`, `runtime.efaceeq`,
  `runtime.convT*`, `runtime.assertE2I*`, `runtime.typeAssert`,
  `runtime.interfaceSwitch` are reachable from any non-trivial
  program (via the linker's keep-alive list). Even a program that
  uses interfaces only indirectly trips compilation of these
  functions.
- **Compilation fails** on `runtime.ifaceeq` because it reads the
  type's `Equal` field (a `func(unsafe.Pointer, unsafe.Pointer)
  bool`) via `i64.load offset=24` and stores the result into the
  per-value local declared `anyref` by `wasm3ValueType`'s TFUNC
  case. Same family of mismatch fails on the other runtime helpers
  too — Stage F has to fix all of them or shim them.

## Design constraints (from doc/wasm3-design.md §6, §10)

- Interface header is `(struct (ref $go.type) (ref $go.object))` —
  two wasmgc refs, mirroring Go's `(typedesc, data)` representation.
- Method dispatch goes through the itab, which becomes a struct of
  funcrefs (one per interface method). Stage G's
  `wasm3RegisterFuncSig` already returns the per-signature funcref
  type — the itab struct fields use those types.
- Type assertion `x.(T)` lowers to `ref.test`/`ref.cast` against
  the wasmgc subtype that represents `T`.
- The `$go.type` descriptor is a wasmgc struct itself (not a
  linear-memory `*_type`). Method `Equal` etc. become typed
  funcref fields.

## The key tension

The Go runtime today represents `_type`, `itab`, `interfacetype`,
etc. as linear-memory structs that the compiler and runtime both
read field-by-field via byte offsets. These structs are
referenced by every runtime helper that operates on interfaces.

Migrating them to wasmgc structs (per the design doc) is
correct but invasive — every load needs to become `struct.get`,
every offset needs to become a field index, and the runtime
package's hand-written assembly / pointer-arithmetic code paths
need parallel `_wasm3.go` rewrites.

## Two viable approaches

**(A) Full wasmgc itab/type, runtime shim per helper.** Migrate
`_type` and `itab` to wasmgc structs, write `_wasm3.go` shadows of
every runtime iface helper (ifaceeq, efaceeq, convT*, assertE2I*,
typeAssert, interfaceSwitch) that operate on wasmgc structs via
`struct.get`/`ref.cast`. Correct, clean, large diff (~10 helpers
+ type collector for `_type`, `itab`, `interfacetype` + ABI
plumbing).

**(B) Keep itab/type as linear-memory, fix the load path.** Add
SSA op support for loading a function-pointer field as a wasmgc
ref via a global function table that maps linear-memory function
addresses to funcrefs. Smaller diff but introduces a table
lookup per indirect call.

**(C) Hybrid: linear-memory itab, but the method-funcref field is
materialised once at itab construction via a wasm3 helper that
stores the funcref into a separate wasmgc array indexed by the
itab's linear-memory address.** Effectively a per-itab side-table.
Avoids redoing the type system but adds runtime complexity.

We choose **(A)**. It aligns with the design-doc target (§6.3
interface = two refs, §10 method-dispatch via funcref struct),
and the wasm3 runtime fork already has the convention of
`*_wasm3.go` shadows for subsystems that need full rewrites. The
larger diff is one-time; (B) and (C) carry permanent overhead.

## Pieces

### Piece 1: type-section model for `_type` / `itab` / `interfacetype`

In `wasm3/wasmtype.go`, add typeCollector entries for these
runtime structs (or their wasm3-specific counterparts in
`runtime/type_wasm3.go`). They live in the prelude or are
registered eagerly.

- `$go.type` = `(struct
    (field i64 size)
    (field i64 ptrdata)
    (field i32 hash)
    (field i8 tflag)
    (field i8 align)
    (field i8 fieldAlign)
    (field i8 kind)
    (field (ref $eqfunc) equal)
    (field ...))`
- `$go.itab` = `(struct
    (field (ref $interfacetype) inter)
    (field (ref $go.type) type)
    (field i32 hash)
    (field i8 fun_count)
    (field (ref (array (ref $funcref))) fun))`
  — or fixed-size if the interface arity is known.
- `$go.interfacetype` = `(sub $go.type (struct ...))`

Sizing the fixed prelude vs lazy-registration: probably
prelude-extend, since these are referenced from the SSA backend
for every interface op.

### Piece 2: TINTER lowering points to the typed refs

Update `wasm3/wasmtype.go:176-183` TINTER case:
```go
return []wasmgc.Field{
    {Storage: wasmgc.RefStorage(typeIdx, true), Mutable: true},  // type descriptor
    {Storage: wasmgc.RefStorage(wasmgc.TypeGoObject, true), Mutable: true},  // data
}
```

Where `typeIdx` is the per-package index of `$go.type` (or the
interface-specific subtype). The data field can stay as
`$go.object` since interface values can hold any concrete type.

### Piece 3: rewrite runtime iface helpers as `_wasm3.go` shadows

For each of:
- `runtime.ifaceeq`
- `runtime.efaceeq`
- `runtime.convT`, `convTnoptr`, `convT16/32/64/string/slice`
- `runtime.assertE2I`, `assertE2I2`
- `runtime.typeAssert`, `interfaceSwitch`
- `runtime.panicdottypeI`, `panicdottypeE`, `panicnildottype`

Write a `runtime/<helper>_wasm3.go` with `//go:build wasm3` that
operates on wasmgc struct refs via builtin compiler intrinsics
(struct.get, ref.cast, ref.test). Each helper retires its
linear-memory equivalent for wasm3 only.

### Piece 4: SSA ops for itab dispatch + type assertion

- `OpWasm3LoadInterfaceType` — `struct.get $iface 0` to extract
  the type descriptor.
- `OpWasm3LoadInterfaceData` — `struct.get $iface 1`.
- `OpWasm3LoadItabFunc` — given an itab ref and method index,
  `struct.get $itab.fun <idx>` to extract a funcref.
- `OpWasm3RefTest` / `OpWasm3RefCast` already exist (Stage E) —
  used for type assertions.

Lower OCALLINTER (interface method call) to:
```
ref.cast (ref $iface_T) iface_value
struct.get 0 -> itab
struct.get $itab.fun <method-idx> -> funcref
... push args ...
struct.get 1 (data ptr) -> ref $go.object
call_ref $funcType
```

### Piece 5: compiler emission of itab structs

The compiler today emits itabs as `*itab` in static data via
`reflectdata.WriteITabs`. For wasm3, those need to be emitted as
wasmgc structs with funcref fields. Either via a const-expr
global initialiser (preferred, like the closure singleton path),
or via runtime construction at module-load.

## Sequencing

1. **Piece 3 first**, in isolation, by stubbing the helpers with
   `unreachable`-trapping bodies that wasm-validate. This
   unblocks compilation of any program that links to them.
2. **Piece 1+2 together**: type collector and TINTER lowering.
3. **Piece 4**: SSA ops + OCALLINTER lowering.
4. **Piece 5**: itab emission as wasmgc structs.
5. **Piece 3 properly**: rewrite the helpers to use the new
   wasmgc itab/type representation.

Step 1 lets us land an iface-using program that compiles, even
if interface methods aren't dispatched correctly yet. Each
subsequent step extends functionality.

## Open questions

- **Static itabs vs dynamic.** Today the compiler synthesises an
  itab per (interface, concrete type) pair. For wasm3, can these
  be emitted as wasm globals (const-expr `struct.new`) like the
  closure singletons? Or do they need runtime construction?
- **Interface arity.** The itab's `fun` array length is determined
  at compile time (one slot per interface method). Can we use a
  fixed-arity wasmgc struct per interface type, or a flexible
  array? Both possible; flexible-array is simpler.
- **`_type.Equal` field.** Currently a `func(*any, *any) bool`.
  With Stage G's anyref-typed function values, it becomes a
  funcref-typed wasmgc field. Helpers that call it
  (`ifaceeq`, `efaceeq`, equality builtins) need to use
  `call_ref` rather than indirect-via-linear-memory-pointer.
- **`reflect` interaction.** Lots of `reflect` machinery probes
  `_type` fields. Some accesses are read-only and can stay on
  linear memory for now (a parallel `*_type` is generated for
  reflect alongside the wasmgc `$go.type`). This is M6 work but
  has dependencies here.

## Verification ladder

1. `/tmp/wasm3-iface` (simple interface call) — ✅ landed
   (923a31faf0). `bump(c Counter) int` returns 101, 102. Both
   the int-returning and string-returning (`Sound() string`)
   forms run end-to-end.
2. Method dispatch through interface to a **value-receiver
   method** — ✅ verified with `/tmp/wasm3-ifaceval`:
   `Square{side:5}.Area() = 25`, `Rect{w:3,h:4}.Area() = 12`.
3. Comma-ok **type assertion** `x, ok := i.(*T)` — ✅ verified
   with `/tmp/wasm3-ifaceassert`: `describe(*Dog{name:"rex"})`
   prints "dog: rex"; `describe(*Cat{})` prints "other: meow".
4. Interface switch `switch x := i.(type)`.
5. Empty interface `interface{}` / `any` round-trip.
6. `error` interface (the most common interface in stdlib).
7. `io.Reader` / `io.Writer` (used by `print` machinery).

## Status (2026-05-18)

Steps 1–3 of the ladder land via the **call_indirect-based
dispatch path**:

- Linker emits a funcref table sized to `funcValueOffset + numFns`,
  populated via an active element segment whose offset matches
  Go's wasm symbol-value encoding. The codeptr loaded from an
  itab + `>> 16` lands at the right table entry with zero
  arithmetic.
- Obj-encoder handles `ACallIndirect`.
- SSA codegen at OpWasm3LoweredInterCall pushes `i64.const 16;
  i64.shr_u; i32.wrap; call_indirect <typeidx> <0>`.
- Runtime iface equality helpers (`interequal`, `nilinterequal`,
  `efaceeq`, `ifaceeq`) are stubbed via `alg_iface_wasm3.go` so
  programs that include them in their reachable set compile;
  they trap if actually called (interface equality isn't on the
  current ladder).

This puts wasm3 on the simpler-of-the-two-approaches: the itab
stays a linear-memory struct, codeptrs stay i64 values, and the
funcref table bridges between them at call sites. The "full
wasmgc itab" Approach A from the plan above is deferred — it
would buy size + GC tracking but is significantly more invasive,
and call_indirect is canonical wasm for this use case.

**Step 4 (type switch)** ✅ landed (0a87276b02). Both
`/tmp/wasm3-typeswitch` (inline `&Dog{id:1}` / `&Cat{id:2}` /
`&Cow{id:3}`) and `/tmp/wasm3-typeswitch2` (heap-allocating
helper functions) print `canine` / `feline` / `other`. The
walkNew fix routes every EscNone ONEW through
runtime.newobject on wasm3 (avoiding the `Get $autotmp(SP)`
stackTempAddr that has no obj-encoder representation); the
runtime.throw split into throwOnSystemstack lets that helper
print directly on wasm3 instead of capturing the string in a
systemstack closure that the captures-in-struct path can't
i64.store into a stack temp.

**Step 7 (`error` interface)** ✅ landed via the same fix:
`/tmp/wasm3-error` (`type myError struct{ msg string }` with
`func (e *myError) Error() string` returning `&myError{...}`
from `check(failed bool) error`) prints `"err: boom\n"` /
`"no err\n"` correctly. error is the most-used interface in
stdlib, so unblocking it is a much bigger reach than its line
on the ladder suggests.

**Step 5 (empty interface round-trip)** exposes a chain of
runtime functions whose bodies fail wasm validation:
`runtime.gwrite` → `runtime.writeErrData` (passes a slice's
wasmgc-typed backing where `*byte` is expected),
`runtime.memclrNoHeapPointersChunked`, and likely more
downstream. The root cause for each is wasm3's
slice/string-in-arg ABI inconsistency: a `[]byte` parameter
lowers to `(anyref backing, i64 len, i64 cap)` but the
helpers' linear-memory callees (`write1(*byte, int32)`, etc.)
expect `(i64 ptr, i32 len)`. Bridging requires copying the
wasmgc array's contents into linear memory — out of scope for
Stage F.

For Stage F's part, the easier wins ARE landed:

- The 761bfdd862 fix to `wasm3ValueType` correctly classifies
  a TFUNC value loaded from linear memory as i64 (not anyref),
  matching the i64.load that produces it. This was the original
  blocker for `runtime.ifaceeq` and the same family of `i64.load
  → anyref-local` errors in other runtime helpers.
- `runtime.gwrite` is a documented no-op stub
  (gwrite_wasm3.go) — the few programs that reach it via
  panic/print paths drop their writes silently. The proper
  bridge belongs in the wasm3 slice-marshalling workstream.

Step 6+ (`error`, `io.Reader`) blocked by the same
slice-marshalling and defer-closure-storage families.

**Step 5 (empty interface boxing) follow-up** ✅ landed by
splitting `mallocgc` into `mallocgc_default.go` (the standard
per-P cache / GC-assisted body, `//go:build !wasm3`) and
`mallocgc_wasm3.go` (a one-line redirect to the bump-heap
`newobject`). Every `convT*` boxing path funnels through
`mallocgc`; on the standard implementation those reach
`mallocgcTiny` / `mallocgcSmallScanHeader` / `fixalloc.alloc`,
each of which has an `f.first(f.arg, v)`-style indirect call or
linear-memory pointer arithmetic that the wasm3 backend lowers
to validation-failing wasm. The shim keeps the entire mcache /
mheap / fixalloc subgraph dead-code on wasm3 — the linker DCE
drops them and only the bump allocator survives. `/tmp/wasm3-
anyiface` (`wrap(42)` / `wrap(7)` / `.(int)`) now prints `ok`.

**`runtime.gwrite` slice-in-arg bridge** ✅ landed. The wasm3
gwrite copies bytes one at a time from the wasmgc-backed `[]byte`
into a 4 KiB linear-memory scratch buffer (`gwrite3Scratch`) and
hands the resulting `*byte` to WASI `fd_write` via `write1`
directly — bypassing `runtime.writeErrData` (which reads
`g.m.dying` off a `*g` shape M2 hasn't populated) and
`runtime.write` (whose `if overrideWrite != nil` indirect-call
probe stores a func-pointer load into an anyref local, failing
validation). One sharp edge surfaced: the wasm3 obj backend
miscompiles a trailing void-context call whose callee returns a
value — it emits `local.set 0` on the call's i32 result, which
on a func with anyref param 0 fails validation with "expected
anyref, found i64". Worked around by stashing `write1`'s return
in a package-global (`gwrite3LastN`) instead of dropping it
inline; a proper backend fix belongs in a separate cycle.
`/tmp/wasm3-gwrite` (`print("ptr: ", &g, "\n")` exercising
printhex → gwrite) now prints the address and `ok`.
