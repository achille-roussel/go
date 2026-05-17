# wasm3 M3 — Structured control flow via a Relooper

Date: 2026-05-17
Status: design pre-implementation
Context: M3 hardening (see `doc/wasm3-m3-notes.md`)

## The problem

The wasm3 obj-encoder's CFG pass (`cmd/internal/obj/wasm/wasm3obj.go:828`,
`wasm3AnalyzeCFG`) chooses between two code-emission modes:

- **Forward-only** — when every `AJMP` in the prog stream targets a
  later boundary than its source, the body is wrapped in nested
  `block`s — one per distinct forward target — and each AJMP becomes
  `br <depth>`. Output is structured wasm control flow.
- **Dispatch** — the moment any AJMP is a back-edge (i.e., the
  function contains a natural loop), the body switches to a wasm1-
  style giant `loop $L { block { block { ... br_table dispatch ... } } }`
  scheme. Every basic block sits inside one of N nested `block`s,
  branches set an `i32` dispatch local and `br $L` to restart, and a
  `br_table` at the top of `$L` jumps to whichever basic block the
  dispatch indicates.

Empirically (May 2026):

- `hello.wasm` (one `fib` loop) → 1 `br_table`, 7,015 bytes.
- `algos.wasm` (4 loop-bearing functions) → 4 `br_table`s, 8,401 bytes.

Every function with a `for` / `for range` / `goto`-back-edge pays for
a dispatch trampoline. The trampoline is wasm1-shaped residue: it
emerged because the original wasm backend took the "fall back to
trampoline on any loop" shortcut, and the wasm3 obj-encoder inherited
that shortcut when it forked off.

This is **not** the goroutine-scheduler PC_F/PC_B trampoline — that
one was already removed at the M2 cutover. The dispatch trampoline
above is a separate, code-quality issue: nothing about WasmGC,
stack-switching, or exception handling forces it.

Engines optimise structured CF much better than `br_table` dispatch
(predictable branch shapes, natural loop entry-points for tier-up
JITs, no spurious data-flow through the dispatch local). The binary
size win is also real — a dispatch nest of N blocks costs `N+2`
end-bytes plus the `br_table` vector plus the per-AJMP dispatch
prelude. A structured emission costs `loop`+`end` per loop plus
`block`+`end` per merge point.

## The decision

**Implement a Relooper-style pass that reconstructs structured wasm
control flow from the SSA basic-block graph.**

The alternative — preserving Go's syntactic structure through SSA so
we can emit it directly — was considered and rejected. The deep
argument is structural: SSA is *designed* to discard syntactic
structure so that CFG-rewriting passes (loop rotation, branch
elimination, CSE, DCE, tighten, copyelim) can transform freely. By
the time control reaches obj-emission, the CFG no longer corresponds
to any single Go source structure — most notably after `loopRotate`,
which moves the loop test from the header to the footer and changes
which block ends the back-edge. Preserving "this was a `for` loop
continue arrow" through these passes requires either:

- threading wasm-specific metadata through every generic SSA pass
  (high ongoing maintenance burden on contributors who don't use the
  wasm target),
- disabling the passes on wasm3 (sacrifices optimisation for
  marginally cleaner disassembly — bad trade), or
- forking the backend to emit wasm directly from the AST (massive
  change; loses SSA's value-level optimisations within structured
  wasm).

