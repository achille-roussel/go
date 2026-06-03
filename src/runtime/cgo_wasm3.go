// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// wasm3 mirror of cgo.go / cgocall.go / cgocheck.go.
//
// cgo is unsupported on wasm3 (no C interop, no dlopen, no
// libc — the host runtime is V8/Node, not POSIX). Per the Rung
// 0 inventory, all three cgo files are TAG_OFF. This file
// provides just enough symbols to satisfy the link.
//
// The `_cgo_*` function-pointer slots are kept (as nil
// unsafe.Pointer) because non-wasm3 proc.go consults them via
// `if _cgo_notify_runtime_init_done != nil` etc. — checks that
// remain on wasm3 too and need the symbol to compile. iscgo,
// cgoHasExtraM, ncgocall are similarly probed by the scheduler /
// traceback paths and stay as zero values forever on wasm3.

// ---- vars from cgo.go ----------------------------------------

var (
	_cgo_init                     unsafe.Pointer
	_cgo_thread_start             unsafe.Pointer
	_cgo_sys_thread_create        unsafe.Pointer
	_cgo_notify_runtime_init_done unsafe.Pointer
	_cgo_callers                  unsafe.Pointer
	_cgo_set_traceback_functions  unsafe.Pointer
	_cgo_call_traceback_function  unsafe.Pointer
	_cgo_call_symbolizer_function unsafe.Pointer
	_cgo_yield                    unsafe.Pointer
	_cgo_pthread_key_created      unsafe.Pointer
	_cgo_bindm                    unsafe.Pointer
	_cgo_getstackbound            unsafe.Pointer
)

//go:linkname iscgo
var iscgo bool

//go:linkname set_crosscall2
var set_crosscall2 func()

var cgoHasExtraM bool
var cgoAlwaysFalse bool

var cgo_yield = &_cgo_yield

func cgoUse(any)       { throw("cgoUse should not be called on wasm3") }
func cgoKeepAlive(any) { throw("cgoKeepAlive should not be called on wasm3") }

func cgoNoCallback(v bool) {
	if v {
		// cgo callbacks aren't a thing on wasm3; if user code is
		// flipping this on, something is misconfigured.
		throw("cgoNoCallback called with v=true on wasm3 (no cgo)")
	}
}

// ---- vars from cgocall.go ------------------------------------

// cgoCallers is the stack-frame buffer the non-wasm3 traceback path
// fills when a crash happens inside a C frame. On wasm3 it stays at
// the zero value because no C frame ever runs.
type cgoCallers [32]uintptr

// ncgocall is incremented on every cgo call by cgocall (gated off).
// Kept here for parity so the runtime's debug printing doesn't
// trip on a missing symbol.
var ncgocall uint64

// cgocall is the runtime entry point cgo-generated code calls into.
// Wasm3 has no cgo-generated code, but the symbol is referenced from
// proc.go (cgocall(_cgo_notify_runtime_init_done, nil)) and from
// syscall_windows.go. The runtime guards those call sites with
// iscgo / _cgo_notify_runtime_init_done != nil, both always false
// on wasm3, so the body is unreachable.
func cgocall(fn, arg unsafe.Pointer) int32 {
	throw("runtime.cgocall unreachable on wasm3")
	return 0
}

// callbackUpdateSystemStack is called from cgo callbacks on the
// non-wasm3 path. Wasm3 doesn't have callbacks; if reached,
// something is very wrong.
func callbackUpdateSystemStack(mp *m, sp uintptr, signal bool) {
	throw("runtime.callbackUpdateSystemStack unreachable on wasm3")
}

// ---- funcs from cgocheck.go ----------------------------------

// cgoCheckMemmove2 / cgoCheckSliceCopy / cgoCheckPtrWrite are
// invoked by mbarrier.go / slice.go when GOEXPERIMENT=cgocheck2
// is on, to verify pointer writes follow the cgo rules. Wasm3
// has no cgo and the host GC tracks every ref slot, so the checks
// are vacuously satisfied — keep them as no-ops.

func cgoCheckMemmove2(typ *_type, dst, src unsafe.Pointer, off, size uintptr) {}

func cgoCheckSliceCopy(typ *_type, dst, src unsafe.Pointer, n int) {}

func cgoCheckPtrWrite(dst *unsafe.Pointer, src unsafe.Pointer) {}

// cgoCheckPointer / cgoCheckResult are //go:linkname'd from
// runtime/linkname.go for cgo-generated code to call. Wasm3 has
// no cgo-generated code, but the linkname must still resolve.
func cgoCheckPointer(ptr any, arg any) {}
func cgoCheckResult(val any)           {}
