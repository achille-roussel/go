# wasm3 M3 — Bypassing the SSA register allocator

Date: 2026-05-16
Status: design pre-implementation, supersedes `wasm3-m3-ref-registers.md`
Context: M3 foundational restructure (see `doc/wasm3-m3-notes.md`)

## The reframe

The Go SSA register allocator exists to solve a problem wasm doesn't
have: physical hardware has 16-32 registers, you have to choose which
SSA values live there and which spill to memory, and the cost of the
choice matters because memory accesses are slow.

wasm has none of this.

- Locals are effectively unlimited — a wasm function can declare any
  number, the binary cost is one entry per local in the function
  body's locals vector.
- Locals are exactly as fast as registers from the Go compiler's
  point of view — both are abstract slots the wasm engine manages.
- The engine (V8, wasmtime, etc.) does **real** register allocation
  onto host CPU registers when it JITs the function. That work
  duplicates anything the Go compiler does up front.

The existing wasm/wasm3 backends do regalloc anyway because they were
forked from a register-machine codebase. The 16-GP-register-plus-spill
dance is busy work that produces the same observable output as a
direct value→local mapping, just with more compiler complexity.

This document proposes deleting that dance for wasm3.

## The architecture

**One SSA value → one wasm local, typed precisely.**

Every SSA value the function produces (post-lowering, post-schedule)
gets its own wasm local. The local's wasm value type matches the
value's Go type:

| Go type | wasm local type |
|---|---|
| `bool`, `int8/16/32`, `uint8/16/32` | `i32` |
| `int`, `int64`, `uint`, `uint64`, `uintptr` | `i64` |
| `float32` | `f32` |
| `float64` | `f64` |
| `*T` (typed pointer) | `(ref null $T)` |
| `unsafe.Pointer` | `(ref null any)` |
| composite types | flattened into multiple locals (one per leaf field) |

Each producer emits its instruction sequence and a single
`local.set N`. Each consumer emits `local.get N` before its use.
No register classes, no spill slots, no `ref.cast` casts for refs
(every local is precisely typed), no anyref pool.

## OnWasmStack preserved as a peephole

The current backend keeps single-use values on the wasm operand
stack to avoid the `local.set N; local.get N` round-trip. We
preserve that optimisation as an independent peephole pass:

- A value V is "stack-stayable" iff: V has exactly one use, the use
  is in the same basic block, and V is the immediately-preceding
  value in schedule order, and no value between V and its use has
  side effects on the operand stack.
- For stack-stayable V: skip the `local.set` after the producer and
  the `local.get` before the consumer.

This is the same shape as `OnWasmStack` today; it just runs as a
wasm3-specific peephole instead of falling out of regalloc.

## Function parameters and `_start`

wasm function parameters are locals 0..N-1 in signature order. The
value-placement pass assigns SSA `OpArg*` values (the formal
parameter ops) to those local indices directly. The rest of the
locals (the per-value ones) come after.

## What the new pipeline looks like

```
Existing wasm3 pipeline:                  New wasm3 pipeline:

  lower                                    lower
  cse                                      cse
  dse                                      dse
  schedule                                 schedule
  flagalloc       \                        wasm3-place-values  (NEW)
  regalloc        --- skipped for wasm3    wasm3-onstack       (NEW)
  stackalloc      /                        genssa
  trim                                     trim
  genssa
```

The two new passes are short (estimated <200 lines combined):

- `wasm3-place-values`: walks the (already-scheduled) values,
  assigns each one a wasm local index and records its wasm value
  type. Stores the mapping on `Func` as a sidecar.
- `wasm3-onstack`: runs the OnWasmStack analysis described above,
  marking each value as either "via local" or "stays on stack."

## Backend changes

`cmd/compile/internal/wasm3/ssa.go`:

- `ssaGenValue` no longer calls `v.Reg()`. Instead it consults the
  per-value local map. For a producer: emit the instruction then
  `local.set <idx>` (or skip the set if OnWasmStack). For each
  use: `local.get <idx>` (or skip if the producer left it on the
  stack).
