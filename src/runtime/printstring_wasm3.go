// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// printstring for GOARCH=wasm3 aliases the string's $go.bytes
// backing as a []byte slice and hands it to write1Bytes. No copy:
// the bridge's WriteLinearMemory walks the slice's backing
// directly via array.get_u $go.bytes.
//
//go:nosplit
func printstring(s string) {
	if len(s) == 0 {
		return
	}
	write1Bytes(2, unsafe.Slice(unsafe.StringData(s), len(s)))
}
