// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// Phase 0 minimum OS-layer for GOOS=js GOARCH=wasm3 — just enough
// to link an empty main. All bodies trap or return zero; Phase 1
// wires them to JS-supplied JSPI imports.
//
// Decls already provided by os_wasm3.go for the GOARCH=wasm3 family
// (osinit, signame, initsig, preemptM, exitThread, osyield, mpreinit,
// minit, unminit, sigsave, msigrestore, sigblock, clearSignalHandlers,
// sigpanic, getCPUCount, crash, newosproc, os_sigpipe, mdestroy,
// usleep_no_g, syscall_now, cputicks, preemptMSupported, _NSIG,
// _SIGSEGV) intentionally NOT re-declared here.
//
// Filename `os_jswasm3` (no underscore between js and wasm3) avoids
// the implicit GOOS_GOARCH suffix; the explicit //go:build tag is
// the sole constraint.

package runtime

// exit terminates the wasm instance. Phase 1 binds this to a JS
// import `__wasm3_exit(code)` that calls process.exit / throws.
func exit(code int32) {
	throw("js/wasm3 runtime.exit not yet implemented")
}

// usleep blocks at least usec microseconds. Single-threaded busy-
// yield until Phase 5 wires it to setTimeout via JSPI.
func usleep(usec uint32) {
	deadline := nanotime() + int64(usec)*1000
	for nanotime() < deadline {
		Gosched()
	}
}

// walltime / nanotime1 are NOT declared here — stubs3.go and
// timestub2.go already declare them as wasmimport from the `gojs`
// module for any non-wasip1 build. Phase 1 implements them on the
// JS side.

// goenvs populates envs/argslice from the JS-supplied environment.
// Phase 1 reads them from the wasm_exec shim.
func goenvs() {}

// readRandom is the entropy source for math/rand and crypto seed.
// Phase 0: zeros (deterministic — fine for empty main but obviously
// not for production). Phase 1+ wires this to crypto.getRandomValues
// via a synchronous JS import.
func readRandom(r []byte) int {
	for i := range r {
		r[i] = 0
	}
	return len(r)
}
