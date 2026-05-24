// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// Boxed-string comparison helpers for GOARCH=wasm3.
//
// On linear-memory targets a string == lowers to a length check plus
// memequal over the string's data pointer, and ordering lowers to
// cmpstring (also a memequal-style scan). Both assume the string's
// bytes live at a linear address. In the wasm3 boxed object model a
// string is a single (ref $go.string) whose bytes live in a
// (ref $go.bytes) array — there is no linear pointer to hand to
// memequal.
//
// The leaf byte loops live in the dynamically-linked go_runtime wat
// module (runtime/wasm/go_runtime.wat): stringEqual implements the
// equality fast path (length-first, then array.get_u byte loop), and
// strcmp implements lexicographic ordering. The Go-side helpers below
// are thin //go:wasmimport bridges that the compiler redirects string
// equality and ordering to (see cmd/compile/internal/walk/compare.go).
// See doc/wasm3-slice-boxing.md and [[wasm3-boxed-bulkops]] for the
// architecture; the wat primitives are engine-validated against the
// runtime/wasm/go_runtime_smoke.wat harness on wasmtime 44.

// wasm3StringEqualImport is the //go:wasmimport bridge to the
// go_runtime module's stringEqual primitive. The boxed-string ABI
// for //go:wasmimport (gated to module="go_runtime") passes each
// string as a single (ref null $go.string), matching the wat side.
//
//go:wasmimport go_runtime stringEqual
func wasm3StringEqualImport(a, b string) int32

// wasm3StringEqual reports whether a and b are equal.
//
//go:nosplit
func wasm3StringEqual(a, b string) bool {
	return wasm3StringEqualImport(a, b) != 0
}

// wasm3StringCompareImport is the //go:wasmimport bridge to the
// go_runtime module's strcmp primitive (a wat function returning
// negative/0/positive in i32 — the boxed-string analog of
// runtime.cmpstring).
//
//go:wasmimport go_runtime strcmp
func wasm3StringCompareImport(a, b string) int32

// wasm3StringCompare returns -1, 0, or +1 according to whether a sorts
// before, equal to, or after b (lexicographic by unsigned bytes). It is
// the boxed-model replacement for runtime.cmpstring.
//
//go:nosplit
func wasm3StringCompare(a, b string) int {
	c := wasm3StringCompareImport(a, b)
	switch {
	case c < 0:
		return -1
	case c > 0:
		return +1
	}
	return 0
}
