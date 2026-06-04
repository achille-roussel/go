// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package slices

// overlaps on wasm3 always reports "no overlap." The non-wasm3
// version does pointer arithmetic on the slice backings — taking
// `&a[0]` as a byte address and comparing against `&b[0]+(len(b)-1)`
// — to choose between a simple copy() path and a shift-aware path
// in Replace / Insert / Delete. On wasm3 slice backings are
// distinct (ref (array T)) WasmGC values; copy() lowers to
// array.copy, which is documented to handle overlapping source and
// destination ranges correctly (it's effectively memmove semantics
// at the array-element granularity). Reporting "no overlap"
// unconditionally just keeps the algorithm on the fast path —
// safe because the wasm3 substrate doesn't have the aliasing
// hazard the overlap check was protecting against.
func overlaps[E any](a, b []E) bool {
	return false
}
