// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm3

package runtime

import "internal/strconv"

// float64 requires 1+17+1+1+1+3 = 24 bytes max (sign+digits+decimal point+e+sign+exponent digits).
const float64Bytes = 24

func printfloat64(v float64) {
	var buf [float64Bytes]byte
	gwrite(strconv.AppendFloat(buf[:0], v, 'g', -1, 64))
}

// float32 requires 1+9+1+1+1+2 = 15 bytes max (sign+digits+decimal point+e+sign+exponent digits).
const float32Bytes = 15

func printfloat32(v float32) {
	var buf [float32Bytes]byte
	gwrite(strconv.AppendFloat(buf[:0], float64(v), 'g', -1, 32))
}

// complex128 requires 24+24+1+1+1 = 51 bytes max (paren+float64+float64+i+paren).
const complex128Bytes = 2*float64Bytes + 3

func printcomplex128(c complex128) {
	var buf [complex128Bytes]byte
	gwrite(strconv.AppendComplex(buf[:0], c, 'g', -1, 128))
}

// complex64 requires 15+15+1+1+1 = 33 bytes max (paren+float32+float32+i+paren).
const complex64Bytes = 2*float32Bytes + 3

func printcomplex64(c complex64) {
	var buf [complex64Bytes]byte
	gwrite(strconv.AppendComplex(buf[:0], complex128(c), 'g', -1, 64))
}
