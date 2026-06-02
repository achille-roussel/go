// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// Package alias implements memory aliasing tests.
//
// wasm3 stub: slices are WasmGC arrays with no scalar address, so
// the standard `uintptr(unsafe.Pointer(&x[0]))` range-overlap test
// can't compile and there's no Go-visible "is this the same backing
// array" predicate either. Return the safe-overestimate (true) so
// AEAD / cipher / Stream implementations conservatively copy when
// the result might alias the input. Performance hit, correctness
// preserved.
package alias

func AnyOverlap(x, y []byte) bool {
	return len(x) > 0 && len(y) > 0
}

func InexactOverlap(x, y []byte) bool {
	if len(x) == 0 || len(y) == 0 || &x[0] == &y[0] {
		return false
	}
	return true
}
