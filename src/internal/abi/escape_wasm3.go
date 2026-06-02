// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 mirror of escape.go. NoEscape on every other arch uses the
// `unsafe.Pointer(uintptr(p) ^ 0)` trick to hide p from escape
// analysis. On wasm3 that conversion is rejected at compile time
// (anyref has no scalar form), and the optimization isn't useful
// anyway: every wasm3 reference is a WasmGC ref managed by the host,
// not a value the Go runtime tries to keep off-heap. The identity
// implementation is therefore both correct and optimal.

package abi

import "unsafe"

// NoEscape on wasm3 must NOT be inlined. The non-wasm3 body uses the
// `unsafe.Pointer(uintptr(p) ^ 0)` launder so that when the inliner
// expands NoEscape into the caller, escape analysis sees the
// uintptr step "lose" the pointer. On wasm3 that conversion is
// rejected at compile time and my body is an identity — if the
// inliner expanded it, escape analysis would propagate the escape
// straight through `return p`. Keep the call live and let the
// escape-analyzer wasm3 carve-out in cmd/compile/internal/escape
// discard the argument's escape contribution at the CallExpr level.
//
//go:noinline
//go:nosplit
//go:nocheckptr
func NoEscape(p unsafe.Pointer) unsafe.Pointer {
	return p
}

var alwaysFalse bool
var escapeSink any

// Escape forces any pointers in x to escape to the heap.
func Escape[T any](x T) T {
	if alwaysFalse {
		escapeSink = x
	}
	return x
}

// EscapeNonString — see the upstream definition in escape.go for the
// rationale. Implemented as a compiler intrinsic.
func EscapeNonString[T any](v T) { panic("intrinsic") }

// EscapeToResultNonString — see the upstream definition. The non-
// wasm3 path uses NoEscape's uintptr-xor trick on &v; on wasm3 the
// identity NoEscape just returns &v, which is fine here since the
// value semantics ("return a copy of v") are preserved by *(*T) deref.
func EscapeToResultNonString[T any](v T) T {
	EscapeNonString(v)
	return *(*T)(NoEscape(unsafe.Pointer(&v)))
}
