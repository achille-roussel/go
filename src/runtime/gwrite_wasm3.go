// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// gwrite for GOARCH=wasm3 forwards to write1Bytes; the linear-
// memory bridge marshalling lives there. Goroutine-buffered output
// (g.writebuf, recordForPanic) is skipped — no current wasm3
// program reaches those paths.
//
//go:nosplit
func gwrite(b []byte) {
	if len(b) == 0 {
		return
	}
	write1Bytes(2, b)
}
