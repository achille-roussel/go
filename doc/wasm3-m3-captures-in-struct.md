# wasm3 captures-in-closureCtx — implementation plan

This converts the design sketch in `doc/wasm3-m3-notes.md` ("Captures
inside the closureCtx struct") into a concrete plan with file/line
references and resolved design decisions. Stage G's functional pipeline
is complete (bare top-level funcs, closures-with-captures, method
values, singleton-optimised materialisation); this is a pure
allocation-coalescing optimisation that collapses the current two-
allocation closure-with-captures evaluation into one.

## Today (Stage G baseline)

For `f := func(x int) int { return x + captured }`:

1. **walkClosure** (`src/cmd/compile/internal/walk/closure.go:120-176`)
   builds `&struct{F uintptr; captured int}{wrapper, captured}`, forces
   it onto the bump heap (`addr.SetEsc(EscHeap)` via the wasm3 branch
   added in `add2e0e83f`), then calls
   `runtime.wasm3WrapClosure(closureType, ocfunc(wrapper), addr-as-
   unsafe-ptr)`.
2. The compiler intrinsifies that runtime call to
   `OpWasm3MakeClosureRef`
   (`src/cmd/compile/internal/ssagen/intrinsics.go`), which lowers to
   `ref.func $wrapper; getValue captures; struct.new $closureCtx`
   (`src/cmd/compile/internal/wasm3/ssa.go:1033+`). The closureCtx
   shape is **per-signature**:
   ```
   $go.closure.<sig> = (struct (ref $go.func.<sig>) i64)
                                   ^funcref          ^captures-ptr
   ```
   (`src/cmd/compile/internal/wasm3/wasmabi.go:683+`).
3. The closure body, like any function with `NeedCtxt`, accesses
   captures via `OffPtr(GetClosurePtr, off) + Load`
   (`src/cmd/compile/internal/ssagen/ssa.go:518+`). On wasm3,
   `OpWasm3LoweredGetClosurePtr` returns `REG_CTXT` (global 1, i64),
   which the body interprets as the linear-memory address of the
   captures struct
   (`src/cmd/compile/internal/wasm3/ssa.go:697`).
4. At an indirect call site, `OpWasm3LoweredClosureCall`
   (`src/cmd/compile/internal/wasm3/ssa.go:414+`) does
   `struct.get $closureCtx 1` → CTXT, then `struct.get 0` → funcref,
   then `call_ref`.

Cost per evaluation: one `runtime.newobject` allocation for the linear-
memory captures struct, plus one `struct.new $closureCtx` allocation
for the wrapper. The wrapper-side allocation is unavoidable (the wasmgc
closureCtx is the indirect-call vehicle); the linear-memory one is the
target of this optimisation.

## Target shape

Per-closure (not per-signature) struct shape with captures inline:

```
$go.closure.<funcsym> = (sub $go.closure.<sig> (struct
    (ref $go.func.<sig>)
    <capture-0-storage>
    <capture-1-storage>
    ...
))
```

The base type `$go.closure.<sig>` stays `(struct (ref $go.func.<sig>))`
— just the funcref. Per-closure types extend it with capture fields.

**Why subtyping**: the call site sees a value of static type
`(ref $go.closure.<sig>)` (the call-site Go func type lowers to this).
Subtyping lets `struct.get $go.closure.<sig> 0` work for any subtype,
so the call site remains polymorphic over bare functions, method-value
wrappers, and per-closure shapes.

**Why per-closure** (vs per-(sig, capture-shape)): each closure literal
has a unique synthetic symbol (`main.main.func1`, etc.), and emitting
one type per symbol is the simplest indexing. Shared shapes between
closures with identical capture layouts could be coalesced later — pure
size win, no semantic change.

## Bare-vs-closure unification (resolved decision)

The cross-cutting question: at an indirect call site we don't know
whether the funcref needs a closure-self (`bare add` doesn't; `f :=
func(x) { ... captured ... }` does). The two viable resolutions:

**A. ALL bodies take an extra leading anyref param.** Changes the
   funcref signature globally. Direct callers of top-level functions
   must also pass anyref-null. Cleanest semantically; most invasive in
   call-site count.

**B. Use a side-channel global `CTXT_REF` (anyref).** Set at the
   indirect-call site, read in closure-body prologues that have
   captures. Bare functions ignore it. **No funcref signature change.**

We choose **B**. The funcref-signature change in A would touch every
direct call site of every Go function in the compiled program; B is
local to closure bodies and the indirect-call lowering. The runtime
cost of B is one `global.set` per indirect call and one `global.get +
ref.cast` per closure-body entry — negligible.

## Pieces (all required to land together)

### Piece 1: linker — new module global `CTXT_REF` (anyref)

File: `src/cmd/link/internal/wasm/asm3.go`, `writeGlobalSec3` (line
559+).

Currently emits 2 + N globals (bump pointer, CTXT, N closure
singletons). Add a third fixed global before the singletons:

```
global 2 (anyref, mutable): CTXT_REF — passes the closure-ref from
  the indirect-call site to closure bodies with captures. Default
  `ref.null any`.
```

Singletons become 3..N. Update `globalCtxIx` arithmetic in
`getOrAllocSingleton` (the singleton index offset shifts from 2 to 3
in `writeGlobalSec3` and any reader).

Update obj-encoder constants in
`src/cmd/internal/obj/wasm/wasm3obj.go`:
- `wasm3GlobalIndex(REG_CTXT) = 1` (unchanged).
- Define `wasm3GlobalIndexCtxRef = 2`.
- Singletons shift: any code referring to them by absolute index moves
  from `2 + idx` to `3 + idx`.

Encoding for the anyref global: valtype byte `0x6F` (anyref) per
function-references proposal, mutability `0x01`, init `0xD0 0x6F 0x0B`
(`ref.null any; end`). Confirm against wasmtime's parser.

### Piece 2: type collector — per-closure closureCtx subtype

File: `src/cmd/compile/internal/wasm3/wasmtype.go` and
`wasmabi.go:683 (collectClosureCtx)`.

Today `closureCtxs map[*types.Type]int` keys on the Go func type.
Add a parallel `perClosureCtxs map[*obj.LSym]int` keyed on the closure
body's LSym. The new entry is:

```go
wasmgc.Type{
    Name:   "go.closure." + closureSymName,
    Kind:   wasmgc.KindStruct,
    Super:  collectClosureCtx(funcType), // the per-signature base
    Fields: append(
        // inherited from base (just the funcref):
        []wasmgc.Field{{Storage: wasmgc.RefStorage(funcIdx, false), Mutable: false}},
        captureFields...,
    ),
}
```

`captureFields` is built by walking `clo.Func.ClosureVars` and
applying `lowerFields(v.Type)` per variable. By-value captures lower
to their natural shape (i64 for an int, ref for a *T, etc.). By-ref
captures lower to `(ref $box)` of the variable's type (the existing
heapaddr indirection — see step Piece 4).

The collector needs access to the closure's ClosureVars list at
type-collection time. The current collector receives only a
`*types.Type`. Extend it: introduce a new entry point
`collectClosurePerSym(sym *obj.LSym, ft *types.Type, captures []*types.Field) int`
called from the OpWasm3MakeClosureRef SSA-lowering site (Piece 3).

### Piece 3: SSA op — OpWasm3MakeClosureRefInline

File: `src/cmd/compile/internal/ssa/_gen/Wasm3Ops.go`, plus the lowering
in `src/cmd/compile/internal/wasm3/ssa.go:1033+`.

Replace (or add a sibling of) `OpWasm3MakeClosureRef`. The new op takes
`(funcref-implicit-via-aux) + N captures` as SSA Args and Aux = the
closure's LSym + a slice of captureType descriptors. Lowering emits:

```
ref.func $sym
getValue capture0
getValue capture1
...
struct.new $closureCtx_<closure>
```

The `getValue captureI` is whatever IR-typed SSA value the walk pass
provided for that capture (the variable itself for by-value, the
boxed `(ref $box)` for by-ref).

Drop the wasm3WrapClosure intrinsic (`intrinsics.go`): no longer
needed. walkClosure emits `OpWasm3MakeClosureRefInline` directly via
`s.entryNewValue...` from the SSA build (closing the route from
walkClosure's outputs through SSA construction).

### Piece 4: closure body prologue + capture access

File: `src/cmd/compile/internal/ssagen/ssa.go:518+` (the
`fn.Needctxt()` branch in `buildssa`).

Today (paraphrased):
```go
clo := s.entryNewValue0(ssa.OpGetClosurePtr, types.BytePtr) // i64
for each captured n:
    ptr := s.newValue1I(ssa.OpOffPtr, types.NewPtr(typ), offset, clo)
    if Byval && CanSSA: assign(n, load(ptr))
    else: setHeapaddr(n, ptr)
```

New wasm3 branch:
```go
// Get closure-ref from CTXT_REF, cast down to the concrete type.
cloRef := s.entryNewValue0(ssa.OpWasm3LoweredGetClosureRef, AnyrefType)
cloRef = s.entryNewValueA(ssa.OpWasm3LoweredCastClosureRef, cloRef,
    /* Aux = per-closure closureCtx type index */)
// Field 0 is funcref; captures start at field index 1.
for i, n := range fn.ClosureVars:
    fld := s.entryNewValueIA(ssa.OpWasm3GetClosureField, cloRef,
        1+i, /* Aux = field's wasm storage type */)
    if Byval && CanSSA: assign(n, fld)
    else: setHeapaddr(n, fld) // fld is the (ref $box) directly
```

Three new SSA ops needed:
- `OpWasm3LoweredGetClosureRef` — lowers to `global.get 2`.
- `OpWasm3LoweredCastClosureRef` (aux: type idx) — lowers to
  `ref.cast (ref $closureCtx_<closure>)`.
- `OpWasm3GetClosureField` (aux: field idx + storage type) — lowers
  to `struct.get $closureCtx_<closure> <idx>`.

The `OffPtr+Load` chain isn't synthesised at all on wasm3 — these new
ops replace it. No SSA rewrite pass needed; the change is at SSA
construction.

### Piece 5: indirect-call lowering — set CTXT_REF instead of CTXT

File: `src/cmd/compile/internal/wasm3/ssa.go`, `OpWasm3LoweredClosureCall`
case (line 414+).

Today (paraphrased):
```
struct.get $closureCtx 1   ; captures-ptr i64
global.set 1               ; CTXT
struct.get $closureCtx 0   ; funcref
... args ...
call_ref $funcType
```

New:
```
local.tee $tmp_ref         ; save the closure-ref
global.set 2               ; CTXT_REF = closure-ref
local.get $tmp_ref
struct.get $go.closure.<sig> 0   ; funcref (on base type, works for any subtype)
... args ...
call_ref $funcType
```

The captures-ptr field at index 1 in the per-signature base is gone
(base is now just `{funcref}`); the singleton initialisers in
`writeGlobalSec3` lose the `(i64.const 0)` operand to `struct.new`.

### Piece 6: walkClosure — emit captures as direct SSA args

File: `src/cmd/compile/internal/walk/closure.go`, lines 120-176.

Today the wasm3 branch builds `&struct{F, captures...}` on the heap,
casts the addr to unsafe.Pointer, and calls wasm3WrapClosure (which
the intrinsic rewrites to OpWasm3MakeClosureRef). Replace with a
direct call to a synthetic intrinsic `wasm3MakeClosureInline(F,
capture0, capture1, ...)` that the SSA intrinsifier rewrites to
`OpWasm3MakeClosureRefInline`.

For by-ref captures: walkClosure already prepares a per-variable
`*Variable` (the heapaddr); pass that directly. For by-value captures
that don't escape: pass the value. The OpWasm3MakeClosureRefInline
lowering emits `struct.new` with these values in order.

Drop the heap-alloc of `&struct{F, captures...}` entirely — captures
live in the wasmgc struct directly, no linear-memory copy.

### Piece 7: method-value -fm wrapper

File: `src/cmd/compile/internal/walk/closure.go:204+` (walkMethodValue)
and the typechecker that synthesises the -fm body.

The -fm wrapper today reads `this.R` from CTXT+8 (a linear-memory
load — see `cmd/internal/obj/wasm/wasm3obj.go` and the assembly dump
in this commit's verification). With the captures-in-struct model,
the wrapper must use the same per-closure shape:
`$closureCtx_<methval>` = `(struct (ref $funcType) <receiver-storage>)`,
and the wrapper body reads field 1 via struct.get on CTXT_REF.

This couples method values into the per-closure scheme. The body
generation for -fm is in
`src/cmd/compile/internal/typecheck/func.go:121+` (search for
"methodValueWrapper"). On wasm3, the synthesised body must:
- Have its NeedCtxt prologue use the same Piece 4 path
  (CTXT_REF + ref.cast + struct.get field 1).
- The first (and only) ClosureVar is the receiver — the existing
  ClosureVars walk reuses cleanly.

The walkMethodValue wasm3 branch from `105cd2e0b4` becomes a Piece 6
variant: build the per-method-value closureCtx with just the receiver
as capture; no heap-alloc of `{F, R}`.

## Migration order (single-PR commit sequence)

All pieces must land together for any test to pass — the call site
and body must agree on closure shape. Single PR, internal commit
order:

1. **(prep)** Add unused `OpWasm3LoweredGetClosureRef`,
   `OpWasm3LoweredCastClosureRef`, `OpWasm3GetClosureField` op
   definitions. Add CTXT_REF global to the linker. Verify a no-closure
   program still builds (no behaviour change).
2. **(prep)** Add `collectClosurePerSym` to typeCollector; add
   per-closure type-index lookup to `wasmtype.go`. Still unused.
3. **(switch)** In a single commit: piece 3 (new MakeClosureRef
   variant), piece 4 (body prologue), piece 5 (call site), piece 6
   (walkClosure), piece 7 (method values). Test with the existing
   `/tmp/wasm3-closure` and `/tmp/wasm3-method` programs first; then
   the audit programs.
4. **(cleanup)** Drop `wasm3WrapClosure` runtime declaration and the
   intrinsic. Drop the i64 captures-ptr field from
   `collectClosureCtx`'s per-signature struct (it's the new base, no
   captures-ptr).

## Cost / benefit

- **-1 heap allocation per closure-with-captures evaluation.** For a
  typical closure-heavy workload (callback-style stdlib code,
  goroutines later in M4), this is significant.
- **+1 anyref module global** (CTXT_REF). Negligible.
- **+1 ref.cast per closure body entry.** WasmGC ref.cast is a single
  type-tag comparison; on V8 this is sub-nanosecond.
- **+1 SSA op family** (GetClosureRef, CastClosureRef,
  GetClosureField). Pure additive; existing OpOffPtr/OpLoad paths
  unchanged for non-closures.
- **Per-closure wasmgc types** add to the type section. For a program
  with N distinct closure literals, +N types — same order as the per-
  closure singletons we already declare for bare-function references.

## Open questions

1. **Recursive captures**: a closure capturing itself (rare but legal
   via heap-address indirection) needs its closureCtx type to be
   self-referential through the `(ref $box)` capture field. wasmgc
   rec groups handle this naturally; verify the type collector emits
   the correct rec-group ordering.
2. **Closure escape across goroutines** (post-M4): the wasmgc
   closureCtx is host-GC-managed, so escape is free as long as it's
   live. Currently the bump-heap captures struct never gets reclaimed;
   moving to wasmgc actually fixes a long-term leak.
3. **Wrapper sharing**: today's wasm3 method-value wrappers are
   per-(receiver type, method) and DUPOK'd across packages. With per-
   closure closureCtx tied to the wrapper LSym, two packages that
   independently take `c.Add` get the same wrapper → same closureCtx
   type → wasmgc deduplication. Should "just work" through DUPOK
   semantics + per-LSym typeCollector keying.

## Implementation notes (state as of 594e7e00ef + 4b44103465)

The prep work is landed:

- `CTXT_REF` anyref module global (writeGlobalSec3, index 2,
  init `ref.null any`).
- `Wasm3GlobalIndexCtxRef` constant exposed from
  `cmd/internal/obj/wasm/wasm3obj.go`.
- Obj-encoder handles `AGlobalGet` / `AGlobalSet` with
  `TYPE_CONST` literal immediate (in addition to the existing
  `R_WASMCLOSURESINGLETON` form).
- Four new SSA ops with full codegen: `OpWasm3MakeClosureRefInline`,
  `OpWasm3LoweredGetClosureRef`, `OpWasm3LoweredCastClosureRef`,
  `OpWasm3GetClosureField`. Codegen in `wasm3/ssa.go`.
- `typeCollector.collectPerClosureCtx` registers per-closure
  subtypes keyed on the body's LSym.
- `wasm3RegisterPerClosureCtx` / `wasm3LookupPerClosureCtx`
  helpers in `wasm3/wasmabi.go`.
- `OpWasm3LoweredClosureCall` codegen now sets CTXT_REF
  (= closure-ref) in addition to CTXT (= captures-ptr i64) at
  every indirect call. Today no body reads CTXT_REF, so the cost
  is one extra `global.set` per call — negligible.
- Dead-store elim recognises the new ops as sym-effect carriers.

**The switch landed** as 4ff7b1a7c7 (1-int-capture) + 5a41024386
(2..4-int-capture extension). End-to-end demonstration:

- `makeAdder(5)` with one captured int compiles to:
  - caller: `ref.func $func1; i64.const 0; local.get $n; struct.new $closureCtx_func1` (1 wasmgc allocation, no heap captures struct).
  - body: `global.get $CTXT_REF; ref.cast (ref $closureCtx_func1); struct.get N` per capture.
- `makeLinear(3, 7)` with two captured ints returns
  `f(0)=7, f(1)=10, f(10)=37` under wasmtime.
- All prior regressions (bare funcvalues, method values, multi-
  capture stdlib closures, pointer-capturing closures, audit
  programs) still pass.

**Resolution to the per-FuncInfo registration problem**: option
(b) — a `sync.Map`-backed side channel `Wasm3ClosureBodyCaptures`
hosted in `cmd/internal/obj/wasm` (a leaf package both ssagen
and wasm3 already import). ssagen stashes `Wasm3ClosureBodyInfo
{FuncType any, Captures any}` keyed on the closure body's LSym;
wasm3 codegen's new helper `wasm3EnsurePerClosureCtxFromSide`
type-asserts back to `*types.Type` / `[]*types.Type`, lazy-
registers the per-closure ctx in *this* FuncInfo's typeCollector
on the first `LoweredCastClosureRef` / `GetClosureField` it
encounters. The obj package stays free of compiler-internal type
references thanks to the `any` fields.

**Bug fix surfaced during multi-capture testing**:
`GetClosureField` re-emits `ref.cast` before each `struct.get`.
The per-value local for `LoweredCastClosureRef` is declared
`anyref` (wasm3place.go has no per-closure type index at
declaration time), so a follow-on `local.get; struct.get`
chain trips "expected (ref null $type), found anyref" once the
first capture stops being a `local.tee` short-circuit. Cost is
one extra `ref.cast` per access — a single type-tag compare on
V8/wasmtime.

**Remaining limitations** (each is a scoped, mechanical follow-
up; none block the headline path):

- Captures other than integers: floats and complex types aren't
  routed through the `uintptr`-arg intrinsic; the body would
  decode them incorrectly anyway. Falls back to the legacy
  `wasm3WrapClosure` heap-captures path.
- Pointer captures: the body's pointer-deref lowering still
  emits `i32.wrap + i64.load` assuming a linear-memory pointer,
  but a captures-in-struct field gives an anyref-shaped wasmgc
  ref. Documented in `wasm3ScalarByValClosureVar`; needs body-
  side pointer-deref to recognise wasmgc pointer locals.
- More than 4 captures: bounded by the per-arity
  `wasm3MakeClosureInlineN` intrinsics declared in
  `runtime.go`. Extending to 5, 6, 7, ... is purely mechanical
  (one runtime decl + one `add(...,sys.ArchWasm3)` per arity).
- Method-value `-fm` wrappers: still on the legacy
  `wasm3WrapClosure` path; they're auto-generated bodies and
  haven't been routed through the captures-in-struct prologue
  yet.
- Cross-package per-signature `closureCtx` types are duplicated
  (one per FuncInfo that registers them). The wasm engines
  canonicalise structurally-equivalent rec groups so this
  works in practice — but the type section is fatter than it
  needs to be. The linker's `mergeTable` says: "Today the merge
  appends each package's program types unconditionally —
  duplicates across packages produce duplicate type-section
  entries, which is valid wasm but redundant. Structural
  deduplication across packages is a later refinement."
