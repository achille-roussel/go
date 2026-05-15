// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm3

package runtime

import "internal/strconv"

func printuint(v uint64) {
	// Note: Avoiding strconv.AppendUint so that it's clearer
	// that there are no allocations in this routine.
	// cmd/link/internal/ld.TestAbstractOriginSanity
	// sees the append and doesn't realize it doesn't allocate.
	var buf [20]byte
	i := strconv.RuntimeFormatBase10(buf[:], v)
	gwrite(buf[i:])
}

func printint(v int64) {
	// Note: Avoiding strconv.AppendUint so that it's clearer
	// that there are no allocations in this routine.
	// cmd/link/internal/ld.TestAbstractOriginSanity
	// sees the append and doesn't realize it doesn't allocate.
	neg := v < 0
	u := uint64(v)
	if neg {
		u = -u
	}
	var buf [20]byte
	i := strconv.RuntimeFormatBase10(buf[:], u)
	if neg {
		i--
		buf[i] = '-'
	}
	gwrite(buf[i:])
}
