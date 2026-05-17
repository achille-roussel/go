// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm3

// Relooper analysis for the wasm3 backend.
//
// Reconstructs structured wasm control flow (block / loop / if / br)
// from the SSA basic-block graph. Replaces the obj-encoder's
// loop+br_table dispatch trampoline that fires on every function with
// a back-edge today (see cmd/internal/obj/wasm/wasm3obj.go).
//
// This file holds only the structural analysis — back-edge detection
// and natural-loop discovery. The scope planner that turns the loop
// tree into per-block open/close decisions, and the emission pass
// that rewrites the prog stream, land in follow-up commits.
//
// See doc/wasm3-m3-relooper-design.md for the overall plan.

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"sync"

	"cmd/compile/internal/ssa"
	"cmd/internal/obj/wasm"
)

// cfgGraph is the minimal block-graph abstraction the relooper needs.
// *ssa.Func satisfies it via the ssaCFGGraph adapter; tests construct
// in-memory adjacencyGraph instances for unit-level coverage without
// dragging in SSA's internal test helpers.
type cfgGraph interface {
	// Entry returns the entry block's ID. Always a block ID, never -1.
	Entry() int32

	// Blocks returns every reachable block's ID in some stable order.
	// The order should be consistent across calls but does not have to
	// be reverse-postorder.
	Blocks() []int32

	// Succs returns the CFG successors of b. The order matters only
	// for two-successor blocks (used to know which side an `if` /
	// `else` arm corresponds to); for one-successor or terminal
	// blocks any order is fine.
	Succs(b int32) []int32

	// Preds returns the CFG predecessors of b.
	Preds(b int32) []int32

	// Dominates reports whether dom dominates b. A block dominates
	// itself.
	Dominates(dom, b int32) bool
}

// backEdge is a CFG edge u→v where v dominates u. Equivalently:
// v is a natural-loop header and u is one of its (possibly several)
// back-edge sources.
type backEdge struct {
	from, to int32
}

// findBackEdges returns every back-edge in g. The order is sorted by
// (to, from) so the result is deterministic across runs.
func findBackEdges(g cfgGraph) []backEdge {
	var edges []backEdge
	for _, u := range g.Blocks() {
		for _, v := range g.Succs(u) {
			if g.Dominates(v, u) {
				edges = append(edges, backEdge{from: u, to: v})
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].to != edges[j].to {
			return edges[i].to < edges[j].to
		}
		return edges[i].from < edges[j].from
	})
	return edges
}

// naturalLoop is a header block plus the set of blocks reachable from
// the header that can reach at least one of the back-edge sources
// without going through the header. By construction the header is in
// the body.
//
// A header may have multiple back-edges from different sources; the
// natural loop is the union over all of them. The body set is a
// minimal cover — additional blocks reachable from the header but
// outside the loop (e.g. exit-block successors) are not included.
type naturalLoop struct {
	header int32
	body   blockSet

	// outer is the smallest other loop whose body strictly contains
	// this loop's header. nil for outermost loops.
	outer *naturalLoop

	// depth is the loop's nesting depth. Outermost loops have depth
	// 1; each nested level adds one.
	depth int
}

// findNaturalLoops returns all natural loops in g, given the back-
// edges (see findBackEdges). Result order is deterministic: loops are
// sorted by header ID ascending.
//
// Irreducible CFGs (multi-entry SCCs from goto-into-loop patterns)
// are not represented here — the back-edge classification still picks
// up the edges whose target dominates the source, but the body
// computation does not reach blocks that are part of an irreducible
// region with a separate entry. Detecting irreducibility and falling
// back to a dispatch nest for the affected region is the responsibility
// of a later pass (per the design doc).
func findNaturalLoops(g cfgGraph, backs []backEdge) []*naturalLoop {
	// Group back-edges by target header.
	byHeader := make(map[int32][]int32)
	for _, e := range backs {
		byHeader[e.to] = append(byHeader[e.to], e.from)
	}

	// Headers in deterministic order.
	var headers []int32
	for h := range byHeader {
		headers = append(headers, h)
	}
	slices.Sort(headers)

	loops := make([]*naturalLoop, 0, len(headers))
	for _, h := range headers {
		body := computeLoopBody(g, h, byHeader[h])
		loops = append(loops, &naturalLoop{header: h, body: body})
	}
	return loops
}

