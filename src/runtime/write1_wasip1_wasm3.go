// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasip1 && wasm3

package runtime

import "unsafe"

// write1Iov and write1Nwritten are the scratch space write1 hands to
// fd_write. Promoted from stack-locals so the wasm3 obj backend
// doesn't have to address them through SP — the M2 cutover boxes
// escaping &local addresses through WasmGC structs in a later rung;
// until that lands, hoisting the few WASI scratch buffers to globals
// is the simplest way to keep write1 available. Single-goroutine
// wasm3 has no concurrency hazard.
var (
	write1Iov      iovec
	write1Nwritten size
)

func write1(fd uintptr, p unsafe.Pointer, n int32) int32 {
	write1Iov.buf = uintptr32(uintptr(p))
	write1Iov.bufLen = size(n)
	if fd_write(int32(fd), unsafe.Pointer(&write1Iov), 1, &write1Nwritten) != 0 {
		throw("fd_write failed")
	}
	return int32(write1Nwritten)
}
