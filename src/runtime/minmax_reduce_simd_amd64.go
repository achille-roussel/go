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
// result; d may be empty. Each has an AVX-512 tier (two 512-bit
// accumulators per iteration; archsimd.X86.AVX512 implies AVX512BW, so
// byte and word lanes are available) and an AVX2 tier (two 256-bit
// accumulators). The vector tiers never fall back to scalar code for
// the remainder: the tail is handled by reloading full vectors
// overlapping already processed elements, which is safe because min and
// max are idempotent. The final vector accumulator is stored to a stack
// buffer whose reduction reuses the trailing scalar loop.
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
	switch {
	case archsimd.X86.AVX512() && len(d) >= 128:
		acc0 := archsimd.BroadcastUint8x64(m)
		acc1 := acc0
		chunks := slicecast[[128]uint8](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadUint8x64(c[0:64]))
			acc1 = acc1.Min(archsimd.LoadUint8x64(c[64:128]))
		}
		if rem := len(d) - len(chunks)*128; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadUint8x64(d[len(d)-64:]))
			if rem > 64 {
				acc1 = acc1.Min(archsimd.LoadUint8x64(d[len(d)-128:]))
			}
		}
		a := acc0.Min(acc1)
		h := a.GetLo().Min(a.GetHi())
		var buf [32]uint8
		h.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 64:
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
		var buf [32]uint8
		acc0.Min(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxUint8(d []uint8, m uint8) uint8 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 128:
		acc0 := archsimd.BroadcastUint8x64(m)
		acc1 := acc0
		chunks := slicecast[[128]uint8](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadUint8x64(c[0:64]))
			acc1 = acc1.Max(archsimd.LoadUint8x64(c[64:128]))
		}
		if rem := len(d) - len(chunks)*128; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadUint8x64(d[len(d)-64:]))
			if rem > 64 {
				acc1 = acc1.Max(archsimd.LoadUint8x64(d[len(d)-128:]))
			}
		}
		a := acc0.Max(acc1)
		h := a.GetLo().Max(a.GetHi())
		var buf [32]uint8
		h.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 64:
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
		var buf [32]uint8
		acc0.Max(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minInt8(d []int8, m int8) int8 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 128:
		acc0 := archsimd.BroadcastInt8x64(m)
		acc1 := acc0
		chunks := slicecast[[128]int8](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadInt8x64(c[0:64]))
			acc1 = acc1.Min(archsimd.LoadInt8x64(c[64:128]))
		}
		if rem := len(d) - len(chunks)*128; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadInt8x64(d[len(d)-64:]))
			if rem > 64 {
				acc1 = acc1.Min(archsimd.LoadInt8x64(d[len(d)-128:]))
			}
		}
		a := acc0.Min(acc1)
		h := a.GetLo().Min(a.GetHi())
		var buf [32]int8
		h.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 64:
		acc0 := archsimd.BroadcastInt8x32(m)
		acc1 := acc0
		chunks := slicecast[[64]int8](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadInt8x32(c[0:32]))
			acc1 = acc1.Min(archsimd.LoadInt8x32(c[32:64]))
		}
		if rem := len(d) - len(chunks)*64; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadInt8x32(d[len(d)-32:]))
			if rem > 32 {
				acc1 = acc1.Min(archsimd.LoadInt8x32(d[len(d)-64:]))
			}
		}
		var buf [32]int8
		acc0.Min(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxInt8(d []int8, m int8) int8 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 128:
		acc0 := archsimd.BroadcastInt8x64(m)
		acc1 := acc0
		chunks := slicecast[[128]int8](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadInt8x64(c[0:64]))
			acc1 = acc1.Max(archsimd.LoadInt8x64(c[64:128]))
		}
		if rem := len(d) - len(chunks)*128; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadInt8x64(d[len(d)-64:]))
			if rem > 64 {
				acc1 = acc1.Max(archsimd.LoadInt8x64(d[len(d)-128:]))
			}
		}
		a := acc0.Max(acc1)
		h := a.GetLo().Max(a.GetHi())
		var buf [32]int8
		h.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 64:
		acc0 := archsimd.BroadcastInt8x32(m)
		acc1 := acc0
		chunks := slicecast[[64]int8](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadInt8x32(c[0:32]))
			acc1 = acc1.Max(archsimd.LoadInt8x32(c[32:64]))
		}
		if rem := len(d) - len(chunks)*64; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadInt8x32(d[len(d)-32:]))
			if rem > 32 {
				acc1 = acc1.Max(archsimd.LoadInt8x32(d[len(d)-64:]))
			}
		}
		var buf [32]int8
		acc0.Max(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minInt16(d []int16, m int16) int16 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 64:
		acc0 := archsimd.BroadcastInt16x32(m)
		acc1 := acc0
		chunks := slicecast[[64]int16](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadInt16x32(c[0:32]))
			acc1 = acc1.Min(archsimd.LoadInt16x32(c[32:64]))
		}
		if rem := len(d) - len(chunks)*64; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadInt16x32(d[len(d)-32:]))
			if rem > 32 {
				acc1 = acc1.Min(archsimd.LoadInt16x32(d[len(d)-64:]))
			}
		}
		a := acc0.Min(acc1)
		h := a.GetLo().Min(a.GetHi())
		var buf [16]int16
		h.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 32:
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
		var buf [16]int16
		acc0.Min(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxInt16(d []int16, m int16) int16 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 64:
		acc0 := archsimd.BroadcastInt16x32(m)
		acc1 := acc0
		chunks := slicecast[[64]int16](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadInt16x32(c[0:32]))
			acc1 = acc1.Max(archsimd.LoadInt16x32(c[32:64]))
		}
		if rem := len(d) - len(chunks)*64; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadInt16x32(d[len(d)-32:]))
			if rem > 32 {
				acc1 = acc1.Max(archsimd.LoadInt16x32(d[len(d)-64:]))
			}
		}
		a := acc0.Max(acc1)
		h := a.GetLo().Max(a.GetHi())
		var buf [16]int16
		h.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 32:
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
		var buf [16]int16
		acc0.Max(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minUint16(d []uint16, m uint16) uint16 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 64:
		acc0 := archsimd.BroadcastUint16x32(m)
		acc1 := acc0
		chunks := slicecast[[64]uint16](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadUint16x32(c[0:32]))
			acc1 = acc1.Min(archsimd.LoadUint16x32(c[32:64]))
		}
		if rem := len(d) - len(chunks)*64; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadUint16x32(d[len(d)-32:]))
			if rem > 32 {
				acc1 = acc1.Min(archsimd.LoadUint16x32(d[len(d)-64:]))
			}
		}
		a := acc0.Min(acc1)
		h := a.GetLo().Min(a.GetHi())
		var buf [16]uint16
		h.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 32:
		acc0 := archsimd.BroadcastUint16x16(m)
		acc1 := acc0
		chunks := slicecast[[32]uint16](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadUint16x16(c[0:16]))
			acc1 = acc1.Min(archsimd.LoadUint16x16(c[16:32]))
		}
		if rem := len(d) - len(chunks)*32; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadUint16x16(d[len(d)-16:]))
			if rem > 16 {
				acc1 = acc1.Min(archsimd.LoadUint16x16(d[len(d)-32:]))
			}
		}
		var buf [16]uint16
		acc0.Min(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxUint16(d []uint16, m uint16) uint16 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 64:
		acc0 := archsimd.BroadcastUint16x32(m)
		acc1 := acc0
		chunks := slicecast[[64]uint16](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadUint16x32(c[0:32]))
			acc1 = acc1.Max(archsimd.LoadUint16x32(c[32:64]))
		}
		if rem := len(d) - len(chunks)*64; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadUint16x32(d[len(d)-32:]))
			if rem > 32 {
				acc1 = acc1.Max(archsimd.LoadUint16x32(d[len(d)-64:]))
			}
		}
		a := acc0.Max(acc1)
		h := a.GetLo().Max(a.GetHi())
		var buf [16]uint16
		h.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 32:
		acc0 := archsimd.BroadcastUint16x16(m)
		acc1 := acc0
		chunks := slicecast[[32]uint16](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadUint16x16(c[0:16]))
			acc1 = acc1.Max(archsimd.LoadUint16x16(c[16:32]))
		}
		if rem := len(d) - len(chunks)*32; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadUint16x16(d[len(d)-16:]))
			if rem > 16 {
				acc1 = acc1.Max(archsimd.LoadUint16x16(d[len(d)-32:]))
			}
		}
		var buf [16]uint16
		acc0.Max(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minInt32(d []int32, m int32) int32 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 32:
		acc0 := archsimd.BroadcastInt32x16(m)
		acc1 := acc0
		chunks := slicecast[[32]int32](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadInt32x16(c[0:16]))
			acc1 = acc1.Min(archsimd.LoadInt32x16(c[16:32]))
		}
		if rem := len(d) - len(chunks)*32; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadInt32x16(d[len(d)-16:]))
			if rem > 16 {
				acc1 = acc1.Min(archsimd.LoadInt32x16(d[len(d)-32:]))
			}
		}
		a := acc0.Min(acc1)
		h := a.GetLo().Min(a.GetHi())
		var buf [8]int32
		h.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 16:
		acc0 := archsimd.BroadcastInt32x8(m)
		acc1 := acc0
		chunks := slicecast[[16]int32](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadInt32x8(c[0:8]))
			acc1 = acc1.Min(archsimd.LoadInt32x8(c[8:16]))
		}
		if rem := len(d) - len(chunks)*16; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadInt32x8(d[len(d)-8:]))
			if rem > 8 {
				acc1 = acc1.Min(archsimd.LoadInt32x8(d[len(d)-16:]))
			}
		}
		var buf [8]int32
		acc0.Min(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxInt32(d []int32, m int32) int32 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 32:
		acc0 := archsimd.BroadcastInt32x16(m)
		acc1 := acc0
		chunks := slicecast[[32]int32](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadInt32x16(c[0:16]))
			acc1 = acc1.Max(archsimd.LoadInt32x16(c[16:32]))
		}
		if rem := len(d) - len(chunks)*32; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadInt32x16(d[len(d)-16:]))
			if rem > 16 {
				acc1 = acc1.Max(archsimd.LoadInt32x16(d[len(d)-32:]))
			}
		}
		a := acc0.Max(acc1)
		h := a.GetLo().Max(a.GetHi())
		var buf [8]int32
		h.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 16:
		acc0 := archsimd.BroadcastInt32x8(m)
		acc1 := acc0
		chunks := slicecast[[16]int32](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadInt32x8(c[0:8]))
			acc1 = acc1.Max(archsimd.LoadInt32x8(c[8:16]))
		}
		if rem := len(d) - len(chunks)*16; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadInt32x8(d[len(d)-8:]))
			if rem > 8 {
				acc1 = acc1.Max(archsimd.LoadInt32x8(d[len(d)-16:]))
			}
		}
		var buf [8]int32
		acc0.Max(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minUint32(d []uint32, m uint32) uint32 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 32:
		acc0 := archsimd.BroadcastUint32x16(m)
		acc1 := acc0
		chunks := slicecast[[32]uint32](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadUint32x16(c[0:16]))
			acc1 = acc1.Min(archsimd.LoadUint32x16(c[16:32]))
		}
		if rem := len(d) - len(chunks)*32; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadUint32x16(d[len(d)-16:]))
			if rem > 16 {
				acc1 = acc1.Min(archsimd.LoadUint32x16(d[len(d)-32:]))
			}
		}
		a := acc0.Min(acc1)
		h := a.GetLo().Min(a.GetHi())
		var buf [8]uint32
		h.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 16:
		acc0 := archsimd.BroadcastUint32x8(m)
		acc1 := acc0
		chunks := slicecast[[16]uint32](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadUint32x8(c[0:8]))
			acc1 = acc1.Min(archsimd.LoadUint32x8(c[8:16]))
		}
		if rem := len(d) - len(chunks)*16; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadUint32x8(d[len(d)-8:]))
			if rem > 8 {
				acc1 = acc1.Min(archsimd.LoadUint32x8(d[len(d)-16:]))
			}
		}
		var buf [8]uint32
		acc0.Min(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxUint32(d []uint32, m uint32) uint32 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 32:
		acc0 := archsimd.BroadcastUint32x16(m)
		acc1 := acc0
		chunks := slicecast[[32]uint32](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadUint32x16(c[0:16]))
			acc1 = acc1.Max(archsimd.LoadUint32x16(c[16:32]))
		}
		if rem := len(d) - len(chunks)*32; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadUint32x16(d[len(d)-16:]))
			if rem > 16 {
				acc1 = acc1.Max(archsimd.LoadUint32x16(d[len(d)-32:]))
			}
		}
		a := acc0.Max(acc1)
		h := a.GetLo().Max(a.GetHi())
		var buf [8]uint32
		h.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 16:
		acc0 := archsimd.BroadcastUint32x8(m)
		acc1 := acc0
		chunks := slicecast[[16]uint32](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadUint32x8(c[0:8]))
			acc1 = acc1.Max(archsimd.LoadUint32x8(c[8:16]))
		}
		if rem := len(d) - len(chunks)*16; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadUint32x8(d[len(d)-8:]))
			if rem > 8 {
				acc1 = acc1.Max(archsimd.LoadUint32x8(d[len(d)-16:]))
			}
		}
		var buf [8]uint32
		acc0.Max(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

// The 64-bit AVX2 tiers below cannot use Min/Max: VPMINSQ, VPMAXSQ,
// VPMINUQ and VPMAXUQ only exist in AVX-512. They select with a compare
// and blend instead; the unsigned Greater is emulated by archsimd with
// a sign-bias and signed compare, which is still AVX2-only.

func minInt64(d []int64, m int64) int64 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 16:
		acc0 := archsimd.BroadcastInt64x8(m)
		acc1 := acc0
		chunks := slicecast[[16]int64](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadInt64x8(c[0:8]))
			acc1 = acc1.Min(archsimd.LoadInt64x8(c[8:16]))
		}
		if rem := len(d) - len(chunks)*16; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadInt64x8(d[len(d)-8:]))
			if rem > 8 {
				acc1 = acc1.Min(archsimd.LoadInt64x8(d[len(d)-16:]))
			}
		}
		var buf [8]int64
		acc0.Min(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 8:
		acc0 := archsimd.BroadcastInt64x4(m)
		acc1 := acc0
		chunks := slicecast[[8]int64](d)
		for j := range chunks {
			c := &chunks[j]
			v0 := archsimd.LoadInt64x4(c[0:4])
			v1 := archsimd.LoadInt64x4(c[4:8])
			acc0 = v0.IfElse(acc0.Greater(v0), acc0)
			acc1 = v1.IfElse(acc1.Greater(v1), acc1)
		}
		if rem := len(d) - len(chunks)*8; rem > 0 {
			t0 := archsimd.LoadInt64x4(d[len(d)-4:])
			acc0 = t0.IfElse(acc0.Greater(t0), acc0)
			if rem > 4 {
				t1 := archsimd.LoadInt64x4(d[len(d)-8:])
				acc1 = t1.IfElse(acc1.Greater(t1), acc1)
			}
		}
		acc0 = acc1.IfElse(acc0.Greater(acc1), acc0)
		var buf [4]int64
		acc0.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxInt64(d []int64, m int64) int64 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 16:
		acc0 := archsimd.BroadcastInt64x8(m)
		acc1 := acc0
		chunks := slicecast[[16]int64](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadInt64x8(c[0:8]))
			acc1 = acc1.Max(archsimd.LoadInt64x8(c[8:16]))
		}
		if rem := len(d) - len(chunks)*16; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadInt64x8(d[len(d)-8:]))
			if rem > 8 {
				acc1 = acc1.Max(archsimd.LoadInt64x8(d[len(d)-16:]))
			}
		}
		var buf [8]int64
		acc0.Max(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 8:
		acc0 := archsimd.BroadcastInt64x4(m)
		acc1 := acc0
		chunks := slicecast[[8]int64](d)
		for j := range chunks {
			c := &chunks[j]
			v0 := archsimd.LoadInt64x4(c[0:4])
			v1 := archsimd.LoadInt64x4(c[4:8])
			acc0 = acc0.IfElse(acc0.Greater(v0), v0)
			acc1 = acc1.IfElse(acc1.Greater(v1), v1)
		}
		if rem := len(d) - len(chunks)*8; rem > 0 {
			t0 := archsimd.LoadInt64x4(d[len(d)-4:])
			acc0 = acc0.IfElse(acc0.Greater(t0), t0)
			if rem > 4 {
				t1 := archsimd.LoadInt64x4(d[len(d)-8:])
				acc1 = acc1.IfElse(acc1.Greater(t1), t1)
			}
		}
		acc0 = acc0.IfElse(acc0.Greater(acc1), acc1)
		var buf [4]int64
		acc0.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minUint64(d []uint64, m uint64) uint64 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 16:
		acc0 := archsimd.BroadcastUint64x8(m)
		acc1 := acc0
		chunks := slicecast[[16]uint64](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Min(archsimd.LoadUint64x8(c[0:8]))
			acc1 = acc1.Min(archsimd.LoadUint64x8(c[8:16]))
		}
		if rem := len(d) - len(chunks)*16; rem > 0 {
			acc0 = acc0.Min(archsimd.LoadUint64x8(d[len(d)-8:]))
			if rem > 8 {
				acc1 = acc1.Min(archsimd.LoadUint64x8(d[len(d)-16:]))
			}
		}
		var buf [8]uint64
		acc0.Min(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 8:
		acc0 := archsimd.BroadcastUint64x4(m)
		acc1 := acc0
		chunks := slicecast[[8]uint64](d)
		for j := range chunks {
			c := &chunks[j]
			v0 := archsimd.LoadUint64x4(c[0:4])
			v1 := archsimd.LoadUint64x4(c[4:8])
			acc0 = v0.IfElse(acc0.Greater(v0), acc0)
			acc1 = v1.IfElse(acc1.Greater(v1), acc1)
		}
		if rem := len(d) - len(chunks)*8; rem > 0 {
			t0 := archsimd.LoadUint64x4(d[len(d)-4:])
			acc0 = t0.IfElse(acc0.Greater(t0), acc0)
			if rem > 4 {
				t1 := archsimd.LoadUint64x4(d[len(d)-8:])
				acc1 = t1.IfElse(acc1.Greater(t1), acc1)
			}
		}
		acc0 = acc1.IfElse(acc0.Greater(acc1), acc0)
		var buf [4]uint64
		acc0.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxUint64(d []uint64, m uint64) uint64 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 16:
		acc0 := archsimd.BroadcastUint64x8(m)
		acc1 := acc0
		chunks := slicecast[[16]uint64](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = acc0.Max(archsimd.LoadUint64x8(c[0:8]))
			acc1 = acc1.Max(archsimd.LoadUint64x8(c[8:16]))
		}
		if rem := len(d) - len(chunks)*16; rem > 0 {
			acc0 = acc0.Max(archsimd.LoadUint64x8(d[len(d)-8:]))
			if rem > 8 {
				acc1 = acc1.Max(archsimd.LoadUint64x8(d[len(d)-16:]))
			}
		}
		var buf [8]uint64
		acc0.Max(acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 8:
		acc0 := archsimd.BroadcastUint64x4(m)
		acc1 := acc0
		chunks := slicecast[[8]uint64](d)
		for j := range chunks {
			c := &chunks[j]
			v0 := archsimd.LoadUint64x4(c[0:4])
			v1 := archsimd.LoadUint64x4(c[4:8])
			acc0 = acc0.IfElse(acc0.Greater(v0), v0)
			acc1 = acc1.IfElse(acc1.Greater(v1), v1)
		}
		if rem := len(d) - len(chunks)*8; rem > 0 {
			t0 := archsimd.LoadUint64x4(d[len(d)-4:])
			acc0 = acc0.IfElse(acc0.Greater(t0), t0)
			if rem > 4 {
				t1 := archsimd.LoadUint64x4(d[len(d)-8:])
				acc1 = acc1.IfElse(acc1.Greater(t1), t1)
			}
		}
		acc0 = acc0.IfElse(acc0.Greater(acc1), acc1)
		var buf [4]uint64
		acc0.StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

// Float reductions are only recognized in the m = min(m, v) builtin
// form, whose semantics the kernels reproduce exactly: NaN propagates
// (any NaN element forces a NaN result) and -0 is preferred over +0
// for min, +0 over -0 for max.
//
// Per-lane Go min is built from two directed compare+blend selects
// whose bit patterns are ORed (the OR fixes both the NaN and the
// -0/+0 cases), mirroring the scalar lowering in AMD64.rules Min64F.
// archsimd's float Min/Max are not used: their NaN semantics are
// undocumented and the compiler may canonicalize the operands of
// commutative operations, while the trick depends on operand order.
// Go max is the min kernel run in the negated domain
// (max(x, y) = -min(-x, -y)), again mirroring the scalar lowering.

func goMinFloat32x8(a, b archsimd.Float32x8) archsimd.Float32x8 {
	m1 := a.IfElse(a.Less(b), b)
	m2 := b.IfElse(b.Less(a), a)
	return m1.AsInt32x8().Or(m2.AsInt32x8()).AsFloat32x8()
}

func goMinFloat32x16(a, b archsimd.Float32x16) archsimd.Float32x16 {
	m1 := a.IfElse(a.Less(b), b)
	m2 := b.IfElse(b.Less(a), a)
	return m1.AsInt32x16().Or(m2.AsInt32x16()).AsFloat32x16()
}

func goMinFloat64x4(a, b archsimd.Float64x4) archsimd.Float64x4 {
	m1 := a.IfElse(a.Less(b), b)
	m2 := b.IfElse(b.Less(a), a)
	return m1.AsInt64x4().Or(m2.AsInt64x4()).AsFloat64x4()
}

func goMinFloat64x8(a, b archsimd.Float64x8) archsimd.Float64x8 {
	m1 := a.IfElse(a.Less(b), b)
	m2 := b.IfElse(b.Less(a), a)
	return m1.AsInt64x8().Or(m2.AsInt64x8()).AsFloat64x8()
}

func minFloat32(d []float32, m float32) float32 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 32:
		acc0 := archsimd.BroadcastFloat32x16(m)
		acc1 := acc0
		chunks := slicecast[[32]float32](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = goMinFloat32x16(acc0, archsimd.LoadFloat32x16(c[0:16]))
			acc1 = goMinFloat32x16(acc1, archsimd.LoadFloat32x16(c[16:32]))
		}
		if rem := len(d) - len(chunks)*32; rem > 0 {
			acc0 = goMinFloat32x16(acc0, archsimd.LoadFloat32x16(d[len(d)-16:]))
			if rem > 16 {
				acc1 = goMinFloat32x16(acc1, archsimd.LoadFloat32x16(d[len(d)-32:]))
			}
		}
		var buf [16]float32
		goMinFloat32x16(acc0, acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 16:
		acc0 := archsimd.BroadcastFloat32x8(m)
		acc1 := acc0
		chunks := slicecast[[16]float32](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = goMinFloat32x8(acc0, archsimd.LoadFloat32x8(c[0:8]))
			acc1 = goMinFloat32x8(acc1, archsimd.LoadFloat32x8(c[8:16]))
		}
		if rem := len(d) - len(chunks)*16; rem > 0 {
			acc0 = goMinFloat32x8(acc0, archsimd.LoadFloat32x8(d[len(d)-8:]))
			if rem > 8 {
				acc1 = goMinFloat32x8(acc1, archsimd.LoadFloat32x8(d[len(d)-16:]))
			}
		}
		var buf [8]float32
		goMinFloat32x8(acc0, acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxFloat32(d []float32, m float32) float32 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 32:
		sign := archsimd.BroadcastInt32x16(-1 << 31)
		acc0 := archsimd.BroadcastFloat32x16(-m)
		acc1 := acc0
		chunks := slicecast[[32]float32](d)
		for j := range chunks {
			c := &chunks[j]
			v0 := archsimd.LoadFloat32x16(c[0:16]).AsInt32x16().Xor(sign).AsFloat32x16()
			v1 := archsimd.LoadFloat32x16(c[16:32]).AsInt32x16().Xor(sign).AsFloat32x16()
			acc0 = goMinFloat32x16(acc0, v0)
			acc1 = goMinFloat32x16(acc1, v1)
		}
		if rem := len(d) - len(chunks)*32; rem > 0 {
			t0 := archsimd.LoadFloat32x16(d[len(d)-16:]).AsInt32x16().Xor(sign).AsFloat32x16()
			acc0 = goMinFloat32x16(acc0, t0)
			if rem > 16 {
				t1 := archsimd.LoadFloat32x16(d[len(d)-32:]).AsInt32x16().Xor(sign).AsFloat32x16()
				acc1 = goMinFloat32x16(acc1, t1)
			}
		}
		var buf [16]float32
		goMinFloat32x16(acc0, acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		for i := range buf {
			buf[i] = -buf[i]
		}
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 16:
		sign := archsimd.BroadcastInt32x8(-1 << 31)
		acc0 := archsimd.BroadcastFloat32x8(-m)
		acc1 := acc0
		chunks := slicecast[[16]float32](d)
		for j := range chunks {
			c := &chunks[j]
			v0 := archsimd.LoadFloat32x8(c[0:8]).AsInt32x8().Xor(sign).AsFloat32x8()
			v1 := archsimd.LoadFloat32x8(c[8:16]).AsInt32x8().Xor(sign).AsFloat32x8()
			acc0 = goMinFloat32x8(acc0, v0)
			acc1 = goMinFloat32x8(acc1, v1)
		}
		if rem := len(d) - len(chunks)*16; rem > 0 {
			t0 := archsimd.LoadFloat32x8(d[len(d)-8:]).AsInt32x8().Xor(sign).AsFloat32x8()
			acc0 = goMinFloat32x8(acc0, t0)
			if rem > 8 {
				t1 := archsimd.LoadFloat32x8(d[len(d)-16:]).AsInt32x8().Xor(sign).AsFloat32x8()
				acc1 = goMinFloat32x8(acc1, t1)
			}
		}
		var buf [8]float32
		goMinFloat32x8(acc0, acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		for i := range buf {
			buf[i] = -buf[i]
		}
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = max(m, v)
	}
	return m
}

func minFloat64(d []float64, m float64) float64 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 16:
		acc0 := archsimd.BroadcastFloat64x8(m)
		acc1 := acc0
		chunks := slicecast[[16]float64](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = goMinFloat64x8(acc0, archsimd.LoadFloat64x8(c[0:8]))
			acc1 = goMinFloat64x8(acc1, archsimd.LoadFloat64x8(c[8:16]))
		}
		if rem := len(d) - len(chunks)*16; rem > 0 {
			acc0 = goMinFloat64x8(acc0, archsimd.LoadFloat64x8(d[len(d)-8:]))
			if rem > 8 {
				acc1 = goMinFloat64x8(acc1, archsimd.LoadFloat64x8(d[len(d)-16:]))
			}
		}
		var buf [8]float64
		goMinFloat64x8(acc0, acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 8:
		acc0 := archsimd.BroadcastFloat64x4(m)
		acc1 := acc0
		chunks := slicecast[[8]float64](d)
		for j := range chunks {
			c := &chunks[j]
			acc0 = goMinFloat64x4(acc0, archsimd.LoadFloat64x4(c[0:4]))
			acc1 = goMinFloat64x4(acc1, archsimd.LoadFloat64x4(c[4:8]))
		}
		if rem := len(d) - len(chunks)*8; rem > 0 {
			acc0 = goMinFloat64x4(acc0, archsimd.LoadFloat64x4(d[len(d)-4:]))
			if rem > 4 {
				acc1 = goMinFloat64x4(acc1, archsimd.LoadFloat64x4(d[len(d)-8:]))
			}
		}
		var buf [4]float64
		goMinFloat64x4(acc0, acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = min(m, v)
	}
	return m
}

func maxFloat64(d []float64, m float64) float64 {
	switch {
	case archsimd.X86.AVX512() && len(d) >= 16:
		sign := archsimd.BroadcastInt64x8(-1 << 63)
		acc0 := archsimd.BroadcastFloat64x8(-m)
		acc1 := acc0
		chunks := slicecast[[16]float64](d)
		for j := range chunks {
			c := &chunks[j]
			v0 := archsimd.LoadFloat64x8(c[0:8]).AsInt64x8().Xor(sign).AsFloat64x8()
			v1 := archsimd.LoadFloat64x8(c[8:16]).AsInt64x8().Xor(sign).AsFloat64x8()
			acc0 = goMinFloat64x8(acc0, v0)
			acc1 = goMinFloat64x8(acc1, v1)
		}
		if rem := len(d) - len(chunks)*16; rem > 0 {
			t0 := archsimd.LoadFloat64x8(d[len(d)-8:]).AsInt64x8().Xor(sign).AsFloat64x8()
			acc0 = goMinFloat64x8(acc0, t0)
			if rem > 8 {
				t1 := archsimd.LoadFloat64x8(d[len(d)-16:]).AsInt64x8().Xor(sign).AsFloat64x8()
				acc1 = goMinFloat64x8(acc1, t1)
			}
		}
		var buf [8]float64
		goMinFloat64x8(acc0, acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		for i := range buf {
			buf[i] = -buf[i]
		}
		m, d = buf[0], buf[1:]
	case archsimd.X86.AVX2() && len(d) >= 8:
		sign := archsimd.BroadcastInt64x4(-1 << 63)
		acc0 := archsimd.BroadcastFloat64x4(-m)
		acc1 := acc0
		chunks := slicecast[[8]float64](d)
		for j := range chunks {
			c := &chunks[j]
			v0 := archsimd.LoadFloat64x4(c[0:4]).AsInt64x4().Xor(sign).AsFloat64x4()
			v1 := archsimd.LoadFloat64x4(c[4:8]).AsInt64x4().Xor(sign).AsFloat64x4()
			acc0 = goMinFloat64x4(acc0, v0)
			acc1 = goMinFloat64x4(acc1, v1)
		}
		if rem := len(d) - len(chunks)*8; rem > 0 {
			t0 := archsimd.LoadFloat64x4(d[len(d)-4:]).AsInt64x4().Xor(sign).AsFloat64x4()
			acc0 = goMinFloat64x4(acc0, t0)
			if rem > 4 {
				t1 := archsimd.LoadFloat64x4(d[len(d)-8:]).AsInt64x4().Xor(sign).AsFloat64x4()
				acc1 = goMinFloat64x4(acc1, t1)
			}
		}
		var buf [4]float64
		goMinFloat64x4(acc0, acc1).StoreArray(&buf)
		archsimd.ClearAVXUpperBits()
		for i := range buf {
			buf[i] = -buf[i]
		}
		m, d = buf[0], buf[1:]
	}
	for _, v := range d {
		m = max(m, v)
	}
	return m
}