// computeLoopBody returns the set of blocks in the natural loop with
// the given header and back-edge sources. The body is computed by a
// reverse search: start from each source and walk predecessors until
// the header is reached, collecting every block visited. Stops at the
// header — the header itself is included but its predecessors outside
// the loop are not followed.
func computeLoopBody(g cfgGraph, header int32, sources []int32) blockSet {
	body := newBlockSet()
	body.add(header)
	// stack of blocks whose predecessors still need to be walked.
	var stack []int32
	for _, s := range sources {
		if body.add(s) {
			stack = append(stack, s)
		}
	}
	for len(stack) > 0 {
		b := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if b == header {
			continue
		}
		for _, p := range g.Preds(b) {
			if body.add(p) {
				stack = append(stack, p)
			}
		}
	}
	return body
}

// nestLoops assigns each loop an `outer` pointer (its smallest
// strictly-containing loop) and a `depth`. The relation is computed
// from body-containment: loop A is outer of B iff A's body strictly
// contains B's header.
//
// Result: depth is set on every loop; outer is set to nil for
// outermost loops and to the immediate parent otherwise.
func nestLoops(loops []*naturalLoop) {
	// O(n²) — fine in practice because per-function loop counts are
	// small (single digits for the audit programs; tens for the
	// loopiest stdlib code).
	for _, inner := range loops {
		var best *naturalLoop
		for _, outer := range loops {
			if outer == inner || !outer.body.has(inner.header) {
				continue
			}
			// inner.header is in outer.body. Keep the smallest such
			// outer (the one whose body contains the fewest other
			// loops' headers — i.e. the most-nested-still-containing).
			if best == nil || best.body.has(outer.header) {
				best = outer
			}
		}
		inner.outer = best
	}
	for _, l := range loops {
		d := 1
		for x := l.outer; x != nil; x = x.outer {
			d++
		}
		l.depth = d
	}
}

// blockSet is a small bitmap-backed set of block IDs. Block IDs are
// dense small integers in practice (the SSA numbering), so a bitmap
// is tighter and faster than a map[int32]bool.
type blockSet struct {
	bits []uint64
}

func newBlockSet() blockSet { return blockSet{} }

// add inserts b into the set. Reports whether b was newly added.
func (s *blockSet) add(b int32) bool {
	w, bit := uint64(b)>>6, uint64(b)&63
	if int(w) >= len(s.bits) {
		grown := make([]uint64, w+1)
		copy(grown, s.bits)
		s.bits = grown
	}
	mask := uint64(1) << bit
	if s.bits[w]&mask != 0 {
		return false
	}
	s.bits[w] |= mask
	return true
}

// has reports whether b is in the set.
func (s *blockSet) has(b int32) bool {
	w, bit := uint64(b)>>6, uint64(b)&63
	if int(w) >= len(s.bits) {
		return false
	}
	return s.bits[w]&(uint64(1)<<bit) != 0
}

// len returns the number of elements in the set.
func (s *blockSet) len() int {
	n := 0
	for _, w := range s.bits {
		for w != 0 {
			w &= w - 1
			n++
		}
	}
	return n
}

// ssaCFGGraph wraps an *ssa.Func as a cfgGraph for the relooper. The
// adapter holds a precomputed []*ssa.Block keyed by ID for O(1)
// successor / predecessor lookups, since *ssa.Block already carries
// its Succs / Preds slices.
type ssaCFGGraph struct {
	f      *ssa.Func
	sdom   ssa.SparseTree
	byID   []*ssa.Block // byID[id] == block with that ID; nil for dead/recycled IDs
	bIDs   []int32      // every live block's ID in f.Blocks order (deterministic)
}

func newSSACFGGraph(f *ssa.Func) *ssaCFGGraph {
	g := &ssaCFGGraph{f: f, sdom: f.Sdom()}
	maxID := int32(0)
	for _, b := range f.Blocks {
		if int32(b.ID) > maxID {
			maxID = int32(b.ID)
		}
	}
	g.byID = make([]*ssa.Block, maxID+1)
	g.bIDs = make([]int32, 0, len(f.Blocks))
	for _, b := range f.Blocks {
		g.byID[b.ID] = b
		g.bIDs = append(g.bIDs, int32(b.ID))
	}
	return g
}

