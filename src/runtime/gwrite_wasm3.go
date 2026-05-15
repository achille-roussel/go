// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// gwrite for GOARCH=wasm3 bypasses the goroutine-local writebuf and
// the recordForPanic backlog: both depend on a g0/m0 that the M2
// cutover has not yet wired up. It writes b directly to fd 2 (stderr)
// via the WASI fd_write syscall.
//
// The simplification is the runtime fork's first real-output piece —
// callers like printint go through here at the bottom of every print
// pipeline, so any program that would print a value gets that value
// to stderr.
func gwrite(b []byte) {
	if len(b) == 0 {
		return
	}
	write1(2, unsafe.Pointer(&b[0]), int32(len(b)))
}
