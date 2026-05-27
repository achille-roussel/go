// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "runtime/wasm"

// printstring for GOARCH=wasm3 copies the string's bytes into a
// stack-allocated wasmgc [N]byte (lowers to array.new_default
// $go.bytes), stages that slice in the linear-memory bridge arena,
// and calls wasm3WriteBytes with the resulting offset. `s[i]`
// lowers to OpWasm3StringByte (array.get_u on the $go.string's
// backing at off+i) — the SSA-level lowering needed for this exact
// pattern.
//
// Strings longer than the scratch buffer are truncated. A chunked
// version would need to substring s, which currently fails wasm
// validation under the string-literal-as-ref fold gate; acceptable
// for runtime prints.
//
//go:nosplit
func printstring(s string) {
	n := len(s)
	if n == 0 {
		return
	}
	var buf [4096]byte
	if n > len(buf) {
		n = len(buf)
	}
	for i := 0; i < n; i++ {
		buf[i] = s[i]
	}
	off := wasm.WriteLinearMemory(0, buf[:n])
	wasm3WriteBytes(2, off, uint32(n))
	wasm.ResetLinearMemory(0, off)
}
