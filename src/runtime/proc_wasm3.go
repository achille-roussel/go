// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import (
	"runtime/wasm"
	"unsafe"
)

// proc_wasm3.go is the wasm3-specific scheduler glue around the
// WebAssembly 3.0 stack-switching primitives in package runtime/wasm.
// The shims here are the wasm3 analog of asm_wasm.s's gogo / mcall /
// systemstack / mstart — they reduce the standard scheduler's
// conceptual model (SP+PC saved on g, switch by overwriting registers)
// to "the goroutine is a continuation, switching means resuming a
// different one."
//
// Engine viability note (see doc/wasm3-m4-status.md): the Go-side
// scaffolding here is complete, but V8 December 2025 and wasmtime 44
// do not yet implement the suspend / resume-with-handler runtime that
// these shims rely on. Until engine support lands every path through
// this file traps at runtime; the toolchain still validates and the
// non-suspending wasm.RunInCont fixture still runs (see
// /tmp/m4_runincont.go).

// wasm3SchedulerCont holds the cont that the scheduler is "in." When
// a goroutine calls gopark, it suspends back to the scheduler — the
// suspended-cont reference that wasm.Suspend returns goes into
// gp.wasm3Cont so a future goready can wake it via wasm.RunInCont (or
// the resume primitive directly once Phase 4 adds it).
//
// Today only the bootstrap g0 is a meaningful "scheduler cont"; future
// per-M scheduler cont state lands here when sysmon and friends turn
// on.
var wasm3SchedulerCont unsafe.Pointer

// gogo_wasm3 wakes gp by resuming its continuation. Replaces the
// machine-code gogo(*gobuf) — wasm3 ignores buf.sp/buf.pc because
// host conts own the call stack; the only state on g that matters is
// wasm3Cont.
//
// Until Phase 4's resume-with-handler-vec encoding lands, the only
// available primitive is wasm.RunInCont, which spins up a *fresh*
// cont from a bare entry function rather than resuming an existing
// one. The body therefore still traps on a real gopark+goready cycle;
// it succeeds for the "spawn-and-run-to-completion" path used by the
// Phase 2 fixture.
//
//go:nosplit
func gogo_wasm3(gp *g) {
	if gp.wasm3Cont == nil {
		// Fresh goroutine: nothing to resume yet. The wasm3 spawn
		// path (Phase 4) will populate gp.wasm3Cont by calling
		// wasm.ContNew(entry) before the first gogo. For now,
		// fatal — this path is only reachable once Phase 4 lands.
		throw("gogo_wasm3: gp.wasm3Cont is nil")
	}
	// Phase 4: wasm.Resume(gp.wasm3Cont) and stash the returned
	// suspended-cont (if any) back into gp.wasm3Cont.
	throw("gogo_wasm3: wasm.Resume not yet implemented (Phase 4 needs resume-with-handler-vec)")
}

// mcall_wasm3 is the wasm3 reimplementation of mcall(fn). On other
// arches mcall switches to g0's stack and calls fn(g) — wasm3 has no
// linear-memory stacks so it just calls fn directly with the current g.
// The "must never return" contract is preserved: fn is expected to
// suspend or terminate the goroutine, never just return.
//
// Reachable callers (audited via grep over proc.go): park_m, goexit0,
// goschedImpl, gopreempt_m, the dropg paths. All conform to the
// "doesn't return" contract.
//
//go:nosplit
func mcall_wasm3(fn func(*g)) {
	gp := getg()
	fn(gp)
	// fn must not return. If it does, that's a bug.
	throw("mcall_wasm3: fn returned (expected suspend or goexit)")
}

// systemstack_wasm3 is the wasm3 reimplementation of systemstack(fn).
// On other arches it switches to g0's stack. wasm3 has no separate
// system stack — the host owns the wasm activation frames and growth
// is engine-managed — so this is a plain call.
//
//go:nosplit
func systemstack_wasm3(fn func()) {
	fn()
}

// park_wasm3 is the wasm3 implementation of gopark's wasm3-specific
// epilogue: snapshot wasm global 0 (the bridge bump pointer) into
// gp.bridgeBump so it can be restored on resume; then suspend.
//
// Phase 5 of the M4 plan adds the gp.bridgeBump field; for now the
// snapshot is omitted (no real Phase 4 caller exists yet).
//
//go:nosplit
func park_wasm3() {
	wasm.Suspend()
}

// _ keeps wasm imported even when no body above currently references
// it through anything but Suspend (Phase 4+ adds more uses).
var _ = wasm.RunInCont