func (g *ssaCFGGraph) Entry() int32 { return int32(g.f.Entry.ID) }

func (g *ssaCFGGraph) Blocks() []int32 { return g.bIDs }

func (g *ssaCFGGraph) Succs(b int32) []int32 {
	blk := g.byID[b]
	if blk == nil {
		return nil
	}
	out := make([]int32, len(blk.Succs))
	for i, e := range blk.Succs {
		out[i] = int32(e.Block().ID)
	}
	return out
}

func (g *ssaCFGGraph) Preds(b int32) []int32 {
	blk := g.byID[b]
	if blk == nil {
		return nil
	}
	out := make([]int32, len(blk.Preds))
	for i, e := range blk.Preds {
		out[i] = int32(e.Block().ID)
	}
	return out
}

func (g *ssaCFGGraph) Dominates(dom, b int32) bool {
	d := g.byID[dom]
	x := g.byID[b]
	if d == nil || x == nil {
		return false
	}
	// SparseTree.IsAncestorEq(a, b) is true iff a dominates b
	// (a block dominates itself).
	return g.sdom.IsAncestorEq(d, x)
}

// reloopPlan is the result of running the structural analysis on a
// function. Later commits will extend it with per-block scope
// open/close decisions; for now it carries only the loop tree, which
// is enough to validate the analysis runs against real compiled
// functions without crashes.
type reloopPlan struct {
	loops          []*naturalLoop
	hasIrreducible bool
}

// computeReloopPlan runs the full structural analysis on f. Safe to
// call on any *ssa.Func, including ones with no back-edges (returns
// an empty plan).
//
// Irreducibility detection is approximate at this stage — we flag a
// function as irreducible if any block in a back-edge target's
// natural loop has predecessors outside the body (typically a
// multi-entry SCC). The dispatch-fallback wiring lands in a later
// commit; for now the flag is informational only.
func computeReloopPlan(f *ssa.Func) *reloopPlan {
	g := newSSACFGGraph(f)
	backs := findBackEdges(g)
	loops := findNaturalLoops(g, backs)
	nestLoops(loops)

	// Approximate irreducibility check: for each loop, walk its body
	// and look for a non-header block whose predecessor set includes
	// at least one block outside the body. That is the canonical
	// signature of a multi-entry SCC (e.g. goto into a loop body).
	hasIrreducible := false
	for _, l := range loops {
		for _, id := range g.Blocks() {
			if id == l.header || !l.body.has(id) {
				continue
			}
			for _, p := range g.Preds(id) {
				if !l.body.has(p) {
					hasIrreducible = true
					break
				}
			}
			if hasIrreducible {
				break
			}
		}
		if hasIrreducible {
			break
		}
	}

	return &reloopPlan{loops: loops, hasIrreducible: hasIrreducible}
}

// scopeKind tags the wasm structure type of a relooper scope.
type scopeKind int

const (
	// scopeLoop is a wasm `loop` scope wrapping a natural loop's body.
	// Branches targeting a scopeLoop are back-edges that re-enter the
	// loop's header.
	scopeLoop scopeKind = iota
	// scopeBlock is a wasm `block` scope wrapping a region whose end
	// is a forward-branch merge point. Branches targeting a
	// scopeBlock are forward edges that exit the block at its `end`.
	scopeBlock
)

// scope describes one open structured-control-flow scope on the
// relooper's stack.
type scope struct {
	kind   scopeKind
	target int32 // for scopeLoop: the loop header block ID. For scopeBlock: the block ID immediately after the scope's `end`.
	endsAt int   // boundary index at which this scope's `end` op is emitted
}

// boundaryOp is the scope manipulation that happens at one boundary.
// Closes are applied first (innermost first, popping the stack);
// opens are applied next (outermost first, pushed in order onto the
// stack). Both lists may be empty.
type boundaryOp struct {
	closes int     // number of scopes to pop before this boundary
	opens  []scope // scopes to push at this boundary, outermost first
}

// branchKey identifies a CFG edge for the branchDepth map.
type branchKey struct {
	from, to int32
}

