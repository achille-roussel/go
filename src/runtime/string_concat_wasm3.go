// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// concatstrings on wasm3 bypasses the shared rawstring → []byte →
// memmove path used by the default implementation. The shared path
// allocates a linear-memory buffer (mallocgc → i64) but exposes it
// as a []byte whose .array is anyref per the wasm3 slice ABI;
// memmove then takes i64 pointers, so the mid-pipeline []byte
// forces the body to span two memory worlds and fails wasm module
// validation with "type mismatch: expected i64, found anyref" at
// the memmove call site.
//
// String storage on wasm3 is still linear memory (TSTRING lowers to
// (i64 data, i64 len) — Stage J's wasmgc-strings cutover has not
// landed), so we allocate the destination directly with mallocgc,
// memmove each source string's bytes into place using only i64
// pointers, and wrap the buffer as a string with unsafe.String. No
// []byte enters the mix.
func concatstrings(buf *tmpBuf, a []string) string {
	idx := 0
	l := 0
	count := 0
	for i, x := range a {
		n := len(x)
		if n == 0 {
			continue
		}
		if l+n < l {
			throw("string concatenation too long")
		}
		l += n
		count++
		idx = i
	}
	if count == 0 {
		return ""
	}
	if count == 1 && (buf != nil || !stringDataOnStack(a[idx])) {
		return a[idx]
	}
	// Allocate in linear memory so the result and every memmove
	// source operand stay i64. The tmpBuf fast path is intentionally
	// skipped: tmpBuf is a *[32]byte whose backing is a wasmgc array
	// on wasm3, which would re-introduce the anyref/i64 split.
	p := mallocgc(uintptr(l), nil, false)
	pos := uintptr(0)
	for _, x := range a {
		n := uintptr(len(x))
		if n == 0 {
			continue
		}
		memmove(unsafe.Add(p, pos), unsafe.Pointer(unsafe.StringData(x)), n)
		pos += n
	}
	return unsafe.String((*byte)(p), l)
}
