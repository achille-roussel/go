// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// printuint / printint for GOARCH=wasm3 compose the decimal digits
// in a Stage-D wasmgc stack array (`var buf [21]byte` lowers to
// `(ref (array i8))`), then forward the digit run as a []byte slice
// to write1Bytes. The slice-arg call boundary works cleanly under
// the unified single-anyref slice ABI (flatPrimitiveFields TSLICE
// → one anyref).

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
	write1Bytes(2, buf[i:])
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
	write1Bytes(2, buf[i:])
}
