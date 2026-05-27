// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// wasm3SliceBytesToString implements `string(b)` on wasm3. Per Go's
// spec, the conversion COPIES b's bytes so a subsequent mutation of
// b does not change the resulting string. We honour that by
// allocating a fresh []byte of len(b) bytes (which lowers to
// array.new_default $go.bytes via OpWasm3MakeSlice — host GC owned),
// copying via the builtin `copy(fresh, b)` (which lowers to
// array.copy $go.bytes $go.bytes via OpWasm3ArrayCopy), then
// wrapping the fresh backing in a $go.string header via
// unsafe.String + unsafe.SliceData. No linear memory involved end
// to end.
//
// The buf argument is honoured for small results so escape-
// analysis-marked non-escaping conversions can reuse a stack-
// allocated tmpBuf, matching the default slicebytetostring's
// optimisation — but wasm3 doesn't yet have escape-analysis-aware
// non-escaping handling for this, so for now we always allocate.
// TODO(M4): tmpBuf reuse.
//
//go:linkname wasm3SliceBytesToString
func wasm3SliceBytesToString(buf *tmpBuf, b []byte) string {
	_ = buf
	n := len(b)
	if n == 0 {
		return ""
	}
	fresh := make([]byte, n)
	copy(fresh, b)
	return unsafe.String(unsafe.SliceData(fresh), n)
}
