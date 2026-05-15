// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasip1 && !wasm3

package runtime

import "unsafe"

func write1(fd uintptr, p unsafe.Pointer, n int32) int32 {
	iov := iovec{
		buf:    uintptr32(uintptr(p)),
		bufLen: size(n),
	}
	var nwritten size
	if fd_write(int32(fd), unsafe.Pointer(&iov), 1, &nwritten) != 0 {
		throw("fd_write failed")
	}
	return int32(nwritten)
}
