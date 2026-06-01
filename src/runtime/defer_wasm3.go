// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 M5 phase 1: defer via a heap-allocated linked list of
// WasmGC structs.
//
// Each `defer fn()` site compiles to a call to wasm3DeferPush(fn),
// which prepends fn to a linked list. Each function with at least
// one defer compiles to a call to wasm3DeferRunAll() in the
// function's exit block (substituted for the standard
// `deferreturn` lookup; see cmd/compile/internal/ssagen/ssa.go's
// InitConfig + exit()). wasm3DeferRunAll walks the list in LIFO
// order and call_ref's each entry.
//
// PHASE 1 LIMITATION (single-goroutine only): the list head lives
// in a package-level global, not on g. That's because runtime.getg()
// has no wasm3-backend lowering — OpGetG has no handler in
// cmd/compile/internal/wasm3/ssa.go, so getg()'s SSA value remains
// uninitialized and any deref traps. Phase 3 (cross-goroutine
// panic) will need a proper getg() implementation (a wasm3 module
// global holding the current g, populated at gogo + initialized at
// boot); when that lands, this file will move the list head onto
// g and the per-goroutine version will replace this stub.
//
// Panic-unwind defer firing (phase 1B) is NOT covered here —
// when a panic propagates past a function, that function's defers
// do not run yet. Phase 1B adds the try_table wrap around the
// function body so the catch handler calls wasm3DeferRunAll
// before re-throwing.

package runtime

// wasm3DeferRec is a single-linked stack node holding one
// deferred function and a link to the next-most-recent one. The
// list shape (vs. a []func() slice) sidesteps the wasm3 backend's
// missing TFUNC entry in wasm3SliceElemInteriorOK — indexing
// []func() falls through to the wasm1 linear-memory arithmetic
// path which is invalid on the WasmGC heap. Adding TFUNC to the
// gate is the right long-term fix; the list shape avoids the
// issue entirely.
type wasm3DeferRec struct {
	fn   func()
	prev *wasm3DeferRec
}

// wasm3DeferHead is the current top of the deferred-function
// stack. Single-goroutine only (see file-level comment) —
// concurrent goroutines would race on this global. The wasm3
// backend lowers a package-level *wasm3DeferRec global to a wasm3
// module ref-global (OpWasm3GlobalGet / OpWasm3GlobalSet); no
// linear-memory addressing involved.
var wasm3DeferHead *wasm3DeferRec

// wasm3DeferPush prepends fn to the deferred-function stack so
// the next wasm3DeferRunAll runs it first (LIFO).
func wasm3DeferPush(fn func()) {
	wasm3DeferHead = &wasm3DeferRec{fn: fn, prev: wasm3DeferHead}
}

// wasm3DeferRunAll pops every deferred function off the stack in
// LIFO order and calls each one. Drains the stack completely;
// subsequent calls are no-ops. Called from a function's exit
// block when the compiler determines the function has at least
// one `defer`.
//
// A deferred function that itself pushes new defers (via
// `defer fn2()` inside its body) extends the list during the
// loop; the head re-read each iteration fires those nested
// defers too — after the parent's other defers, before the
// parent's wasm3DeferRunAll caller returns. Matches Go semantics.
func wasm3DeferRunAll() {
	for wasm3DeferHead != nil {
		rec := wasm3DeferHead
		// Pop first so a nested defer pushed by rec.fn() lands
		// at the CORRECT depth (deeper than the parent's
		// siblings, not the parent itself).
		wasm3DeferHead = rec.prev
		rec.fn()
	}
}
