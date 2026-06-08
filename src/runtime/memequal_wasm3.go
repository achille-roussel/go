// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// memequal for GOARCH=wasm3 is implemented in Go rather than via the
// internal/bytealg assembly in equal_wasm3.s. That asm is written in
// the wasm linear-memory frame ABI (`I64Load a+0(FP)` etc.) which the
// wasm3 obj backend can't lower — the unencodable body is emitted as a
// bare `unreachable` stub, so any caller traps. The runtime ABI-bridge
// wrapper generation that would otherwise span ABI0 (the asm) and
// ABIInternal (callers) is also disabled on wasm3 (see
// cmd/compile/internal/ssagen.GenABIWrappers), since its bridging body
// uses SP-relative spills the wasm3 backend likewise can't lower.
//
// This Go body runs under ABIInternal so the wasm3 backend lowers the
// parameters as ordinary wasm function params. Byte-by-byte is fine
// here: the wasm3 backend doesn't yet have a sized-memory.compare
// intrinsic, and correctness wins over throughput for the bring-up.
//
//go:linkname memequal
//go:nosplit
func memequal(a, b unsafe.Pointer, size uintptr) bool {
	if a == b || size == 0 {
		return true
	}
	// Use unsafe.Slice instead of `unsafe.Add(a, i)` + `*(*byte)`
	// per-byte deref. The pointer-arithmetic shape (offset add then
	// scalar load) lowers on wasm3 to a polyptr accessor pair that
	// passes a *byte fat pointer through anyref-classified per-
	// value locals, and the deref tail is the documented #167-
	// pending bridge-gap pattern. unsafe.Slice synthesizes a real
	// []byte that goes through the native wasm3 slice ABI: aS[i]
	// becomes array.get_u $go.bytes (sa.off + i) — no polyptr step,
	// no bridge gap, same byte-by-byte logic. Equivalent semantics
	// for memequal's purposes (compare exactly size bytes from
	// each pointer).
	aS := unsafe.Slice((*byte)(a), size)
	bS := unsafe.Slice((*byte)(b), size)
	for i := uintptr(0); i < size; i++ {
		if aS[i] != bS[i] {
			return false
		}
	}
	return true
}
