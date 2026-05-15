// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// printstring for GOARCH=wasm3 calls write1 directly with the
// string's underlying pointer and length, bypassing the bytes()
// helper. bytes() reflects on a goroutine-stack-allocated slice
// header and would emit `Get $name(SP)` — the wasm3 obj backend
// bails on SP-relative addressing, and boxing the slice through
// WasmGC is a later rung.
func printstring(s string) {
	if len(s) == 0 {
		return
	}
	write1(2, unsafe.Pointer(unsafe.StringData(s)), int32(len(s)))
}
