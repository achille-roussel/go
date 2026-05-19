// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// wasm3SliceBytesToString bridges `string(b)` on wasm3 from a
// wasmgc-backed []byte to a linear-memory-backed string. The
// standard slicebytetostring takes a `*byte` data ptr (i64 on
// wasm3), but a wasm3-make-style slice's .array is anyref —
// passing it through the slicebytetostring ABI fails wasm
// validation ("expected i64, found anyref" at the call site).
//
// Bridge approach: receive the slice intact (its ABI lowers to
// anyref backing + i64 len + i64 cap, all natively handled by
// the wasm3 slice-arg lowering), allocate a linear-memory
// buffer of len(b) bytes from the bump heap, copy bytes one at
// a time (b[i] lowers to `array.get_u` on the wasmgc array
// via the existing slice-index rewrite rules; the linear-
// memory store stays as `i32.store8`), and return a string
// whose .data points into linear memory. Strings on wasm3 use
// the linear-memory shape (i64 data ptr + i64 len) so the
// returned string flows through the rest of the language
// machinery without further bridging.
//
// The buf argument is honoured for small results so escape-
// analysis-marked non-escaping conversions reuse the caller's
// stack-allocated tmpBuf, matching the default
// slicebytetostring's optimisation.
//
//go:linkname wasm3SliceBytesToString
func wasm3SliceBytesToString(buf *tmpBuf, b []byte) string {
	n := len(b)
	if n == 0 {
		return ""
	}
	var p unsafe.Pointer
	if buf != nil && n <= len(buf) {
		p = unsafe.Pointer(buf)
	} else {
		p = mallocgc(uintptr(n), nil, false)
	}
	for i := 0; i < n; i++ {
		*(*byte)(unsafe.Add(p, i)) = b[i]
	}
	return unsafe.String((*byte)(p), n)
}