A relooper, by contrast, operates on whatever CFG it's given. The
algorithm is well-understood (Zakai's Relooper, OOPSLA 2011;
Stackifier, PLDI 2017; Ramsey's "Beyond Relooper," 2022), reference
implementations exist (Binaryen, LLVM's WebAssembly CFG Stackify pass),
and the input — SSA basic blocks with control-flow edges — is exactly
what's available at obj-emission time.

The relooper is the standard answer in the field for "compiler with
SSA → wasm structured CF". This document commits wasm3 to it.

## The algorithm

The Go SSA already provides every analysis the relooper needs:

- `f.Sdom()` — sparse dominator tree (`SparseTree` in
  `cmd/compile/internal/ssa/dom.go`).
- `f.loopnest()` — natural-loop analysis (`loopnest` in
  `cmd/compile/internal/ssa/likelyadjust.go`), with per-loop header,
  outer-loop pointer, nesting depth, and a `hasIrreducible` flag for
  CFGs the loop analyser cannot reduce.
- `f.Postorder()` / reverse-postorder — block traversal in dominance
  order.

The reconstruction proceeds in two structural passes plus an emission
pass:

### Pass 1 — Block scheduling

Walk blocks in reverse-postorder. This gives a topological ordering
consistent with dominance (every block appears after its
immediate dominator) and, on reducible CFGs, makes each natural-loop
body contiguous.

### Pass 2 — Scope inference

For each block B in the schedule, determine which wasm scopes need
to be **open** when B is emitted:

- **Loop scopes.** If B is the header of a natural loop L (i.e., L
  has a back-edge with B as target), open a `loop $L_B` scope
  immediately before B. The scope closes after the last block in L's
  body. The back-edge becomes `br $L_B`; falling out of the loop
  reaches the `end`.

- **Block scopes (forward-branch merge points).** If B has multiple
  predecessors and at least one of them is a forward edge from
  outside the smallest scope containing B, open a `block $B_B` scope
  spanning [earliest-block-needing-to-br-to-B, B). Forward branches
  to B become `br $B_B`; B's natural fall-through is `end + B`.

- **If scopes.** A two-successor block B whose successors S1 and S2
  both join at a common dominatee J can lower to `if $S1 else $S2 end`,
  with J following the `end`. This is an optimisation over the
  generic "two `block`s and `br`" lowering — smaller binary, better
  for engine branch prediction. Detect when both successors are
  immediately dominated by B and their merge is also immediately
  dominated by B.

Scope inference is local to each block. The scope stack at any
emission point is determined by intersecting the block's "must be
inside" set across all open scopes.

### Pass 3 — Emission

Walk the schedule in order, maintaining a stack of open scopes:

1. Before emitting B, close any scopes whose body ends at B's
   position (emit `end`s) and open any scopes that begin at B (emit
   `loop`, `block`, or `if`).
2. Emit B's prog body (the existing `ssaGenValue` output) verbatim.
3. For B's terminator:
   - One successor S that is the immediate fall-through: no branch.
   - One successor S elsewhere: emit `br $scope-targeting-S` where
     `$scope-targeting-S` is the nearest enclosing scope whose `end`
     lands at S, or whose `loop` header is S (for back-edges).
   - Two successors via an `If`-shaped scope: emit the condition,
     `if`, recurse, `else`, recurse, `end`.
   - Two successors via the generic "two `block`s" lowering: emit
     `br_if` to one and fall through to the other; the surrounding
     scopes ensure each lands at the right `end`.
   - `Ret`: emit `return`.

This emission walk replaces the current `wasm3AnalyzeCFG` +
prog-stream walk in `encodeWasm3Body`. The obj-side encoder still
exists, but it consumes a prog stream that already carries the right
`ABlock`/`ALoop`/`AIf`/`ABr`/`AEnd` ops — no CFG analysis at
obj-emission time.

### Irreducible CFG handling

`f.loopnest().hasIrreducible == true` signals that one or more
strongly-connected components in the CFG don't form natural loops
(a multi-entry SCC, typically the result of `goto` into a loop body
or an unstructured `goto` mesh). The relooper falls back to a
dispatch-block scheme **for the affected SCC only**:

- Compute the SCC's entry blocks.
- Wrap the SCC's blocks in a `loop $L` + N nested `block`s with a
  `br_table` selector, identical in shape to today's whole-function
  dispatch — but scoped to the SCC.
- Blocks outside the SCC continue to use structured emission and
  reach SCC entry blocks via the dispatch-set-then-`br` sequence.

This is a strict improvement over today: the dispatch tax pays only
for the irreducible region, not for the entire function. Functions
that contain a small irreducible region plus a large reducible region
get most of the size win.

In practice, irreducible CFGs in Go are rare. They require either
`goto` patterns that aren't structurally reducible, or compiler
passes producing them — most commonly they don't. Most or all of the
audit programs are 100% reducible.

## Integration point

Two implementation strategies, picking one:

### Strategy 1 — pre-pass in `ssagen`

A new pass in `cmd/compile/internal/wasm3/` runs after SSA codegen
but before the obj-side. It takes `f` (the `*ssa.Func`) plus the
emitted prog stream and rewrites the prog stream to carry
`ABlock`/`ALoop`/`AIf`/`AEnd`/`ABr` ops directly. The obj-side
`encodeWasm3Body` becomes a pure prog→bytes encoder with no CFG
analysis.

Pro: clean separation. The CFG reconstruction is in the
architecture-specific package, not in the generic obj layer.
Con: needs careful prog-stream surgery — re-anchoring AJMPs to the
right scope labels.

### Strategy 2 — replace `wasm3AnalyzeCFG` in obj

Keep the prog stream as-is. Replace `wasm3AnalyzeCFG` with a routine
that takes the prog stream + a sidechannel containing the SSA
loopnest + dominators (passed via a per-LSym auxiliary structure),
runs the relooper, and produces the scope plan that
`encodeWasm3Body` consumes.

Pro: smaller diff — `encodeWasm3Body`'s emission loop already takes
a `wasm3CFG` plan.
Con: requires plumbing SSA-level data structures (dominators, loop
nest) down to the obj layer, which has no SSA dependency today.

**Recommend Strategy 1.** The obj layer shouldn't grow a dependency
on SSA internals. The CFG reconstruction is logically an
architecture-specific lowering step — it belongs alongside the rest
of the wasm3-specific codegen.

## What this enables

- **Binary size.** Each loop-bearing function drops its per-function
  dispatch trampoline. Concretely, for a function with N basic
  blocks containing one natural loop today's emission is roughly
  `loop + N×(block + 0x40) + br_table_vector(N+1) + per-AJMP
  (i32.const + local.set + br)`. After reloop: `loop + per-loop
  blocks + per-AJMP br`. Saves the entire br_table vector and the
  per-AJMP dispatch prelude.
- **Engine optimisability.** V8 / wasmtime / SpiderMonkey tier-up
  JITs treat `loop` headers as natural OSR entry points and
  structured CF as the input format their optimisers were built for.
  `br_table` over an i32 dispatch local creates a data dependency
  the optimiser has to track through, often defeating loop-invariant
  code motion across what should be a flat control edge.
- **Disassembly readability.** `wasm-tools print` produces something
  a human can follow — a `for` loop appears as a `loop`, an `if`
  appears as an `if`. Today's disassembly looks like a coroutine
  trampoline.
- **Foundation for Stage F.** Interface method dispatch (Stage F)
  adds itab struct types whose method-ref fields are invoked via
  `call_ref`. Each method is a separate wasm function. Without the
  relooper, each loop-bearing method pays a trampoline. Stage F's
  function-count multiplier compounds the dispatch tax.

## What this costs

- **Implementation effort.** A correct reducible-CFG relooper plus
  irreducible SCC dispatch fallback. Estimated 2–3 focused sessions.
- **Test surface.** The audit programs (`hello`, `algos`, `sortMe`)
  exercise reducible CFGs and should be byte-for-byte deterministic.
  Add at least one program with a goto-induced irreducible CFG to
  exercise the dispatch fallback. The M2 14/14 regression should
  pass unchanged.
- **One-time disassembly diff.** All wasm3 binaries change shape on
  the same commit. The diff is large but mechanical (block/loop/if
  vs. dispatch nest); reviewable by running `wasm-tools print` on
  one representative binary before and after.
- **Maintenance.** The relooper sits in a single file (~300–500
  lines is a reasonable target including comments). It's
  self-contained — no other backend code needs to know it exists.

## Implementation sequence

1. **Spike** — implement the reducible-CFG relooper on a single
   function (e.g., `fib`) by hand, in a scratch test, producing
   structured ops. Verify the output validates and runs. ~½ session.
2. **`cmd/compile/internal/wasm3/cfg.go`** — productionise the spike
   into a per-function pass. Use `f.Sdom()` and `f.loopnest()`. Emit
   `ABlock`/`ALoop`/`AIf`/`ABr`/`AEnd` into the prog stream;
   delete redundant `ARESUMEPOINT`s (or repurpose them as scope
   boundaries). Drive entirely off the SSA block graph. ~1 session.
3. **`encodeWasm3Body`** — simplify by removing `wasm3AnalyzeCFG`
   and the dispatch-mode branch. The encoder just walks the prog
   stream and emits bytes. ~½ session.
4. **Irreducible fallback** — detect via `f.loopnest().hasIrreducible`
   and emit the dispatch nest scoped to the affected SCC. ~½ session.
5. **Validation** — run the M2 14/14 regression, the audit programs,
   `wasm-tools validate` on each output, measure binary size deltas,
   diff one disassembly for review. ~½ session.

Total: ~2–3 sessions. Each step lands as its own commit.

## Comparison with alternatives

| Approach | Effort | Robustness vs. SSA passes | Disassembly fidelity | Maintenance |
|---|---|---|---|---|
| **A: Relooper (this doc)** | 2–3 sessions | Full — operates on whatever CFG arrives | Structured, but synthetic; doesn't recover source names | Localised to wasm3 |
| **B1: structure metadata through SSA** | Large | Fragile — every CFG-rewriting pass must maintain it | Higher — can map back to source | Ongoing tax on all SSA contributors |
| **B2: disable CFG-rewriting passes on wasm3** | Small | n/a — sacrifices optimisation to preserve structure | Higher | Marked-as-special architecture |
| **B3: emit wasm from AST, SSA only for values** | Very large | n/a — bypasses the problem | Highest | Forks backend |
| **C: A + AST hints** | A + ~1 session | Same as A | Higher — hint-driven naming where structure survived | A + small hint thread |

**C is the right long-term answer if disassembly fidelity matters.**
Implement A first; C is purely additive — the relooper picks its
decomposition, and if a hint is consistent with the chosen
decomposition the hint informs scope naming and prefers source-
matching ordering. If the hint is inconsistent (because passes
transformed the CFG), the relooper ignores it. A doesn't need to
know about C, and C never has to be undone.

**B in any variant is not recommended.** The deep argument is that
the post-optimisation CFG IS the source of truth; trying to preserve
"the original" structure through optimisations fights the architecture
of the compiler and produces worse code for no upside the relooper
doesn't also provide.

## Open questions

1. **Where do scope labels come from?** The prog stream needs label
   identifiers so AJMPs can name their target scope. Today's
   wasm3AnalyzeCFG keys on boundary indices. Options: integer
   labels keyed off block IDs, or a per-function counter assigned
   at scope-open time. The latter is cleaner but requires the prog
   stream to carry the label.

2. **Should `ARESUMEPOINT` stay?** Today it's the basic-block
   boundary marker. After the relooper runs, structured ops
   (`ABlock`/`ALoop`/`AEnd`) carry the boundary information. The
   `ARESUMEPOINT` op can either be deleted (the boundary is implicit
   in the scope structure) or repurposed as "this block's stable
   ID anchor for relocations / debug info". Decide before
   touching the prog stream.

3. **Does `loopRotate` produce shapes the relooper handles cleanly?**
   Post-rotation, a `for cond { body }` becomes `body; if cond { goto
   body }`, with the back-edge from the conditional branch. The
   natural loop has the rotated-condition block as header — not the
   source `for` keyword. The relooper produces a `loop` whose body
   includes the conditional. Verify on a rotated loop in the spike.

4. **How does the relooper interact with `OpWasm3LoweredNilCheck`'s
   `if` emission?** Nil checks today emit `if; unreachable; end` —
   already an `AIf`/`AEnd` pair the encoder handles. The relooper
   pass must not treat the nil-check `if` as a CFG-level scope. The
   simplest rule: nil-check `if`s are emitted by `ssaGenValue` after
   the relooper has decided the CFG-level scopes, so they're inside
   a basic block's body and the relooper never sees them. Verify the
   ordering.

5. **What's the irreducible-CFG test program?** Probably a small Go
   function using `goto` that jumps into the middle of an existing
   loop. Need to author one and confirm `f.loopnest().hasIrreducible`
   fires. (Or determine whether Go's typechecker forbids the
   patterns that produce irreducibility, in which case the fallback
   is dead code we still want for safety.)

6. **`if/else` heuristic.** When does the relooper emit `if`/`else`
   vs. two `block`s + `br_if`? Both produce correct results; `if/else`
   is smaller when both arms are short. Pick a simple heuristic
   (e.g., always prefer `if` when both successors are dominated by
   the current block and the merge is also dominated) and revisit
   if size measurements suggest otherwise.

## Recommendation

Implement Approach A. Strategy 1 (pre-pass in `cmd/compile/internal/wasm3/`).
Reducible-only first; irreducible fallback in a follow-up commit. The
spike (step 1) is the first concrete deliverable and de-risks the
rest of the sequence — if the spike output validates and matches the
hand-derived structured CF for a simple `for` loop, the
productionisation steps are mechanical.

Schedule it **before Stage F**. Stage F adds itab method-dispatch
functions (each potentially a loop-bearing one-off) and would
otherwise compound the dispatch tax. Land the relooper, take the
size win, then begin Stage F on a base that doesn't carry the
trampoline residue forward.

The Stage G arc currently in flight (function values + `call_ref`)
can land in parallel — it touches the call ABI and obj encoder, not
the CFG reconstruction. The two arcs are orthogonal.

## Implementation status (2026-05-17)

Landed:

- `08ed73b1ce` — back-edge detection, natural-loop discovery, loop
  nesting (`cmd/compile/internal/wasm3/cfg.go`) + 9 unit tests on
  synthetic graphs.
- `c2a28e2f35` — `*ssa.Func` adapter, `computeReloopPlan`, shadow-
  run wiring in `ssaGenBlock` (GOWASM3_RELOOPER_DEBUG-gated dump).
- `c1f646edb1` — full per-boundary scope planner with branch-depth
  computation + 5 planner unit tests; plan is stashed per-LSym for
  the obj-side consumer.
- `14215294b7` — obj-encoder integration. `Wasm3StructuredPlan`
  surfaced in `cmd/internal/obj/wasm`; `encodeWasm3Body` consumes
  the plan when present and emits structured `block`/`loop`/`br`
  ops, falling back to the legacy `wasm3AnalyzeCFG` dispatch path
  when the plan is absent.
- `a0c812e70f` — skip `loopRotate` for the wasm3 backend (a
  header-at-end layout is not expressible as structured wasm CF).
- `1d537acc07` — block-scope open position respects loop nesting
  (open at the start of the smallest containing loop, or at
  function start). Eliminates the partial-overlap "stack not empty
  at end" bail.

Coverage as of `1d537acc07` on a full `hello.wasm` build:

  4684 / 5087 functions take the relooper path (92%)
   403 / 5087 functions bail to legacy

Size + br_table impact on the three audit programs:

  hello.wasm: 1 br_table → 0, 7015 → 6924 bytes (−1.3%)
  algos.wasm: 4 br_tables → 1, 8401 → 8254 bytes (−1.7%)
  run.wasm:   2 br_tables → 1, 7940 → 7845 bytes (−1.2%)

`runtime.printint` — the function that motivated the arc — now
takes the planned path. All M2 14/14 regression cases pass; the
audit programs produce correct output and pass `wasm-tools
validate`.

Remaining future work:

- **Bail-rate reduction.** The remaining 8% of functions hit one
  of the conservative bails (typically a branch with no recorded
  depth at trust-but-verify, in a complex nested-loop shape). Each
  pattern is its own analysis and fix; expanding coverage is
  incremental.
- **SCC-scoped dispatch fallback.** Today the bail path uses
  `wasm3AnalyzeCFG`'s whole-function dispatch trampoline. Per the
  design above, a properly-scoped fallback would emit dispatch
  only for the affected irreducible SCC, leaving the rest of the
  function structured. The Step 4 of the implementation sequence.
- **AST hints (Approach C).** Pure additive layer on top of the
  current relooper for disassembly fidelity. Deferred until the
  bail-rate work has stabilised the algorithm.
