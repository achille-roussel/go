// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// wasm3 stubs the float / complex print helpers to placeholder
// strings. The default routes them through gwrite(strconv.
// AppendFloat / AppendComplex), which pulls every internal/
// strconv helper into the link — and several trip wasm3
// validation on the slice-in-arg ABI. Real formatted output
// awaits Stage E composite-marshalling. Programs that want to
// see numeric values can format them as integers via int(v)
// before printing.

func printfloat64(v float64) {
	printstring("<float64>")
}

func printfloat32(v float32) {
	printstring("<float32>")
}

func printcomplex128(c complex128) {
	printstring("<complex128>")
}

func printcomplex64(c complex64) {
	printstring("<complex64>")
}
