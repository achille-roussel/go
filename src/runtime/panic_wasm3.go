// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import (
	"internal/runtime/atomic"
	_ "unsafe" // for go:linkname
)

// wasm3 mirror of panic.go.
//
// panic.go is tagged !wasm3 because the defer-chain manipulation
// (badDefer, deferprocStack's write-barrier-bypass triple, and the
// open-coded defer paths initOpenCodedDefers / savedOpenDeferState)
// uses `unsafe.Pointer(uintptr(...))` pointer arithmetic that anyref
// cannot represent. None of those four sites is reachable on wasm3:
// the defer machinery lives in defer_wasm3.go (wasm3DeferPush /
// wasm3DeferRunAll), the panic/recover entry points lower through
// wasm3Gopanic / wasm3Gorecover (M5 phase 1A+2+1B), and open-coded
// defers are disabled on wasm3 (commit handling of #142).
//
// What this file provides is the small subset of panic.go's
// surface that the rest of the wasm3 runtime build still references:
// `throw`, `fatal`, `throwType` (+ constants), `panicCheck1`, the
// `panicking` / `paniclk` / `runningPanicDefers` / `deadlock`
// globals, and Goexit as a wasm3-friendly forwarder. The runtime
// bounds/index panic helpers (`goPanicIndex`, `goPanicSliceAlen`,
// etc.) are emitted as compiler-internal calls from runtime checks
// on every arch — they're stubbed below to call panic() with a
// boundsError so the wasm3 panic path picks them up.

// throwType indicates the current type of ongoing throw, which
// affects the amount of detail printed to stderr. Same values as
// the non-wasm3 file so any panic-printing code that compares
// gp.m.throwing sees consistent constants.
type throwType uint32

const (
	throwTypeNone throwType = iota
	throwTypeUser
	throwTypeRuntime
)

// throw triggers a fatal runtime error.
//
// On non-wasm3 throw runs an elaborate two-stack dance to print a
// stack trace and crash the process. On wasm3 the host (V8) gives
// us no preempted-stack walking machinery and the runtime's
// scheduler is single-threaded, so we degrade to a print + exit.
//
//go:linkname throw
//go:nosplit
func throw(s string) {
	print("fatal error: ", s, "\n")
	exit(2)
}

// fatal triggers a fatal panic. Like throw, but used for fatal
// errors caused by user code (racing map writes etc.). Degrades
// to print + exit on wasm3 for the same reasons throw does.
//
//go:nosplit
func fatal(s string) {
	print("fatal error: ", s, "\n")
	exit(2)
}

// panicCheck1 is called by the runtime bounds/index/slice panic
// helpers to escalate to a throw when the panic happened inside
// runtime code (where panic recovery would be unsafe) or during
// malloc (where allocating a panic value would re-enter malloc).
//
// The non-wasm3 implementation uses the funcname/findfunc machinery
// to detect runtime-internal callers; wasm3 doesn't carry pclntab
// (#166), so we keep just the mallocing check.
func panicCheck1(pc uintptr, msg string) {
	gp := getg()
	if gp != nil && gp.m != nil && gp.m.mallocing != 0 {
		throw(msg)
	}
}

// runningPanicDefers is non-zero while running deferred functions
// for panic. Kept here so non-wasm3 callers that link against it
// (and the wasm3 panic helpers, if they ever start counting)
// find a definition. Wasm3 panic via wasm3Gopanic does not yet
// increment this.
var runningPanicDefers atomic.Uint32

// panicking is non-zero when crashing the program for an
// unrecovered panic.
var panicking atomic.Uint32

// paniclk would be held while printing panic information on
// non-wasm3 to prevent interleaved panic output across goroutines.
// On wasm3 the scheduler is single-threaded so interleaving cannot
// happen, but the symbol is retained so wasm3 panic helpers that
// might lock it for parity stay valid.
var paniclk mutex

// divideError / overflowError are referenced by math/bits via
// go:linkname (see runtime/linkname.go). Define them with the same
// underlying errorString values as the non-wasm3 path so any code
// that does `err == divideError` comparisons works identically.
var divideError = error(errorString("integer divide by zero"))
var overflowError = error(errorString("integer overflow"))

// deadlock is acquired twice in succession by the scheduler when
// the program deadlocks, so the second acquire blocks forever (or
// blocks long enough for the runtime's deadlock detector to fire
// its fatal). proc.go uses it on all arches; we keep the var here
// for symbol parity.
var deadlock mutex

// Goexit terminates the goroutine that calls it. No other goroutine
// is affected.
//
// On non-wasm3 Goexit walks the defer chain explicitly via _panic
// machinery; on wasm3 the defer chain lives in defer_wasm3.go's
// wasm3Defer* records, so we run those then exit the goroutine.
func Goexit() {
	wasm3DeferRunAll()
	goexit1()
}
