// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm3

import (
	"reflect"
	"sort"
	"testing"
)

// adjacencyGraph is an in-memory cfgGraph used by the relooper tests.
// Dominators are precomputed by a textbook iterative algorithm — small
// graphs, so performance does not matter; what matters is that we are
// not exercising SSA's dominator pass and accidentally testing it
// instead of the relooper.
type adjacencyGraph struct {
	entry int32
	succ  map[int32][]int32
	pred  map[int32][]int32
	dom   map[int32]blockSet // dom[b] = set of blocks that dominate b
}

func newAdjacencyGraph(entry int32, edges [][2]int32) *adjacencyGraph {
	g := &adjacencyGraph{
		entry: entry,
		succ:  map[int32][]int32{},
		pred:  map[int32][]int32{},
	}
	seen := map[int32]bool{entry: true}
	for _, e := range edges {
		u, v := e[0], e[1]
		g.succ[u] = append(g.succ[u], v)
		g.pred[v] = append(g.pred[v], u)
		seen[u] = true
		seen[v] = true
	}
	g.dom = computeDominators(g, seen)
	return g
}

func (g *adjacencyGraph) Entry() int32 { return g.entry }
func (g *adjacencyGraph) Blocks() []int32 {
	out := make([]int32, 0, len(g.dom))
	for b := range g.dom {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
func (g *adjacencyGraph) Succs(b int32) []int32 { return g.succ[b] }
func (g *adjacencyGraph) Preds(b int32) []int32 { return g.pred[b] }
func (g *adjacencyGraph) Dominates(dom, b int32) bool {
	s, ok := g.dom[b]
	if !ok {
		return false
	}
	return s.has(dom)
}

// computeDominators runs the classic iterative dataflow algorithm
// over a small set of blocks. Initialises every block's dom set to
// "all blocks," initialises the entry's to "{entry}," then
// repeatedly intersects each block's predecessors' dom sets with
// {self} added until nothing changes.
func computeDominators(g *adjacencyGraph, all map[int32]bool) map[int32]blockSet {
	allSet := newBlockSet()
	for b := range all {
		allSet.add(b)
	}
	dom := map[int32]blockSet{}
	for b := range all {
		if b == g.entry {
			s := newBlockSet()
			s.add(b)
			dom[b] = s
		} else {
			// Copy allSet.
			s := newBlockSet()
			for x := range all {
				if allSet.has(x) {
					s.add(x)
				}
			}
			dom[b] = s
		}
	}
	changed := true
	for changed {
		changed = false
		for b := range all {
			if b == g.entry {
				continue
			}
			preds := g.pred[b]
			if len(preds) == 0 {
				continue
			}
			// Start with first pred's dom set.
			inter := copyBlockSet(dom[preds[0]])
			for _, p := range preds[1:] {
				inter = intersect(inter, dom[p])
			}
			// Add self.
			inter.add(b)
			if !equalBlockSet(inter, dom[b]) {
				dom[b] = inter
				changed = true
			}
		}
	}
	return dom
}

func copyBlockSet(s blockSet) blockSet {
	out := blockSet{bits: make([]uint64, len(s.bits))}
	copy(out.bits, s.bits)
	return out
}

func intersect(a, b blockSet) blockSet {
	n := len(a.bits)
	if len(b.bits) < n {
		n = len(b.bits)
	}
	out := blockSet{bits: make([]uint64, n)}
	for i := 0; i < n; i++ {
		out.bits[i] = a.bits[i] & b.bits[i]
	}
	return out
}

func equalBlockSet(a, b blockSet) bool {
	n := len(a.bits)
	if len(b.bits) > n {
		n = len(b.bits)
	}
	for i := 0; i < n; i++ {
		var av, bv uint64
		if i < len(a.bits) {
			av = a.bits[i]
		}
		if i < len(b.bits) {
			bv = b.bits[i]
		}
		if av != bv {
			return false
		}
	}
	return true
}

func TestFindBackEdges_Linear(t *testing.T) {
	// 1 → 2 → 3 (terminal). No back-edges.
	g := newAdjacencyGraph(1, [][2]int32{{1, 2}, {2, 3}})
	if got := findBackEdges(g); len(got) != 0 {
		t.Fatalf("linear CFG: got back-edges %+v, want none", got)
	}
}

func TestFindBackEdges_IfElse(t *testing.T) {
	// 1 → {2, 3}; 2 → 4; 3 → 4. Diamond, no back-edges.
	g := newAdjacencyGraph(1, [][2]int32{{1, 2}, {1, 3}, {2, 4}, {3, 4}})
	if got := findBackEdges(g); len(got) != 0 {
		t.Fatalf("diamond CFG: got back-edges %+v, want none", got)
	}
}

func TestFindBackEdges_SimpleLoop(t *testing.T) {
	// 1 → 2; 2 → 3; 3 → 2 (back-edge); 3 → 4 (exit).
	g := newAdjacencyGraph(1, [][2]int32{{1, 2}, {2, 3}, {3, 2}, {3, 4}})
	got := findBackEdges(g)
	want := []backEdge{{from: 3, to: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("simple loop: got back-edges %+v, want %+v", got, want)
	}
}

func TestFindBackEdges_NestedLoops(t *testing.T) {
	// 1 → 2 (outer header)
	// 2 → 3 (inner header)
	// 3 → 4
	// 4 → 3 (inner back-edge)
	// 4 → 5
	// 5 → 2 (outer back-edge)
	// 5 → 6 (exit)
	g := newAdjacencyGraph(1, [][2]int32{
		{1, 2}, {2, 3}, {3, 4}, {4, 3}, {4, 5}, {5, 2}, {5, 6},
	})
	got := findBackEdges(g)
	want := []backEdge{
		// Sorted by (to, from): the outer loop's back-edge has
		// header 2, the inner loop's has header 3, so outer first.
		{from: 5, to: 2}, // outer
		{from: 4, to: 3}, // inner
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nested loops: got back-edges %+v, want %+v", got, want)
	}
}

func TestFindNaturalLoops_SimpleLoop(t *testing.T) {
	// Same CFG as TestFindBackEdges_SimpleLoop.
	g := newAdjacencyGraph(1, [][2]int32{{1, 2}, {2, 3}, {3, 2}, {3, 4}})
	loops := findNaturalLoops(g, findBackEdges(g))
	if len(loops) != 1 {
		t.Fatalf("got %d loops, want 1", len(loops))
	}
	l := loops[0]
	if l.header != 2 {
		t.Errorf("header: got %d, want 2", l.header)
	}
	if l.body.len() != 2 || !l.body.has(2) || !l.body.has(3) {
		t.Errorf("body: got len=%d, want {2, 3}", l.body.len())
	}
}

func TestFindNaturalLoops_NestedLoops(t *testing.T) {
	g := newAdjacencyGraph(1, [][2]int32{
		{1, 2}, {2, 3}, {3, 4}, {4, 3}, {4, 5}, {5, 2}, {5, 6},
	})
	loops := findNaturalLoops(g, findBackEdges(g))
	if len(loops) != 2 {
		t.Fatalf("got %d loops, want 2", len(loops))
	}
	// Sorted by header ID ascending: outer (header 2) first, then
	// inner (header 3).
	outer, inner := loops[0], loops[1]
	if outer.header != 2 {
		t.Errorf("outer header: got %d, want 2", outer.header)
	}
	if inner.header != 3 {
		t.Errorf("inner header: got %d, want 3", inner.header)
	}
	// Outer body is {2, 3, 4, 5}.
	for _, b := range []int32{2, 3, 4, 5} {
		if !outer.body.has(b) {
			t.Errorf("outer body missing block %d", b)
		}
	}
	if outer.body.has(1) || outer.body.has(6) {
		t.Errorf("outer body contains a non-loop block: bits=%+v", outer.body)
	}
	// Inner body is {3, 4}.
	for _, b := range []int32{3, 4} {
		if !inner.body.has(b) {
			t.Errorf("inner body missing block %d", b)
		}
	}
	if inner.body.has(2) || inner.body.has(5) {
		t.Errorf("inner body should not contain blocks reachable from outer header but outside inner: bits=%+v", inner.body)
	}
}

func TestNestLoops_NestedLoops(t *testing.T) {
	g := newAdjacencyGraph(1, [][2]int32{
		{1, 2}, {2, 3}, {3, 4}, {4, 3}, {4, 5}, {5, 2}, {5, 6},
	})
	loops := findNaturalLoops(g, findBackEdges(g))
	nestLoops(loops)
	outer, inner := loops[0], loops[1]
	if outer.outer != nil {
		t.Errorf("outer.outer: got %+v, want nil", outer.outer)
	}
	if outer.depth != 1 {
		t.Errorf("outer.depth: got %d, want 1", outer.depth)
	}
	if inner.outer != outer {
		t.Errorf("inner.outer: got %+v, want outer", inner.outer)
	}
	if inner.depth != 2 {
		t.Errorf("inner.depth: got %d, want 2", inner.depth)
	}
}

func TestNestLoops_TwoSiblingLoops(t *testing.T) {
	// 1 → 2; 2 → 3; 3 → 2 (loop A back-edge); 3 → 4; 4 → 5; 5 → 4 (loop B back-edge); 5 → 6.
	g := newAdjacencyGraph(1, [][2]int32{
		{1, 2}, {2, 3}, {3, 2}, {3, 4}, {4, 5}, {5, 4}, {5, 6},
	})
	loops := findNaturalLoops(g, findBackEdges(g))
	nestLoops(loops)
	if len(loops) != 2 {
		t.Fatalf("got %d loops, want 2", len(loops))
	}
	for i, l := range loops {
		if l.outer != nil {
			t.Errorf("loop %d (header %d): outer should be nil for sibling loops, got %+v", i, l.header, l.outer)
		}
		if l.depth != 1 {
			t.Errorf("loop %d (header %d): depth %d, want 1", i, l.header, l.depth)
		}
	}
}

func TestBlockSet_Basics(t *testing.T) {
	var s blockSet
	if s.has(0) {
		t.Error("empty set: has(0) should be false")
	}
	if !s.add(0) {
		t.Error("add(0): should return true (newly added)")
	}
	if !s.has(0) {
		t.Error("after add(0): has(0) should be true")
	}
	if s.add(0) {
		t.Error("add(0) again: should return false (already present)")
	}
	if !s.add(127) {
		t.Error("add(127): should return true")
	}
	if !s.has(127) {
		t.Error("after add(127): has(127) should be true")
	}
	if s.has(128) {
		t.Error("has(128): should be false")
	}
	if got := s.len(); got != 2 {
		t.Errorf("len: got %d, want 2", got)
	}
}

// planTestSetup runs the full pipeline (back-edges → natural loops →
// nesting → emit plan) on an in-memory graph plus a layout. Returns
// the plan, or nil if the relooper bails. Tests use this to keep
// setup terse.
func planTestSetup(g cfgGraph, layout []int32) *emitPlan {
	backs := findBackEdges(g)
	loops := findNaturalLoops(g, backs)
	nestLoops(loops)
	rp := &reloopPlan{loops: loops}
	// Approximate irreducibility check (mirrors computeReloopPlan).
	for _, l := range loops {
		for _, id := range g.Blocks() {
			if id == l.header || !l.body.has(id) {
				continue
			}
			for _, p := range g.Preds(id) {
				if !l.body.has(p) {
					rp.hasIrreducible = true
					break
				}
			}
			if rp.hasIrreducible {
				break
			}
		}
		if rp.hasIrreducible {
			break
		}
	}
	return computeEmitPlanFor(g, layout, rp)
}

func TestEmitPlan_Linear(t *testing.T) {
	// 1 → 2 → 3 (terminal). No loops, no merge points; the plan
	// should be empty (no scopes anywhere).
	g := newAdjacencyGraph(1, [][2]int32{{1, 2}, {2, 3}})
	plan := planTestSetup(g, []int32{1, 2, 3})
	if plan == nil {
		t.Fatal("plan should not be nil for linear CFG")
	}
	for i, bop := range plan.perBoundary {
		if bop.closes != 0 || len(bop.opens) != 0 {
			t.Errorf("boundary %d: got closes=%d opens=%d, want empty", i, bop.closes, len(bop.opens))
		}
	}
	if len(plan.branchDepth) != 0 {
		t.Errorf("branchDepth should be empty for linear CFG, got %+v", plan.branchDepth)
	}
}

func TestEmitPlan_IfElseMerge(t *testing.T) {
	// 1 → {2, 3}; 2 → 4; 3 → 4. Diamond. Layout: 1, 2, 3, 4.
	// Block 1's layout-adjacent successor is 2, so 1→2 falls through
	// but 1→3 needs a branch. Block 2's layout-adjacent successor
	// would be 3, so 2→4 needs a branch (4 is past 3). Block 3's
	// adjacent successor is 4, so 3→4 falls through.
	// Two block scopes: one for 3 (endsAt=2) and one for 4 (endsAt=3).
	// Both open at boundary 0 (block 1's idom — which is block 1
	// itself, the entry, at layout position 0).
	g := newAdjacencyGraph(1, [][2]int32{{1, 2}, {1, 3}, {2, 4}, {3, 4}})
	plan := planTestSetup(g, []int32{1, 2, 3, 4})
	if plan == nil {
		t.Fatal("plan should not be nil for diamond CFG")
	}
	if len(plan.perBoundary) != 5 {
		t.Fatalf("perBoundary len: got %d, want 5", len(plan.perBoundary))
	}
	// Boundary 0: opens two block scopes, outermost (endsAt=3) first.
	if got := len(plan.perBoundary[0].opens); got != 2 {
		t.Fatalf("boundary 0: opens len %d, want 2", got)
	}
	if op := plan.perBoundary[0].opens[0]; op.kind != scopeBlock || op.target != 4 || op.endsAt != 3 {
		t.Errorf("boundary 0 opens[0] (outermost): got %+v, want scopeBlock target=4 endsAt=3", op)
	}
	if op := plan.perBoundary[0].opens[1]; op.kind != scopeBlock || op.target != 3 || op.endsAt != 2 {
		t.Errorf("boundary 0 opens[1] (innermost): got %+v, want scopeBlock target=3 endsAt=2", op)
	}
	// Boundary 2: close inner block scope (for 3).
	if plan.perBoundary[2].closes != 1 {
		t.Errorf("boundary 2: closes %d, want 1", plan.perBoundary[2].closes)
	}
	// Boundary 3: close outer block scope (for 4).
	if plan.perBoundary[3].closes != 1 {
		t.Errorf("boundary 3: closes %d, want 1", plan.perBoundary[3].closes)
	}
	// Branch 1→3: depth 0 (innermost is block scope for 3 at sidx=1).
	if d, ok := plan.branchDepth[branchKey{from: 1, to: 3}]; !ok || d != 0 {
		t.Errorf("branchDepth[1→3]: got %d ok=%v, want 0 ok=true", d, ok)
	}
	// Branch 2→4: depth 1 (scope for 4 is at sidx=0, scope for 3
	// is at sidx=1 above it; depth = 2-1-0 = 1).
	if d, ok := plan.branchDepth[branchKey{from: 2, to: 4}]; !ok || d != 1 {
		t.Errorf("branchDepth[2→4]: got %d ok=%v, want 1 ok=true", d, ok)
	}
}

func TestEmitPlan_SimpleLoop(t *testing.T) {
	// 1 → 2 → 3 → 2 (back-edge); 3 → 4 (exit). Layout: 1, 2, 3, 4.
	// Loop body: {2, 3}, header 2.
	g := newAdjacencyGraph(1, [][2]int32{{1, 2}, {2, 3}, {3, 2}, {3, 4}})
	plan := planTestSetup(g, []int32{1, 2, 3, 4})
	if plan == nil {
		t.Fatal("plan should not be nil for simple loop")
	}
	// Loop scope opens at boundary 1 (before block 2), closes at
	// boundary 3 (before block 4 — after block 3).
	if got := len(plan.perBoundary[1].opens); got != 1 {
		t.Fatalf("boundary 1: opens len %d, want 1", got)
	}
	if op := plan.perBoundary[1].opens[0]; op.kind != scopeLoop || op.target != 2 {
		t.Errorf("boundary 1 open: got %+v, want scopeLoop target=2", op)
	}
	if got := plan.perBoundary[3].closes; got != 1 {
		t.Errorf("boundary 3: closes %d, want 1", got)
	}
	// Branch 3→2 (back-edge): depth 0 (loop is innermost).
	if d, ok := plan.branchDepth[branchKey{from: 3, to: 2}]; !ok || d != 0 {
		t.Errorf("branchDepth[3→2]: got %d ok=%v, want 0 ok=true", d, ok)
	}
}

func TestEmitPlan_NestedLoops(t *testing.T) {
	// 1 → 2 → 3 → 4 → 3 (inner back-edge); 4 → 5 → 2 (outer back-edge);
	// 5 → 6 (exit). Layout: 1, 2, 3, 4, 5, 6.
	// Outer loop: header 2, body {2,3,4,5}.
	// Inner loop: header 3, body {3,4}.
	g := newAdjacencyGraph(1, [][2]int32{
		{1, 2}, {2, 3}, {3, 4}, {4, 3}, {4, 5}, {5, 2}, {5, 6},
	})
	plan := planTestSetup(g, []int32{1, 2, 3, 4, 5, 6})
	if plan == nil {
		t.Fatal("plan should not be nil for nested loops")
	}
	// At boundary 1: open outer loop (target=2, endsAt=5).
	// At boundary 2: open inner loop (target=3, endsAt=4).
	// At boundary 4: close inner loop.
	// At boundary 5: close outer loop.
	if got := len(plan.perBoundary[1].opens); got != 1 {
		t.Errorf("boundary 1: opens len %d, want 1", got)
	}
	if got := len(plan.perBoundary[2].opens); got != 1 {
		t.Errorf("boundary 2: opens len %d, want 1", got)
	}
	if plan.perBoundary[4].closes != 1 {
		t.Errorf("boundary 4: closes %d, want 1", plan.perBoundary[4].closes)
	}
	if plan.perBoundary[5].closes != 1 {
		t.Errorf("boundary 5: closes %d, want 1", plan.perBoundary[5].closes)
	}
	// Back-edge 4→3 (inner): depth 0 (inner is innermost).
	if d, ok := plan.branchDepth[branchKey{from: 4, to: 3}]; !ok || d != 0 {
		t.Errorf("branchDepth[4→3]: got %d ok=%v, want 0 ok=true", d, ok)
	}
	// Back-edge 5→2 (outer): depth 0 (inner has been closed by
	// boundary 5; at block 5's terminator only outer remains open).
	if d, ok := plan.branchDepth[branchKey{from: 5, to: 2}]; !ok || d != 0 {
		t.Errorf("branchDepth[5→2]: got %d ok=%v, want 0 ok=true", d, ok)
	}
}

func TestEmitPlan_IrreducibleBail(t *testing.T) {
	// 1 → 2; 1 → 3; 2 → 3; 3 → 2. SCC {2, 3} with two entries from
	// outside (1→2 and 1→3) — irreducible.
	g := newAdjacencyGraph(1, [][2]int32{{1, 2}, {1, 3}, {2, 3}, {3, 2}})
	plan := planTestSetup(g, []int32{1, 2, 3})
	if plan != nil {
		t.Errorf("plan should be nil for irreducible CFG, got %+v", plan)
	}
}
