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
