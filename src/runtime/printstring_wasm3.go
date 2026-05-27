// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// printstring for GOARCH=wasm3 copies the string's bytes into a
// stack-allocated wasmgc [N]byte (lowers to array.new_default
// $go.bytes), then hands the slice to write1Bytes which marshals
// it through the linear-memory bridge. `s[i]` lowers to
// OpWasm3StringByte (array.get_u on the $go.string's backing at
// off+i). For strings longer than the scratch buffer, sub-slice
// the source and loop — OpWasm3SubString builds a fresh
// $go.string header sharing the original bytes backing.
//
//go:nosplit
func printstring(s string) {
	for len(s) > 0 {
		var buf [4096]byte
		k := len(s)
		if k > len(buf) {
			k = len(buf)
		}
		for i := 0; i < k; i++ {
			buf[i] = s[i]
		}
		write1Bytes(2, buf[:k])
		s = s[k:]
	}
}
