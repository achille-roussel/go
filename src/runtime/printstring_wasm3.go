// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// printstring for GOARCH=wasm3 calls write1 directly with the
// string's underlying pointer and length, bypassing the slice
// header gwrite path — recordForPanic + the gwrite writebuf
// check both run code that the wasm3 obj backend hasn't grown
// support for yet (live g.m fields, slice-on-stack-array via
// Stage C). Retiring this shim is gated on those.
func printstring(s string) {
	if len(s) == 0 {
		return
	}
	write1(2, unsafe.Pointer(unsafe.StringData(s)), int32(len(s)))
}
