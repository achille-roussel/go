// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// Phase 2 write path for GOOS=js GOARCH=wasm3: stage the byte
// slice in the linear-memory bridge arena and call a 3-i32-param
// wasmimport that copies (mem[off:off+n]) to process.stdout /
// stderr by fd.

package runtime

import (
	"runtime/wasm"
	"unsafe"
)

//go:wasmimport gojs runtime.writeBytes
func wasmJSWriteBytes(fd uint32, off uint32, n uint32)

// write1Bytes is pulled by syscall.Write on js/wasm3 (which routes
// stdio writes through the runtime instead of syscall/js — there's
// no JS bridge on wasm3). The //go:linkname directive marks the
// symbol as a push-target so the linker's cross-package linkname
// allow-check passes.
//
//go:linkname write1Bytes
//go:nosplit
func write1Bytes(fd uintptr, b []byte) int32 {
	n := uint32(len(b))
	if n == 0 {
		return 0
	}
	off := wasm.WriteLinearMemory(0, b)
	wasmJSWriteBytes(uint32(fd), off, n)
	wasm.ResetLinearMemory(0, off)
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
