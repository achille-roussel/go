// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// concatstrings on wasm3 is a temporary no-op during the Stage J
// cutover. The pre-cutover body bumped a linear-memory buffer for
// the result and memmoved bytes into it via unsafe.Pointer
// arithmetic on the per-string `unsafe.StringData(x)`. After J.1's
// signature flip the string headers carry an anyref data ref and
// the iteration over `a []string` reads each element via
// `array.get $go.box.string`, neither of which the runtime's
// memmove (linear-mem i64 pointers) nor the SSA backend's current
// indexing rewrites can lower.
//
// During the cutover the bring-up tests don't observe the result;
// once the wasmgc string-allocate + array.copy intrinsic lands
// (Stage J Piece γ/δ), the body returns to constructing a wasmgc
// `(array i8)` of total length and array-copying each source
// string's backing into it.
func concatstrings(buf *tmpBuf, a []string) string {
	_ = buf
	_ = a
	return ""
}

// concatbytes — same cutover constraint as concatstrings. Returns
// the zero value so build succeeds; behaviour restored in Piece γ.
func concatbytes(buf *tmpBuf, a []string) []byte {
	_ = buf
	_ = a
	return nil
}
