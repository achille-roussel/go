// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasip1 && wasm3

package runtime

import (
	"runtime/wasm"
	"unsafe"
)

// write1 keeps the (uintptr, unsafe.Pointer, int32) signature so
// runtime.write in time_nofake.go (linkname-locked) still compiles.
// On wasm3 the unsafe.Pointer is meaningless and the body traps;
// all reachable wasm3 print sites use write1Bytes below.
//
//go:nosplit
func write1(fd uintptr, p unsafe.Pointer, n int32) int32 {
	_ = fd
	_ = p
	_ = n
	throw("wasm3: runtime.write1(unsafe.Pointer) unreachable — use write1Bytes")
	return -1
}

// write1Bytes is the wasm3 print path: stage the byte slice + iovec
// + nwritten cell in the linear-memory bridge arena and call
// fd_write. The slice param crosses the Go-to-Go boundary as a
// single (ref $go.slice.u8) thanks to the unified slice ABI
// (flatPrimitiveFields TSLICE → one anyref). No more sidestepping.
//
//go:nosplit
func write1Bytes(fd uintptr, b []byte) int32 {
	n := uint32(len(b))
	if n == 0 {
		return 0
	}
	bufOff := wasm.WriteLinearMemory(0, b)
	// iovec: { uint32 buf; uint32 buf_len } — 8 bytes, little-endian.
	var iovBytes [8]byte
	iovBytes[0] = byte(bufOff)
	iovBytes[1] = byte(bufOff >> 8)
	iovBytes[2] = byte(bufOff >> 16)
	iovBytes[3] = byte(bufOff >> 24)
	iovBytes[4] = byte(n)
	iovBytes[5] = byte(n >> 8)
	iovBytes[6] = byte(n >> 16)
	iovBytes[7] = byte(n >> 24)
	iovOff := wasm.WriteLinearMemory(0, iovBytes[:])
	// nwritten out-cell — 4 zero bytes the host writes into.
	var nwBytes [4]byte
	nwOff := wasm.WriteLinearMemory(0, nwBytes[:])
	rc := fd_write(int32(fd), iovOff, 1, nwOff)
	wasm.ResetLinearMemory(0, bufOff)
	if rc != 0 {
		throw("fd_write failed")
	}
	return int32(n)
}
