// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(js && wasm3)

// sched_notjswasm3.go provides no-op / standard-goready stubs of
// the wasm3PreparePark / wasm3Goready / wasm3Park hooks that
// chan.go / select.go / proc.go call unconditionally. On
// js/wasm3 these are real (sched_jswasm3.go); everywhere else
// they reduce to the standard runtime contract.

package runtime

import "unsafe"

// wasm3PreparePark is a no-op outside js/wasm3.
//
//go:nosplit
func wasm3PreparePark(sg *sudog) {}

// wasm3Goready forwards to the standard goready outside
// js/wasm3.
//
//go:nosplit
func wasm3Goready(sg *sudog) {
	goready(sg.g, 3)
}

// wasm3Park is unreachable outside js/wasm3 — the gopark
// dispatch's compile-time `goos.IsJs == 1 && goarch.IsWasm3 == 1`
// gate DCE's the call on other arches, but the symbol still
// needs to resolve at link time.
//
//go:nosplit
func wasm3Park(unlockf func(*g, unsafe.Pointer) bool, lock unsafe.Pointer) {
	throw("wasm3Park called on non-js/wasm3 build")
}
