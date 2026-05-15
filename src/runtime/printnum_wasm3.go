// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// printnumBuf is the formatting scratch space printuint and printint
// share. Promoted from a stack-local `var buf [20]byte` to a package
// global so the wasm3 obj backend doesn't have to address it through
// SP — the M2 cutover boxes escaping &local addresses through WasmGC
// structs in a later rung; until that lands, hoisting the few runtime
// scratch buffers to globals is the simplest way to keep print
// available for diagnostics. Single-goroutine wasm3 has no concurrency
// hazard for sharing this buffer.
//
// 21 bytes is enough for an int64 (-9223372036854775808 = 20 digits +
// sign).
var printnumBuf [21]byte

// formatUint10 writes the base-10 representation of v into
// printnumBuf, starting from the right. Returns the index of the first
// byte written. Open-coded so the body uses only globals and
// registers — no stack-allocated slice — which is what the wasm3
// encoder can express without boxing.
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
