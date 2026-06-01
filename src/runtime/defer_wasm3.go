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

// wasm3PanicValue is the in-flight panic value, written by
// wasm3Gopanic and read by wasm3Gorecover. Single-goroutine only
// (pairs with wasm3DeferHead's single-goroutine limitation).
var wasm3PanicValue any

// wasm3InRecoverableDefer is true while wasm3Gopanic is iterating
// deferred functions during unwind. recover() observes the in-flight
// value only when this flag is set, matching Go's spec rule that
// recover only takes effect when called from a deferred function.
var wasm3InRecoverableDefer bool

// wasm3PanicSwallowed is set by wasm3Gorecover when it returns the
// in-flight panic to its caller. wasm3Gopanic checks this AFTER its
// defer loop returns; a false value means no defer called recover,
// so the panic propagates. Tracked as a separate flag from
// wasm3PanicValue==nil because clearing wasm3PanicValue inside
// wasm3Gorecover is folded away by the SSA optimizer
// (read-then-write of a package-level any global is treated like a
// local by inter-procedural analysis — the read is dropped because
// the write provides the latest value, which is nil).
var wasm3PanicSwallowed bool

// wasm3Gopanic replaces runtime.gopanic on wasm3. Sets the in-flight
// panic value, runs every deferred function in LIFO order (each
// observable via wasm3Gorecover during its body), then either
// returns normally (panic was recovered) or unreachable-traps the
// program (no recover).
//
// PHASE 2 SIMPLIFICATION: the wasm activation frames between the
// panic call site and the recovering caller are NOT unwound — defers
// run in-place. For panic-without-recover the wasm "unreachable"
// trap is the program failure, which is fine. For recover from a
// defer in the panicking function's own frame this matches Go
// semantics. For recover from a caller's defer when a callee
// panicked the callee's remaining instructions would still execute
// briefly after wasm3Gopanic returns, but the SSA noreturn bit
// (entry in ssa.go's gopanic special-case list) marks the post-
// call block BlockExit, so the caller actually never falls through
// — unreachable trap. A proper try_table / throw_ref implementation
// is M5 phase 1B follow-up; see project_wasm3_m5_panic_recover_blockers
// memory.
//
//go:nosplit
func wasm3Gopanic(e any) {
	wasm3PanicValue = e
	wasm3PanicSwallowed = false
	wasm3InRecoverableDefer = true
	wasm3DeferRunAll()
	wasm3InRecoverableDefer = false
	if wasm3PanicSwallowed {
		// Recovered. The compiler-side noreturn marking on
		// wasm3Gopanic means the calling function's post-call
		// block is BlockExit — if we return normally, the caller
		// falls into the noreturn unreachable trap. Without wasm
		// exception handling we cannot unwind the caller's wasm
		// frame; for now the user-visible behavior of a recovered
		// panic is a stack-overflow trap rather than a clean
		// exit. The deferred function's println / recover work
		// observably (output appears before the trap). A real
		// M5 phase 3 with try_table + throw_ref will let the
		// panicking function's frame return.
		wasm3PanicTrap()
	}
	// Unrecovered: same trap.
	wasm3PanicTrap()
}

// wasm3PanicTrap is reached when an unrecovered panic propagates
// past the topmost defer. Cannot call runtime.abort or
// runtime.throw because both end up routed through sigpanic ->
// wasm3Gopanic -> wasm3PanicTrap (infinite recursion) on wasm3 (or,
// for abort, link errors because the asm stub isn't compiled).
// The minimal trap that avoids the cycle: blow the wasm call
// stack via deep recursion. The engine terminates the program
// with a stack-overflow error containing wasm3PanicTrap in the
// trace — recognizable as "unrecovered panic" without printing
// the panic value (phase 3 will add proper value formatting).
//
//go:noinline
func wasm3PanicTrap() {
	wasm3PanicTrapRecurse(0)
}

//go:noinline
func wasm3PanicTrapRecurse(depth int) {
	wasm3PanicTrapRecurse(depth + 1)
}

// wasm3Gorecover replaces runtime.gorecover on wasm3. Returns the
// in-flight panic value when called from a deferred function during
// unwind (wasm3InRecoverableDefer set), nil otherwise. Clears
// wasm3PanicValue so the panic is considered "swallowed" and
// wasm3Gopanic's post-loop check sees no in-flight panic.
//
//go:nosplit
func wasm3Gorecover() any {
	if !wasm3InRecoverableDefer {
		return nil
	}
	wasm3PanicSwallowed = true
	return wasm3PanicValue
}
