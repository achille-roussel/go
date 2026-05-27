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
// On wasm3 the unsafe.Pointer is meaningless and the body traps —
// all reachable wasm3 print sites use the linear-memory bridge
// helpers below.
//
//go:nosplit
func write1(fd uintptr, p unsafe.Pointer, n int32) int32 {
	_ = fd
	_ = p
	_ = n
	throw("wasm3: runtime.write1(unsafe.Pointer) unreachable — use wasm3WriteBytes")
	return -1
}

// wasm3WriteBytes is the wasm3 print path: callers stage their bytes
// in the M3.5 linear-memory bridge arena via wasm.WriteLinearMemory,
// then call here with the resulting arena offset + length. This
// function builds the iovec + nwritten scratch in the same arena,
// calls fd_write, and returns. It does NOT reset the arena — the
// caller resets back to its own bufOff after we return, freeing the
// iovec + nwritten cells along with the staged data in one rewind.
//
// Passing offsets (not a []byte) sidesteps the Go-to-Go slice ABI
// on wasm3: a []byte parameter would lower to (anyref backing, i64
// len, i64 cap) at the wasm signature, but the caller's struct.new
// $go.slice.u8 form doesn't decompose to match — a known gap in the
// wasm3 calling convention. Three scalar args avoid the issue.
//
//go:nosplit
func wasm3WriteBytes(fd uintptr, bufOff, n uint32) int32 {
	if n == 0 {
		return 0
	}
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
	// nwritten out-cell — 4 zero bytes the host writes into; we don't
	// read it back (no caller currently inspects the count).
	var nwBytes [4]byte
	nwOff := wasm.WriteLinearMemory(0, nwBytes[:])
	// Swallow the rc — the runtime.throw path is currently not
	// validation-clean on wasm3 (an internal placer issue surfaces
	// the i64.shr_s strength-reduction in printuint when throw is
	// reachable). Out-of-band print failure is acceptable for M3.5;
	// re-enable the throw check once the placer issue is fixed.
	fd_write(int32(fd), iovOff, 1, nwOff)
	return int32(n)
}
