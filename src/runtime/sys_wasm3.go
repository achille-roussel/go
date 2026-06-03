// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/runtime/sys"
	"unsafe"
)

type m0Stack struct {
	_ [8192 * sys.StackGuardMultiplier]byte
}

var wasmStack m0Stack

func wasmDiv()

func wasmTruncS()
func wasmTruncU()

//go:wasmimport gojs runtime.wasmExit
func wasmExit(code int32)

// gostartcall on non-wasm3 adjusts the Gobuf as if it had
// executed a call to fn with context ctxt and stopped before the
// first instruction in fn. On wasm3 the gobuf has no real
// PC/SP — goroutines run as JSPI continuations and V8 owns the
// frame; gobuf.pc isn't dispatched from. Skip the stack-frame-
// pointer write and the `uintptr(fn)` cast; stash ctxt only.
//
// stack_wasm3.go's gostartcallfn (called by newproc) uses ctxt
// for the funcval reference, which is the only thing actually
// needed when the wasm3 scheduler resumes the goroutine.
func gostartcall(buf *gobuf, fn, ctxt unsafe.Pointer) {
	buf.ctxt = ctxt
}

func notInitialized() // defined in assembly, call notInitialized1

// Called if a wasmexport function is called before runtime initialization
//
//go:nosplit
func notInitialized1() {
	writeErrStr("runtime: wasmexport function called before runtime initialization\n")
	if isarchive || islibrary {
		writeErrStr("\tcall _initialize first\n")
	} else {
		writeErrStr("\tcall _start first\n")
	}
}
