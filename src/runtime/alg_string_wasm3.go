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
// memequal. These helpers compare through the boxed representation
// using len + indexing, which the wasm3 backend lowers to
// struct.get (length) and array.get (bytes). The compiler redirects
// string equality, ordering, and the generated per-type equality
// functions to call these instead of the memequal path. See
// doc/wasm3-slice-boxing.md and the project memory on the boxed
// bulk-ops architecture.

// wasm3StringEqual reports whether a and b are equal.
//
//go:nosplit
func wasm3StringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// wasm3StringCompare returns -1, 0, or +1 according to whether a sorts
// before, equal to, or after b (lexicographic by unsigned bytes). It is
// the boxed-model replacement for runtime.cmpstring.
//
//go:nosplit
func wasm3StringCompare(a, b string) int {
	l := len(a)
	if len(b) < l {
		l = len(b)
	}
	for i := 0; i < l; i++ {
		ca, cb := a[i], b[i]
		if ca < cb {
			return -1
		}
		if ca > cb {
			return +1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return +1
	}
	return 0
}