// emitPlan is the full relooper output for one function. The
// emission pipeline (in ssa.go and wasm3obj.go) walks the prog
// stream and consults perBoundary at every ARESUMEPOINT, and
// consults branchDepth at every AJMP, instead of running its own
// CFG analysis.
//
// A nil emitPlan signals "fall back to the legacy wasm3AnalyzeCFG
// path." computeEmitPlan returns nil for functions the relooper
// cannot handle (irreducible CFGs, non-contiguous loop bodies, etc.)
// so the legacy path remains available as a safety net while the
// relooper matures.
type emitPlan struct {
	// perBoundary[i] is the scope op applied at boundary i, for i in
	// [0, numBlocks]. Boundary 0 is at function start (before the
	// first prog of block layout[0]); boundary numBlocks is at
	// function end (after the last prog of block layout[numBlocks-1]).
	perBoundary []boundaryOp

	// branchDepth maps each CFG edge to the wasm br-depth at the
	// source block's terminator. Computed once at plan time so the
	// emission pipeline does not need to maintain its own scope
	// stack: each AJMP looks up its depth directly.
	branchDepth map[branchKey]int

	// boundaryOfBlock maps each block ID to its layout index, which
	// is the boundary index of its start (= boundary just after its
	// preceding ARESUMEPOINT).
	boundaryOfBlock map[int32]int
}

// computeEmitPlan returns the full emission plan for f, or nil if
// the relooper cannot handle the function. Conditions that trigger
// the bail-to-legacy path:
//   - The CFG is irreducible.
//   - A natural loop's body is non-contiguous in layout order.
//   - A natural loop's header is not at the first layout position of
//     its body.
//
// These are conservative checks; relaxing them is future work
// (block reordering pre-pass; SCC-scoped dispatch fallback).
func computeEmitPlan(f *ssa.Func, rp *reloopPlan) *emitPlan {
	layout := make([]int32, len(f.Blocks))
	for i, b := range f.Blocks {
		layout[i] = int32(b.ID)
	}
	return computeEmitPlanFor(newSSACFGGraph(f), layout, rp)
}

