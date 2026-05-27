// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// wasm3SliceBytesToString bridges `string(b)` on wasm3 from a
// wasmgc-backed []byte to a WasmGC $go.string sharing the same
// $go.bytes backing. The standard slicebytetostring takes a `*byte`
// data ptr (i64 on wasm) and calls mallocgc to allocate a fresh
// linear-memory buffer — neither lowers on wasm3 (M3.5 retired the
// linear-memory bump heap; *byte is anyref).
//
// Pure WasmGC approach:
//
//   - unsafe.SliceData(b) lowers via the OSPTR / OpSlicePtr chain
//     to OpWasm3SliceData (struct.get $go.slice.u8 0), returning
//     the boxed $go.bytes backing as an anyref-typed *byte.
//
//   - unsafe.String(p, n) lowers to ir.OUNSAFESTRING → OpStringMake
//     → OpWasm3StructNew with $go.string as Aux. The StructNew
//     codegen ref.casts p to (ref $go.bytes) for the backing field
//     and emits struct.new $go.string {bytes, 0, len}.
//
// Net effect: a string header is built around the slice's existing
// backing, no copy, no linear memory. The buf argument is ignored
// because the WasmGC backing is owned by the host GC; no alloc-
// optimisation tmpBuf trick is needed.
//
// SEMANTIC NOTE: string(b) on standard Go copies b's data so a
// subsequent mutation of b doesn't change the resulting string.
// Phase 3 of M3.5 ships the aliasing form for simplicity; the
// follow-up to add unsafe.SliceClone / array.copy-backed string
// conversion preserves the immutability invariant. Map keys built
// from []byte (the main consumer of this function in the runtime's
// hot path) hash and equate by current bytes, so the aliasing
// works correctly there. User code that string-converts a mutable
// []byte and expects immutability is broken until the follow-up
// lands; this is documented as a wasm3 known issue.
//
//go:linkname wasm3SliceBytesToString
func wasm3SliceBytesToString(buf *tmpBuf, b []byte) string {
	_ = buf
	n := len(b)
	if n == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), n)
}
