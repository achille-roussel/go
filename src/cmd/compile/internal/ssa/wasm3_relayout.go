// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

// wasm3RelayoutForRelooper reorders f.Blocks so every natural loop's
// body forms a contiguous run starting at the loop header. The wasm3
// relooper (cmd/compile/internal/wasm3/cfg.go) requires this layout
// to emit structured wasm `loop` scopes; the default Go layout pass
// optimises for fall-through and can interleave non-body blocks
// among body blocks, or place a non-header body block at the body's
// layout start, both of which are incompatible with the relooper.
//
// Algorithm: process loops innermost-first. For each loop L, gather
// L's body blocks (b where loopnest.b2l[b.ID] is L or any loop
// nested inside L) in their current layout order and move them into
// one contiguous run anchored at L.header's current layout position.
// Inner loops are already contiguous when an outer loop is
// processed, so they move as a single chunk.
//
// Bails (no-op) on irreducible loop nests — the relooper bails on
// those anyway, and the underlying loopnest analysis warns it does
// not track them precisely.
func wasm3RelayoutForRelooper(f *Func) {
	ln := f.loopnest()
	if ln.hasIrreducible {
		return
	}
	if len(ln.loops) == 0 {
		return
	}

	// "Belongs to L" predicate: block b is in L's body iff
	// loopnest.b2l[b.ID] is L or a descendant of L in the loop tree.
	belongsTo := func(b *Block, L *loop) bool {
		bl := ln.b2l[b.ID]
		for bl != nil {
			if bl == L {
				return true
			}
			bl = bl.outer
		}
		return false
	}

	// Process loops innermost-first. Two loops at the same depth are
	// processed in header-ID order for determinism.
	order := append([]*loop(nil), ln.loops...)
	sortLoops := func() {
		for i := 1; i < len(order); i++ {
			for j := i; j > 0; j-- {
				if order[j-1].depth < order[j].depth ||
					(order[j-1].depth == order[j].depth && order[j-1].header.ID > order[j].header.ID) {
					order[j-1], order[j] = order[j], order[j-1]
				} else {
					break
				}
			}
		}
	}
	sortLoops()

	for _, L := range order {
		// Find body blocks in current layout order.
		var body []*Block
		var headerIdx int = -1
		for i, b := range f.Blocks {
			if belongsTo(b, L) {
				if b == L.header {
					headerIdx = i
				}
				body = append(body, b)
			}
		}
		if headerIdx < 0 || len(body) == 0 {
			continue
		}

		// Check whether the body is already in the canonical shape:
		// header at f.Blocks[headerIdx], next len(body)-1 entries
		// are the remaining body in current order. If so, skip.
		canonical := f.Blocks[headerIdx] == L.header
		if canonical {
			for i := 1; i < len(body); i++ {
				if headerIdx+i >= len(f.Blocks) || f.Blocks[headerIdx+i] != body[i] {
					canonical = false
					break
				}
			}
		}
		if canonical && body[0] == L.header {
			continue
		}

		// Build the body run in the right order: header first, then
		// the rest in their current layout order (preserving inner-
		// loop contiguity which was established by earlier
		// iterations).
		run := make([]*Block, 0, len(body))
		run = append(run, L.header)
		for _, b := range body {
			if b != L.header {
				run = append(run, b)
			}
		}

		// Build the new f.Blocks: blocks before headerIdx in their
		// current order, then run, then everything after headerIdx
		// that isn't in the body (skipping body blocks).
		inBody := make(map[ID]struct{}, len(run))
		for _, b := range run {
			inBody[b.ID] = struct{}{}
		}
		newBlocks := make([]*Block, 0, len(f.Blocks))
		for i, b := range f.Blocks {
			if i < headerIdx {
				// Preserve blocks before the header.
				if _, ok := inBody[b.ID]; ok {
					// A body block that appears BEFORE the header in
					// the current layout — move it into the run
					// position. Skip here; we'll emit the whole run
					// at headerIdx.
					continue
				}
				newBlocks = append(newBlocks, b)
				continue
			}
			if i == headerIdx {
				newBlocks = append(newBlocks, run...)
				continue
			}
			// After headerIdx: skip body blocks (already in run).
			if _, ok := inBody[b.ID]; ok {
				continue
			}
			newBlocks = append(newBlocks, b)
		}

		// If body blocks appeared BEFORE the header in the original
		// layout, the run still ends up at headerIdx in newBlocks,
		// which is now off by the number of body-before-header blocks
		// that were skipped. The newBlocks construction handles that
		// implicitly (it skips them before headerIdx, then emits the
		// full run at headerIdx, then appends the rest).
		f.Blocks = newBlocks

		// Invalidate cached loopnest / dominators / sdom since we
		// reordered blocks. The next call to f.loopnest() or
		// f.Sdom() will recompute. Subsequent loop iterations re-read
		// the loopnest via the cached ln (which is now stale but
		// still references the same *loop instances; b2l is also
		// stale only in the sense that block-list order is different,
		// but b2l[b.ID] still correctly identifies the innermost
		// containing loop, since reordering does not change CFG).
	}

	f.invalidateCFG()
}