// computeEmitPlanFor is the testable core of the planner: it operates
// on a generic cfgGraph plus an explicit layout order. The SSA
// adapter at the call site supplies the layout from f.Blocks; tests
// supply hand-built adjacency graphs and any layout order.
func computeEmitPlanFor(g cfgGraph, layout []int32, rp *reloopPlan) *emitPlan {
	if rp.hasIrreducible {
		return nil
	}

	layoutIdx := make(map[int32]int, len(layout))
	for i, b := range layout {
		layoutIdx[b] = i
	}

	// Verify loop body contiguity in layout and header-at-start.
	type loopExtent struct {
		first, last int // layout index of body's first / last block
	}
	loopExtents := make(map[int32]loopExtent, len(rp.loops))
	for _, L := range rp.loops {
		first, last := len(layout), -1
		for i, b := range layout {
			if L.body.has(b) {
				if i < first {
					first = i
				}
				if i > last {
					last = i
				}
			}
		}
		for i := first; i <= last; i++ {
			if !L.body.has(layout[i]) {
				return nil
			}
		}
		if layout[first] != L.header {
			return nil
		}
		loopExtents[L.header] = loopExtent{first: first, last: last}
	}

	// For each block T, decide whether it needs a wasm `block` scope
	// opened ending at T. Yes iff T has at least one forward
	// predecessor that does not layout-fall-through to T.
	//
	// A predecessor P is a forward predecessor iff layoutIdx[P] <
	// layoutIdx[T] AND P→T is not a back-edge (T does not dominate
	// P). P fall-throughs to T iff P is layout-adjacent to T
	// (layoutIdx[P] == layoutIdx[T]-1).
	needsBlockScope := make(map[int32]bool)
	for i, T := range layout {
		if i == 0 {
			continue
		}
		for _, P := range g.Preds(T) {
			pi, ok := layoutIdx[P]
			if !ok || pi >= i {
				continue
			}
			if g.Dominates(T, P) {
				continue // back-edge
			}
			if pi != i-1 {
				needsBlockScope[T] = true
				break
			}
			// pi == i-1: layout-adjacent forward predecessor. May or
			// may not need a scope depending on whether P emits an
			// explicit branch to T or falls through. ssaGenBlock
			// falls through when P's `next` parameter equals T,
			// which is the case when T is layout-adjacent AND T is
			// not P's only "non-natural" successor. Conservative:
			// only mark scope-needed if some OTHER predecessor of T
			// is at a different position. The current pi-iteration
			// continues to look at other preds, so the break above
			// catches the "needs scope" case.
		}
	}

	// For each block T needing a block scope, compute the open
	// position: the layout index of T's immediate dominator (the
	// latest block before T in layout that dominates T). All forward
	// predecessors of T are dominated by T.idom (definition of
	// dominators), so they are all at or after the idom in layout
	// for reducible CFGs.
	blockScopeOpenAt := make(map[int32]int, len(needsBlockScope))
	for T := range needsBlockScope {
		ti := layoutIdx[T]
		idomPos := 0
		for j := ti - 1; j >= 0; j-- {
			if g.Dominates(layout[j], T) {
				idomPos = j
				break
			}
		}
		blockScopeOpenAt[T] = idomPos
	}

	// Walk boundaries and build the plan.
	numBoundaries := len(layout) + 1
	plan := &emitPlan{
		perBoundary:     make([]boundaryOp, numBoundaries),
		branchDepth:     make(map[branchKey]int),
		boundaryOfBlock: layoutIdx,
	}

	var stack []scope

	for i := 0; i <= len(layout); i++ {
		bop := &plan.perBoundary[i]

		// Close scopes whose end is at this boundary, innermost first.
		for len(stack) > 0 && stack[len(stack)-1].endsAt == i {
			stack = stack[:len(stack)-1]
			bop.closes++
		}

		// Collect scopes opening at this boundary, sort outermost-first
		// by ending position (largest endsAt first), then push.
		var opens []scope
		for _, L := range rp.loops {
			ext := loopExtents[L.header]
			if ext.first == i {
				opens = append(opens, scope{
					kind:   scopeLoop,
					target: L.header,
					endsAt: ext.last + 1,
				})
			}
		}
		for T, openAt := range blockScopeOpenAt {
			if openAt == i {
				opens = append(opens, scope{
					kind:   scopeBlock,
					target: T,
					endsAt: layoutIdx[T],
				})
			}
		}
		sort.Slice(opens, func(a, b int) bool {
			if opens[a].endsAt != opens[b].endsAt {
				return opens[a].endsAt > opens[b].endsAt
			}
			// Tie-break deterministically by (kind, target).
			if opens[a].kind != opens[b].kind {
				return opens[a].kind < opens[b].kind
			}
			return opens[a].target < opens[b].target
		})

		for _, op := range opens {
			stack = append(stack, op)
		}
		bop.opens = opens

		// Compute branch depths for the block whose terminator runs
		// at boundary i+1 (i.e., block layout[i]'s terminator).
		if i < len(layout) {
			srcBlock := layout[i]
			for _, succ := range g.Succs(srcBlock) {
				depth := -1
				for sidx := len(stack) - 1; sidx >= 0; sidx-- {
					s := stack[sidx]
					if s.target == succ {
						// Loop target = back-edge to header; block
						// target = forward branch to scope's end.
						depth = len(stack) - 1 - sidx
						break
					}
				}
				if depth >= 0 {
					plan.branchDepth[branchKey{from: srcBlock, to: succ}] = depth
				}
			}
		}
	}

	if len(stack) != 0 {
		// Some scope was not properly closed — typically because a
		// block scope opened inside a loop ends past the loop's end
		// (improper nesting). Bail to legacy; the nesting-fix is
		// future work (open the block scope outside the loop when
		// the target is outside the loop too).
		return nil
	}

	// Trust-but-verify: every CFG edge that is NOT a layout
	// fall-through must have a recorded branch depth. If we missed
	// one — typically because the target lives in a multi-entry
	// SCC that escaped the natural-loop / hasIrreducible detection
	// — bail to legacy rather than emit broken wasm.
	for _, from := range layout {
		fi := layoutIdx[from]
		for _, to := range g.Succs(from) {
			ti, ok := layoutIdx[to]
			if !ok {
				return nil
			}
			if ti == fi+1 {
				continue // layout-adjacent fall-through; no branch needed
			}
			if _, ok := plan.branchDepth[branchKey{from: from, to: to}]; !ok {
				return nil
			}
		}
	}

	return plan
}

// funcPlan is the bundled output of the relooper for one function:
// the structural analysis (loops + irreducibility flag) plus the
// emission plan. emit may be nil when the relooper bails on the
// function — the obj-encoder then falls back to the legacy
// wasm3AnalyzeCFG path.
type funcPlan struct {
	reloop *reloopPlan
	emit   *emitPlan
}