- `getValue32` / `getValue64` collapse into a single
  `pushValue(v, t)` that emits `local.get <idx>` and any width-
  matching conversions (`i32.wrap_i64`, `i64.extend_i32_u`).
- `setReg` / `getReg` and their callers go away.
- The call site argument-pushing loop pushes per-register-arg
  values from the per-value mapping.

`cmd/internal/obj/wasm/wasm3obj.go`:

- `wasm3Locals` becomes radically smaller. It no longer scans the
  prog stream to discover locals — the compile-time pass has
  already emitted the local-declarations vector via a function aux.
  `wasm3Locals` just reads the aux and emits the declarations
  verbatim.
- The spill-slot tracking (`spillOf`, `wasm3SpillSlot`) goes away.
- The narrow-parameter widening prologue stays: a wasm i32
  parameter widens into the function's first user local once at
  entry, same as today.

`cmd/compile/internal/ssa/_gen/Wasm3Ops.go`:

- Most regInfo entries (`gp01`, `gp11`, `fp32_21`, etc.) become
  unused and can be deleted, simplifying ops to `argLength: N,
  aux: ...` without register masks.
- The register-name table (`regNamesWasm3`) shrinks to special
  registers only: SP (for the bump allocator pointer until M3
  Stage J retires it), g (currently unused but referenced by some
  generic ops), and SB (the pseudo-register for symbol refs).

## What this enables

1. **Refs are free.** Every `*Point` value's local is declared
   `(ref null $Point)`. struct.new, struct.get, struct.set need no
   casts; the wasm engine sees the precise types and optimises
   accordingly. The whole `gpRef` register-class design from the
   previous doc evaporates.

2. **Slices, strings, interfaces are mechanical.** Composite types
   flatten across multiple locals (e.g. a `string` SSA value
   becomes 3 locals: `(ref null $go.bytes)`, `i32`, `i32`). Each
   field of the composite has its own precise local type.

3. **The runtime-fork shims can retire.** `gwrite_wasm3.go`,
   `printint_wasm3.go`, `write1_wasip1_wasm3.go`, `newobject_wasm3.go`
   exist because escaping `&local` values produced `Get $name(SP)`
   that the current backend bails on. With per-value locals, an
   escaping local just becomes a `(ref null $T)` local that the
   address-of operation captures the index of — no SP at all.

4. **Binary size stays similar.** More local declarations (~10
   bytes per local on average), but the OnWasmStack peephole keeps
   single-use values on the stack so we don't pay the local.get/set
   tax for them. Heavily-used values pay one local.set + one
   local.get per access, exactly as today.

## What this costs

- A real restructure of `wasm3/ssa.go` and `wasm3obj.go`. The
  current ~2000 lines across both files collapses to maybe ~1200,
  but those 1200 are different lines.
- A new compile pass slot. Need to plumb the wasm3-specific passes
  into `ssa/compile.go` via the same gating mechanism that decides
  arch-specific passes today.
- Risk: some pre-regalloc SSA passes might break if regalloc
  doesn't run after them. Each pass needs to be audited for wasm3
  compatibility. Mitigation: start by skipping ONLY regalloc and
  the passes that depend on its output (stackalloc, flagalloc,
  trim), keep everything else; iterate from there.

## Implementation sequence

**Phase 1 — value-placement pass landing in parallel with regalloc.**

Add the `wasm3-place-values` pass as a new pipeline phase that runs
**alongside** regalloc (not instead of), recording the per-value
local map on a sidecar. genssa still uses `v.Reg()`. This pass is
inert from a codegen perspective — verifying it produces sane output
without changing behaviour. Easy rollback.

**Phase 2 — flip ssaGenValue to use the new map for one op family.**

Pick a small op family (probably the const ops: `OpWasm3I32Const`,
`OpWasm3I64Const`, etc.). Make their ssaGenValue cases consult the
new map for output placement, while everything else still uses
`v.Reg()`. Verify a const-heavy test still works. This proves the
hybrid path.

**Phase 3 — convert all op families to the new map.**

Op family by op family, switch `setReg(v.Reg())` to
`localSet(localOf[v.ID])`. The current backend ~50 cases; expect
~20-30 are mechanical, rest need attention. Each conversion is
verifiable in isolation.

