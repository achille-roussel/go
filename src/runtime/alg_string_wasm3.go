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
// These helpers are plain Go: `a[i]` lowers via OpWasm3StringByte
// (Phase 2 of M3.5) to `array.get_u $go.bytes` on the $go.string
// backing at the right offset, so the byte loops below compile
// cleanly without any wat primitives. The compiler redirects
// string equality and ordering to these (see
// cmd/compile/internal/walk/compare.go).

// wasm3StringEqual reports whether a and b are equal.
//
//go:nosplit
func wasm3StringEqual(a, b string) bool {
	la := len(a)
	if la != len(b) {
		return false
	}
	for i := 0; i < la; i++ {
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
	la := len(a)
	lb := len(b)
	n := la
	if lb < n {
		n = lb
	}
	for i := 0; i < n; i++ {
		ca := a[i]
		cb := b[i]
		if ca < cb {
			return -1
		}
		if ca > cb {
			return +1
		}
	}
	if la < lb {
		return -1
	}
	if la > lb {
		return +1
	}
	return 0
}
