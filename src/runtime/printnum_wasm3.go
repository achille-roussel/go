// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

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
