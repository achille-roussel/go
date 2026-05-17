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
	"slices"
	"sort"
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