// reloopPlans caches the computed plan per *ssa.Func so the analysis
// runs at most once per function. The map is populated lazily on
// first call to planForFunc; subsequent calls in the same compilation
// reuse the cached result.
var reloopPlans sync.Map // *ssa.Func -> *funcPlan

// toObjPlan converts the compiler-internal emitPlan into the slim
// wasm.Wasm3StructuredPlan the obj-encoder consumes. Performed once
// per function at planForFunc-cache-miss time so the obj-side does
// not need to depend on the internal scope/branchKey/etc. types.
func (ep *emitPlan) toObjPlan() *wasm.Wasm3StructuredPlan {
	if ep == nil {
		return nil
	}
	// Recover the layout from boundaryOfBlock (which is block ID →
	// layout index). Layout is the inverse.
	maxIdx := 0
	for _, idx := range ep.boundaryOfBlock {
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	layout := make([]int32, maxIdx+1)
	for bid, idx := range ep.boundaryOfBlock {
		layout[idx] = bid
	}

	out := &wasm.Wasm3StructuredPlan{
		PerBoundary: make([]wasm.Wasm3BoundaryOp, len(ep.perBoundary)),
		Layout:      layout,
	}
	for i, bop := range ep.perBoundary {
		out.PerBoundary[i].Closes = bop.closes
		if len(bop.opens) > 0 {
			out.PerBoundary[i].Opens = make([]wasm.Wasm3ScopeOpen, len(bop.opens))
			for j, op := range bop.opens {
				kind := wasm.Wasm3ScopeKindLoop
				if op.kind == scopeBlock {
					kind = wasm.Wasm3ScopeKindBlock
				}
				out.PerBoundary[i].Opens[j] = wasm.Wasm3ScopeOpen{
					Kind:   kind,
					Target: op.target,
				}
			}
		}
	}
	return out
}

// planForFunc returns the relooper plan for f, computing it the
// first time and caching the result. Safe for concurrent use from
// multiple compilation goroutines.
func planForFunc(f *ssa.Func) *funcPlan {
	if cached, ok := reloopPlans.Load(f); ok {
		return cached.(*funcPlan)
	}
	rp := computeReloopPlan(f)
	ep := computeEmitPlan(f, rp)
	fp := &funcPlan{reloop: rp, emit: ep}
	actual, loaded := reloopPlans.LoadOrStore(f, fp)
	fp = actual.(*funcPlan)
	if !loaded {
		if ep != nil && f.OwnAux != nil && f.OwnAux.Fn != nil {
			wasm.Wasm3EmitPlans.Store(f.OwnAux.Fn, ep.toObjPlan())
		}
		if reloopDebug {
			dumpReloopPlan(f.Name, newSSACFGGraph(f), fp)
		}
	}
	return fp
}

// reloopDebug is enabled by GOWASM3_RELOOPER_DEBUG=1 in the
// environment. When set, every function the compiler processes gets
// its loop tree dumped to stderr — the early-stage validation that
// the analysis behaves the same on real compiled functions as on the
// unit-test CFGs.
var reloopDebug = os.Getenv("GOWASM3_RELOOPER_DEBUG") == "1"

// dumpReloopPlan prints a human-readable summary of plan to stderr.
// Format: one line per function ("name: N loops, irreducible=Y/N,
// emit=Y/N"), followed by one line per loop ("  loop header=bK
// depth=D body=bA,bB,..."). Used only when reloopDebug is set.
func dumpReloopPlan(funcName string, g cfgGraph, plan *funcPlan) {
	emitStatus := "yes"
	if plan.emit == nil {
		emitStatus = "no (bail to legacy)"
	}
	fmt.Fprintf(os.Stderr, "wasm3-relooper: %s: %d loops, irreducible=%v, emit=%s\n",
		funcName, len(plan.reloop.loops), plan.reloop.hasIrreducible, emitStatus)
	for _, l := range plan.reloop.loops {
		var members []int32
		for _, id := range g.Blocks() {
			if l.body.has(id) {
				members = append(members, id)
			}
		}
		fmt.Fprintf(os.Stderr, "wasm3-relooper:   loop header=b%d depth=%d body=%v\n",
			l.header, l.depth, members)
	}
}
