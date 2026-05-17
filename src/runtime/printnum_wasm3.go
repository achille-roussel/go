// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// printuint / printint for GOARCH=wasm3 share a package-global
// [21]byte buffer rather than the `var buf [20]byte` stack
// pattern the standard implementation in printnum.go uses.
//
// M3 Stage D now lowers `var buf [N]byte` to a wasmgc
// `(ref (array i8))`, but write1 — the WASI fd_write bridge —
// takes a linear-memory pointer for the buffer (the host can't
// dereference a wasmgc ref). `unsafe.Pointer(&buf[i])` on a
// ref-backed array can't produce that pointer; the SSA value
// would be the array ref (anyref) where i64 is expected,
// yielding a wasmtime validation failure.
//
// A package-global byte array IS in linear memory (lives in the
// data section), so &printnumBuf[i] gives the linear-memory
// pointer write1 needs. The trade is single-goroutine: the
// global is shared. That's fine while the wasm3 runtime stays
// single-goroutine — the goroutine machinery's own bring-up
// has its own milestone.
//
// Full retirement of this shim needs a wasmgc <-> linear-memory
// bridge for I/O (array.copy from buf to a scratch region, or
// a WASI 0.3 / component-model array-aware fd_write). Either is
// a Stage I-ish piece of work.
var printnumBuf [21]byte

func formatUint10(v uint64) int {
	i := 20
	for {
		i--
		printnumBuf[i] = byte(v%10) + '0'
		v /= 10
		if v == 0 {
			break
		}
	}
	return i
}

func printuint(v uint64) {
	i := formatUint10(v)
	write1(2, unsafe.Pointer(&printnumBuf[i]), int32(20-i))
}

func printint(v int64) {
	neg := v < 0
	u := uint64(v)
	if neg {
		u = -u
	}
	i := formatUint10(u)
	if neg {
		i--
		printnumBuf[i] = '-'
	}
	write1(2, unsafe.Pointer(&printnumBuf[i]), int32(20-i))
}
