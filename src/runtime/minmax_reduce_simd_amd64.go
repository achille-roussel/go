// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simd

package runtime

import (
	"simd/archsimd"
	"unsafe"
)

// This file provides vectorized min/max reduction kernels. The compiler
// rewrites reduction loops of the form
//
//	for _, v := range x { m = min(m, v) }
//
// into calls to these functions when GOEXPERIMENT=simd is enabled; see
// cmd/compile/internal/walk/minmax.go.
//
// Each kernel folds the elements of d into the seed m and returns the
// result; d may be empty. The vector paths never fall back to scalar
// code for the remainder: the tail is handled by reloading full vectors
// overlapping already processed elements, which is safe because min and
// max are idempotent. The final 256-bit accumulator is reduced by
// storing it to a stack buffer and running the scalar loop over it.
//
// The trailing scalar loops are themselves reduction loops of the form
// the compiler rewrites; the rewrite skips the runtime package (see
// minmaxEnabled in cmd/compile/internal/walk/minmax.go), which is what
// keeps these kernels from calling themselves.

// slicecast returns s reinterpreted as a slice of To-typed elements
// sharing the same backing array, with the length scaled by the ratio
// of the element sizes; trailing bytes of s that do not fill a To are
// dropped. The kernels use it to view their input as a slice of
// chunk-sized arrays: indexing the chunks with a range loop and
// slicing them with constant bounds is provably in range, which keeps
// the vector loops free of bounds checks.
func slicecast[To, From any](s []From) []To {
	n := uintptr(len(s)) * unsafe.Sizeof(*new(From)) / unsafe.Sizeof(*new(To))
	return unsafe.Slice((*To)(unsafe.Pointer(unsafe.SliceData(s))), n)
}

func minUint8(d []uint8, m uint8) uint8 {
	if archsimd.X86.AVX2() && len(d) >= 64 {
		acc0 := archsimd.BroadcastUint8x32(m)
		acc1 := acc0
		chunks := slicecast[[64]uint8](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadUint8x32(c[0:32]))
			acc1 = acc1.Min(archsimd.LoadUint8x32(c[32:64]))
		}
		if rem := len(d) - len(chunks)*64; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadUint8x32(d[len(d)-32:]))
			if rem > 32 {
				acc1 = acc1.Min(archsimd.LoadUint8x32(d[len(d)-64:]))
			}
		}
		acc0 = acc0.Min(acc1)
		var buf [32]uint8
		acc0.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m = buf[0]
		d = buf[1:]
	}
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxUint8(d []uint8, m uint8) uint8 {
	if archsimd.X86.AVX2() && len(d) >= 64 {
		acc0 := archsimd.BroadcastUint8x32(m)
		acc1 := acc0
		chunks := slicecast[[64]uint8](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadUint8x32(c[0:32]))
			acc1 = acc1.Max(archsimd.LoadUint8x32(c[32:64]))
		}
		if rem := len(d) - len(chunks)*64; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadUint8x32(d[len(d)-32:]))
			if rem > 32 {
				acc1 = acc1.Max(archsimd.LoadUint8x32(d[len(d)-64:]))
			}
		}
		acc0 = acc0.Max(acc1)
		var buf [32]uint8
		acc0.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m = buf[0]
		d = buf[1:]
	}
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minInt16(d []int16, m int16) int16 {
	if archsimd.X86.AVX2() && len(d) >= 32 {
		acc0 := archsimd.BroadcastInt16x16(m)
		acc1 := acc0
		chunks := slicecast[[32]int16](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadInt16x16(c[0:16]))
			acc1 = acc1.Min(archsimd.LoadInt16x16(c[16:32]))
		}
		if rem := len(d) - len(chunks)*32; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadInt16x16(d[len(d)-16:]))
			if rem > 16 {
				acc1 = acc1.Min(archsimd.LoadInt16x16(d[len(d)-32:]))
			}
		}
		acc0 = acc0.Min(acc1)
		var buf [16]int16
		acc0.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m = buf[0]
		d = buf[1:]
	}
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxInt16(d []int16, m int16) int16 {
	if archsimd.X86.AVX2() && len(d) >= 32 {
		acc0 := archsimd.BroadcastInt16x16(m)
		acc1 := acc0
		chunks := slicecast[[32]int16](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadInt16x16(c[0:16]))
			acc1 = acc1.Max(archsimd.LoadInt16x16(c[16:32]))
		}
		if rem := len(d) - len(chunks)*32; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadInt16x16(d[len(d)-16:]))
			if rem > 16 {
				acc1 = acc1.Max(archsimd.LoadInt16x16(d[len(d)-32:]))
			}
		}
		acc0 = acc0.Max(acc1)
		var buf [16]int16
		acc0.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m = buf[0]
		d = buf[1:]
	}
	for _, v := range d {
		m = max(m, v)
	}
	return m
}
