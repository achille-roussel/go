// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "runtime/wasm"

// gwrite for GOARCH=wasm3 stages the byte slice in the linear-
// memory bridge arena and hands the offset to wasm3WriteBytes.
// Goroutine-buffered output (g.writebuf, recordForPanic) is
// skipped — no current wasm3 program reaches those paths.
//
//go:nosplit
func gwrite(b []byte) {
	n := uint32(len(b))
	if n == 0 {
		return
	}
	off := wasm.WriteLinearMemory(0, b)
	wasm3WriteBytes(2, off, n)
	wasm.ResetLinearMemory(0, off)
}
