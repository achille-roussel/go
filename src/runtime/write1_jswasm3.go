// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// Phase 2 write path for GOOS=js GOARCH=wasm3: stage an
// [fd:u32 | n:u32 | data:n_bytes] header in the linear-memory
// bridge arena and call a single-i32-param wasmimport that copies
// (mem[off+8 : off+8+n]) to process.stdout / stderr. The single-
// param convention matches the legacy gojs ABI used for every
// other gojs import; a multi-param wasmimport gap remains
// (see cmd/compile/internal/ssagen/abi.go setupWasmImport, the
// gojs special case).

package runtime

import (
	"runtime/wasm"
	"unsafe"
)

//go:wasmimport gojs runtime.writeBytes
func wasmJSWriteBytes(headerOff uint32)

//go:nosplit
func write1Bytes(fd uintptr, b []byte) int32 {
	n := uint32(len(b))
	if n == 0 {
		return 0
	}
	// Header: [fd:u32][n:u32] little-endian.
	var hdr [8]byte
	hdr[0] = byte(fd)
	hdr[1] = byte(fd >> 8)
	hdr[2] = byte(fd >> 16)
	hdr[3] = byte(fd >> 24)
	hdr[4] = byte(n)
	hdr[5] = byte(n >> 8)
	hdr[6] = byte(n >> 16)
	hdr[7] = byte(n >> 24)
	hdrOff := wasm.WriteLinearMemory(0, hdr[:])
	wasm.WriteLinearMemory(0, b) // contiguous after hdr (bump pointer)
	wasmJSWriteBytes(hdrOff)
	wasm.ResetLinearMemory(0, hdrOff)
	return int32(n)
}

// write1 keeps the (uintptr, unsafe.Pointer, int32) shape that
// time_nofake.go's linkname-locked runtime.write depends on. The
// wasm3 path uses write1Bytes (slice ABI); this signature stays a
// trap.
//
//go:nosplit
func write1(fd uintptr, p unsafe.Pointer, n int32) int32 {
	_ = fd
	_ = p
	_ = n
	throw("js/wasm3 write1(unsafe.Pointer) unreachable")
	return -1
}
