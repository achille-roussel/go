// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "runtime/wasm"

// printuint / printint for GOARCH=wasm3 compose the decimal digits
// in a Stage-D wasmgc stack array (`var buf [21]byte` lowers to
// `(ref (array i8))`), stage the digit run in the linear-memory
// bridge arena, and call wasm3WriteBytes with the resulting
// offset. No package-global scratch buffer needed; the bridge
// arena owns the temporary storage and the reset rewinds the
// whole sequence.

const printnumScratchSize = 21 // -9223372036854775808 is 20 chars

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
	off := wasm.WriteLinearMemory(0, buf[i:])
	wasm3WriteBytes(2, off, uint32(printnumScratchSize-1-int(i)))
	wasm.ResetLinearMemory(0, off)
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
	off := wasm.WriteLinearMemory(0, buf[i:])
	wasm3WriteBytes(2, off, uint32(printnumScratchSize-1-int(i)))
	wasm.ResetLinearMemory(0, off)
}