**Phase 3a (lifecycle + layout prep, landed):**
attachWasmType is moved to run *before* SSA passes (it was a
post-genssa hook), so wasm3PlaceValues and genssa can both read
the per-function WasmType.Params. Per-value locals are
declared in the wasm function body immediately after the
parameter locals (was: at the very end) — this gives genssa
a deterministic formula `nparams + Wasm3ValueLocals[v.ID]` for
each placement's absolute wasm-local index without needing to
know the register-local or spill-local count. `OpArgIntReg`
and `OpArgFloatReg` are intentionally skipped by the placement
pass: the wasm function signature already places parameters in
locals 0..nparams-1, so the regalloc fallback (`getReg(v.Reg())`
→ `local.get <param>`) reads them in place. With these in
place, the codegen flip becomes an isolated mechanical change.

**Phase 3b (the codegen flip, LANDED):**

`setReg(v.Reg())` in `ssaGenValue`'s default case flips to
`localSetIdx(nparams + Wasm3ValueLocals[v.ID])` whenever a per-
value local exists; same for `getValue32`/`getValue64` on the
read side. Values without a placement (`OpArg*`, `OpSelect*`,
spilled values) still take the regalloc-managed register-local
path.

Per-edge Phi resolution replaces regalloc's destination-
register-sharing scheme. `wasm3PlaceValues` now assigns a per-
value local to every `OpPhi`, and the wasm3 backend's
`ssaGenBlock` invokes `emitPhiCopies` before each control
transfer: for every Phi in the successor block, push the
incoming value from this predecessor and `local.set` into the
Phi's local. The source push is delegated to `readPhiSource`
which handles the three places a Phi argument can live — per-
value local, register-local (for unplaced values like `OpArg*`),
or spill local via `spillLoadOp`+`AddrAuto` (for values
regalloc decided to spill).

`OpSelect0`/`OpSelect1`/`OpSelectN` are intentionally left
unplaced. Regalloc assigns them the same register as the
underlying tuple element (`regalloc.go` ~1519), and the call
result placement loop populates that register-local. Routing
selectors through the register-local path (rather than giving
them their own per-value local that nothing populates) lets
calls work without modifying the call-result placement loop.

The cycle case for parallel Phi copies (the classic "swap"
problem — Phi1 reads Phi2 while Phi2 reads Phi1) is not yet
handled. The M2 regression test programs don't hit it; the
standard fix is to detect cycles in `emitPhiCopies` and break
them with a per-cycle temp local.

Binary cost so far: ~60 bytes per wasm3 binary against the
Phase 3a baseline. Both regalloc-managed register-locals *and*
per-value locals are declared while both schemes coexist; the
size win arrives in Phase 4 when regalloc is skipped entirely.

**Phase 3c (incremental followups):**

- OpArg{Int,Float}Reg routed through per-value locals via an
  entry-prologue copy (`local.get <param>; widen?; local.set
  <Larg>`). `wasm3OpArgParamInfo` walks WasmType.Params to map
  the per-class v.AuxInt to the absolute wasm parameter index.
- OpSelectN routed through per-value locals via the call result
  placement loop scanning v.Block.Values for selectors whose
  Args[0] == call. ssagen's outer genssa loop has a
  `case ssa.OpSelect0, ssa.OpSelect1, ssa.OpSelectN,
  ssa.OpMakeResult: // nothing to do` clause and never calls
  Arch.SSAGenValue for selectors, so the bridge must run on the
  call side, not the selector side.

After these, the values still on the regalloc-managed register-
local path are: regalloc-inserted OpCopy values (added after
wasm3PlaceValues runs, so unplaced); REG_CTXT (closure context);
REG_SP / NAME_PARAM (caller SP for runtime); and anything
regalloc decides to spill.

