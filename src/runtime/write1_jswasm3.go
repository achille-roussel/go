// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// Phase 0 write path for GOOS=js GOARCH=wasm3. Both write1Bytes and
// write1 trap — empty main doesn't print. Phase 2 replaces these
// with calls into a JS-side `__wasm3_write(fd, bytes)` import that
// forwards to process.stdout (Node) or console.log (browser).

package runtime

import "unsafe"

//go:nosplit
func write1Bytes(fd uintptr, b []byte) int32 {
	_ = fd
	_ = b
	throw("js/wasm3 write1Bytes not yet implemented")
	return -1
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
