// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package wasm

// sched_wasm3.go exposes the WebAssembly 3.0 stack-switching primitives
// (cont.new / resume / suspend / switch) as compiler intrinsics. The
// bodies trap because the SSA layer rewrites every call into the
// corresponding OpWasm3* SSA op before lowering. See
// cmd/compile/internal/ssagen/intrinsics.go and
// cmd/compile/internal/wasm3/ssa.go.
//
// M4 Phase 2 lands the minimum interface (RunInCont) needed to exercise
// the cont machinery end-to-end. Phase 3 expands it with separate
// ContNew / Resume / Suspend / Switch primitives so the scheduler can
// split goroutine creation from goroutine resumption.

// RunInCont allocates a continuation that calls fn, resumes it once,
// and returns when fn reaches its end. fn must be a bare top-level
// function (PFUNC) so the intrinsic can lower it to a ref.func instead
// of a closureCtx unpack — passing a closure literal or method value
// is a compile-time error from the intrinsic.
//
// The cont's body type is the empty (func) — V8 (December 2025) does
// not yet implement resume against (func ... -> anyref) shapes. Args
// and results route through globals or runtime side channels until
// engine support catches up; Phase 3+ extends this with separate
// ContNew / Resume / Suspend / Switch primitives that pass anyref
// payloads through tags.
func RunInCont(fn func()) {
	panic("runtime/wasm: RunInCont intrinsic not registered (ssagen.intrinsics, sys.ArchWasm3)")
}

// Suspend yields control back to the most recent enclosing resume that
// installed a handler for the M4 "goroutine park" tag (the singleton
// declared by cmd/link/internal/wasm/asm3.go writeTagSec3 at
// Wasm3TagIndexPark=0). When the suspended continuation is later
// resumed, Suspend returns.
//
// Calling Suspend outside of a cont resumed with a park-tag handler
// installed traps with "uncaught tag" — Phase 3 callers run inside a
// resume that catches the tag, so this is correct by construction.
//
// The tag carries no payload today; values flow through wasm globals
// or runtime side channels in the meantime (see [wasm3sched-anyref-shape]).
func Suspend() {
	panic("runtime/wasm: Suspend intrinsic not registered (ssagen.intrinsics, sys.ArchWasm3)")
}

// RunInContCatchSuspend is the suspend-catching variant of RunInCont.
// It runs fn inside a fresh continuation with a handler installed for
// the M4 park tag (Wasm3TagIndexPark), so any wasm.Suspend that fn
// executes is caught at the resume site instead of escaping up the
// call stack. The suspended cont is discarded today; Phase 5 will
// instead stash it into the calling goroutine's g.wasm3Cont so the
// scheduler can resume it later.
//
// Same restriction as RunInCont: fn must be a bare top-level function
// name (PFUNC) so the intrinsic can lower it to ref.func directly.
//
// Engine viability: V8 December 2025 fatals "unimplemented code" in
// GetContinuationResumeDescriptor when it executes a resume with a
// handler that catches a suspend, and wasmtime 44 has no stack-
// switching at all. The toolchain emits the correct wire bytes (see
// doc/wasm3-m4-status.md) and wasm-tools validates them, but actually
// invoking this function on either engine traps at module-instantiate
// or function-call time. The full Phase 4 scheduler that this hook
// plugs into is gated on engine maturity.
func RunInContCatchSuspend(fn func()) {
	panic("runtime/wasm: RunInContCatchSuspend intrinsic not registered (ssagen.intrinsics, sys.ArchWasm3)")
}