Attempted but didn't help: a `wasm3-patch-values` pass after
regalloc to extend Wasm3ValueLocals over the OpCopy values
regalloc inserted. The OpCopy values are themselves Phi-
resolution copies (regalloc's traditional Phi scheme); giving
them per-value locals doesn't eliminate them — it just renames
their backing store from a register-local to a per-value local
of equal size. Net result was +62 bytes on the M2 regression
binary, not bytes saved.

Landed instead, two smaller wins that recover most of the size
overhead from running both schemes side by side:

- `readPhiSource` now traces through OpCopy chains
  (regalloc.go's Phi-resolution copies) to read the original
  value's per-value local directly. Reaches an OpCopy that
  *is* placed only if a future pass started giving them per-
  value locals; otherwise traces to the non-OpCopy source.
- For rematerialized constants (`OpWasm3I64Const`,
  `OpWasm3F32Const`, `OpWasm3F64Const`) that regalloc
  duplicated, `ssaGenValue`'s default case suppresses the
  original emit + setReg, and `readPhiSource` recreates the
  const inline at each Phi edge that consumes it. The dead
  register-local no longer gets declared by the obj backend's
  wasm3Locals scan.
- `wasm3HasOutput` skips placement for `Uses == 1` non-generic
  non-memory values — the same condition regalloc uses to flag
  OnWasmStack candidates. If the value really is consumed
  inline on the wasm stack, the per-value local would have
  been declared but never written; if it ends up needing a
  local, genssa's fallback declares a register-local on
  demand — same cost as the old register-local scheme.

Net binary impact (M2 14-case regression): 12141 (pre-flip) →
12179 (post-flip with all Phase 3c followups), a +38 byte
overhead for the regalloc-bypass machinery while regalloc itself
is still running.

**Phase 4 — skip regalloc for wasm3. ✅ LANDED.**

`regalloc()` early-returns on `f.Config.arch == "wasm3"`, leaving
`f.RegAlloc` nil. The places that previously consulted regalloc's
output now handle the empty case:

- `value.AutoVar` returns `(nil, 0)` when both the RegAlloc
  location and `v.Aux` are absent; the liveness machinery's
  `n == nil` short-circuit then skips the entry. Triggered by
  OpKeepAlive on values that previously got LocalSlot.
- `liveness.epilogue` and the post-compact "recorded as live on
  entry" sanity check both early-`continue` on wasm3 when an
  output param remains live at function entry. Without
  regalloc-inserted OpStoreReg kills, output variables can
  stay live; the wasm engine scans declared locals directly so
  an over-conservative liveness map is harmless. Triggered by
  vendor/golang.org/x/net/dns/dnsmessage.NewBuilder.
- `ssagen.CheckLoweredPhi` early-returns when `RegAlloc` is
  empty — the Phi-arg-register-equivalence assertion it does
  has nothing to check when there are no registers.
- The wasm3 backend's `OpWasm3LoweredAddr` `*ir.Name` case
  names `REG_SP` directly instead of reading `v.Args[0].Reg()`
  (args[0] is always OpSP in this case).

**Phase 4b — OnWasmStack analysis ported to wasm3PlaceValues. ✅ LANDED.**

The OnWasmStack analysis used to live inside regalloc setup; with
regalloc gone, `wasm3MarkOnStack` runs as the first step of
wasm3PlaceValues. It mirrors `regalloc.go` ~850's eligibility
check with one extra constraint: skip the v.Args scan when `v`
itself is generic. Without that constraint, generic consumers like
OpConvert — which `ssagen` handles via `nothing to do` and never
call into the wasm3 backend — leak OnWasmStackSkipped. Triggered
by runtime.SetFinalizer.func1.

readPhiSource also gained an OnWasmStack branch that decrements
Skipped and re-emits the value inline via `ssaGenValueOnStack`, so
a Phi source flagged OnWasmStack lands on the wasm stack at the
Phi resolution point.

**Phase 4 final state:** M2 14-case regression passes 14/14;
`go build std` succeeds clean for `GOOS=wasip1 GOARCH=wasm3`;
binary 12141 (pre-flip Phase 3a) → 12038 (post-Phase 4b +
codegen polishing) — **-103 bytes, 0.8% smaller** than the
baseline. The regalloc-bypass rewrite is now a net byte saver,
not a cost.

Codegen polishing that made up the difference:

- `cmd/internal/obj/wasm`: run-length encode the locals
  declaration vector. Consecutive same-type entries collapse
  into one (count, type) pair. wasm3PlaceValues output is
  largely all-i64 runs, so the saving is meaningful — listSum's
  16-i64 + 1-i32 vector goes from 34 declaration bytes to 4.
- `cmd/internal/obj/wasm`: peephole `local.set N; local.get N`
  → `local.tee N` in the wasm3 encoder. Saves 2 bytes per
  occurrence (one opcode + one leb128 index repeat). 14
  occurrences in the M2 regression's 34 functions.
- `wasm3MarkOnStack` whitelists `OpMakeResult` from its "skip
  arg scan when v is generic" rule — BlockRet pulls each return
  value via getValue / getValue32 / getValue64, so MakeResult
  args do honor the OnWasmStack contract. Return expressions
  now flow straight off the wasm stack instead of round-tripping
  through a per-value local.

**Phase 5 — drop register-class infrastructure from Wasm3Ops.go.**

Now that nothing reads regalloc's output, the `reg: gp01 / gp11 /
gp21 / fp32_01 / fp64_01 / regInfo{...}` annotations on every
Wasm3Op are bookkeeping for an unused subsystem. Strip them and
the gp/fp register-mask vars in Wasm3Ops.go's local scope.

**Phase 5b — OnWasmStack as a peephole pass (deferred).**

Optional refinement: replace the regalloc-style backward-walk
analysis with a more aggressive peephole that catches additional
inline-eligible patterns the current heuristic misses. Verify the
14-case regression still passes and measure.

**Phase 6 — retire the runtime-fork shims.**

With escaping locals now becoming refs (no SP needed), revert
`printint_wasm3.go`, `gwrite_wasm3.go`, `write1_wasip1_wasm3.go`,
`newobject_wasm3.go`, `printstring_wasm3.go`, `printlock_wasm3.go`,
and let the standard runtime implementations work.

Each phase is committable independently and rollback-safe.

## Comparison with the ref-register design (Option A from prior doc)

| | Ref-register class (anyref + cast) | One-local-per-value |
|---|---|---|
| New register class needed | yes (`gpRef`, 8 regs) | no |
| Per-ref-access overhead | 3 bytes (`ref.cast`) | 0 bytes |
| Refs typed precisely | no (all anyref) | yes |
| Implementation effort for refs | ~moderate | bundled with bigger restructure |
| Implementation effort overall | small (just refs) | larger (touches all ops) |
| Backend size after | grows | shrinks |
| Enables retiring shims | no | yes |
| Long-term direction | needs eventual rewrite | is the eventual rewrite |

The ref-register design is the smaller change but it's load-bearing
technical debt: anyref + casts is exactly the kind of thing the
wasm engine has to undo at JIT time, and we'd be paying the
compile-time and code-size cost for nothing.

The one-local-per-value design is the larger change but it pays
back across the whole M3 — every subsequent stage (slices,
strings, interfaces, maps) needs ref handling, and they all get it
for free.

## Open questions

- **How big is the binary-size impact in practice?** Need to
  measure on the hello/algos/heap-alloc benchmarks. Anecdotal
  estimate: ~5-10% larger pre-OnWasmStack, recovered to within
  ±2% with the peephole.
- **Does any SSA pass break?** Specifically `tighten`, `dse`,
  `phi tighten` — they're all listed as before regalloc but might
  emit value forms that only work post-regalloc. Audit needed.
- **OpPhi handling.** Phi nodes need their args' locals to be the
  same for the resolution to work. The value-placement pass needs
  to merge phi arg local indices accordingly. Doable but a corner
  to get right.
- **Composite-value lowering.** A struct-typed SSA value flattens
  to multiple locals — the place-values pass needs to know how to
  count and type the leaves. Aligns with what `wasm3Fields`
  already does for parameters.

## Recommendation

Do this. It's the right architecture for wasm and it makes every
remaining M3 stage easier instead of harder. Start with Phase 1
(value-placement landing in parallel with regalloc) — that's a
small, no-risk first step that creates the scaffold.
