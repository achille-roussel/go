// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// printuint / printint for GOARCH=wasm3 compose the decimal digits
// in a Stage-D wasmgc stack array (`var buf [21]byte` lowers to
// `(ref (array i8))`), then copy the run of digits, one byte at a
// time, into a package-global linear-memory scratch region and hand
// that pointer to WASI fd_write. The copy loop is the wasmgc <->
// linear-memory bridge — there is no wasm opcode that copies from a
// wasmgc array directly into linear memory, so we do it in Go.
//
// Why we can't take `unsafe.Pointer(&buf[i])` directly: a wasmgc
// array element is not addressable as a linear-memory pointer; the
// host cannot dereference a wasmgc ref. A package-global byte array
// lives in the data section, which IS linear memory, so
// `&printnumScratch[0]` materialises a real i32 pointer fd_write
// can consume.
//
// The bridge is inlined into each caller rather than factored into
// a helper because the wasm3 backend does not yet bridge a wasmgc
// ref across a function-call boundary — passing `*[21]byte` lowers
// the parameter to i64, and the caller-side push of an anyref local
// would fail wasm validation. Stage I (wasmexport composite
// marshalling) sketches the ref-typed-parameter ABI that would
// retire the manual inlining.
//
// Single-goroutine wasm3 lets us share one global scratch buffer
// across both print routines. When the goroutine machinery's own
// milestone (M4) brings up real parking, a per-M scratch will be
// needed.

const printnumScratchSize = 21 // -9223372036854775808 is 20 chars

var printnumScratch [printnumScratchSize]byte

//go:nosplit
func printuint(v uint64) {
	var buf [printnumScratchSize]byte
	i := int32(printnumScratchSize - 1)
	for {
		i--
		buf[i] = byte(v%10) + '0'
		v /= 10
		if v == 0 {
			break
		}
	}
	n := int32(printnumScratchSize-1) - i
	for j := int32(0); j < n; j++ {
		printnumScratch[j] = buf[i+j]
	}
	write1(2, unsafe.Pointer(&printnumScratch[0]), n)
}

//go:nosplit
func printint(v int64) {
	neg := v < 0
	u := uint64(v)
	if neg {
		u = -u
	}
	var buf [printnumScratchSize]byte
	i := int32(printnumScratchSize - 1)
	for {
		i--
		buf[i] = byte(u%10) + '0'
		u /= 10
		if u == 0 {
			break
		}
	}
	if neg {
		i--
		buf[i] = '-'
	}
	n := int32(printnumScratchSize-1) - i
	for j := int32(0); j < n; j++ {
		printnumScratch[j] = buf[i+j]
	}
	write1(2, unsafe.Pointer(&printnumScratch[0]), n)
}
