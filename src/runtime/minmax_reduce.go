// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.simd || !amd64

package runtime

// Scalar fallbacks for the min/max reduction kernels; see
// minmax_reduce_simd_amd64.go. The compiler only rewrites reduction
// loops into calls to these functions on configurations where the
// vector versions exist, so these definitions just keep the runtime
// package definition complete on other configurations. In particular,
// this file is only compiled when that rewriting is disabled, so its
// loops cannot be rewritten into calls to themselves.

func minUint8(d []uint8, m uint8) uint8 {
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxUint8(d []uint8, m uint8) uint8 {
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minInt8(d []int8, m int8) int8 {
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxInt8(d []int8, m int8) int8 {
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minInt16(d []int16, m int16) int16 {
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxInt16(d []int16, m int16) int16 {
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minUint16(d []uint16, m uint16) uint16 {
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxUint16(d []uint16, m uint16) uint16 {
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minInt32(d []int32, m int32) int32 {
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxInt32(d []int32, m int32) int32 {
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minUint32(d []uint32, m uint32) uint32 {
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxUint32(d []uint32, m uint32) uint32 {
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minInt64(d []int64, m int64) int64 {
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxInt64(d []int64, m int64) int64 {
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minUint64(d []uint64, m uint64) uint64 {
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxUint64(d []uint64, m uint64) uint64 {
	for _, v := range d {
		m = max(m, v)
	}
	return m
}
