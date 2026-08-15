// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"math/rand"
	"runtime"
	"testing"
)

// The reference reductions below intentionally iterate backwards: the
// compiler's reduction-loop recognition only matches forward loops with
// an i < len(d) condition (or forward range loops), so it cannot
// rewrite these references into calls to the kernels under test.

func refMinUint8(d []uint8, m uint8) uint8 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] < m {
			m = d[i]
		}
	}
	return m
}

func refMaxUint8(d []uint8, m uint8) uint8 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] > m {
			m = d[i]
		}
	}
	return m
}

func refMinInt8(d []int8, m int8) int8 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] < m {
			m = d[i]
		}
	}
	return m
}

func refMaxInt8(d []int8, m int8) int8 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] > m {
			m = d[i]
		}
	}
	return m
}

func refMinInt16(d []int16, m int16) int16 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] < m {
			m = d[i]
		}
	}
	return m
}

func refMaxInt16(d []int16, m int16) int16 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] > m {
			m = d[i]
		}
	}
	return m
}

func refMinUint16(d []uint16, m uint16) uint16 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] < m {
			m = d[i]
		}
	}
	return m
}

func refMaxUint16(d []uint16, m uint16) uint16 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] > m {
			m = d[i]
		}
	}
	return m
}

var minmaxSizes = []int{0, 1, 2, 15, 16, 31, 32, 33, 63, 64, 65, 96, 127, 128, 129, 255, 256, 1000, 4096}

func TestMinMaxUint8Kernel(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, n := range minmaxSizes {
		d := make([]uint8, n)
		for i := range d {
			d[i] = uint8(r.Uint32())
		}
		// Place extremes at positions the tail handling must not miss.
		if n > 0 {
			d[n-1] = 0
			d[0] = 255
		}
		for _, seed := range []uint8{0, 1, 128, 254, 255} {
			if got, want := runtime.MinUint8Kernel(d, seed), refMinUint8(d, seed); got != want {
				t.Errorf("minUint8(%d, len %d) = %d, want %d", seed, n, got, want)
			}
			if got, want := runtime.MaxUint8Kernel(d, seed), refMaxUint8(d, seed); got != want {
				t.Errorf("maxUint8(%d, len %d) = %d, want %d", seed, n, got, want)
			}
		}
	}
}

func TestMinMaxInt16Kernel(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for _, n := range minmaxSizes {
		d := make([]int16, n)
		for i := range d {
			d[i] = int16(r.Uint32())
		}
		if n > 0 {
			d[n-1] = -32768
			d[0] = 32767
		}
		for _, seed := range []int16{-32768, -1, 0, 1, 32767} {
			if got, want := runtime.MinInt16Kernel(d, seed), refMinInt16(d, seed); got != want {
				t.Errorf("minInt16(%d, len %d) = %d, want %d", seed, n, got, want)
			}
			if got, want := runtime.MaxInt16Kernel(d, seed), refMaxInt16(d, seed); got != want {
				t.Errorf("maxInt16(%d, len %d) = %d, want %d", seed, n, got, want)
			}
		}
	}
}

func TestMinMaxInt8Kernel(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	for _, n := range minmaxSizes {
		d := make([]int8, n)
		for i := range d {
			d[i] = int8(r.Uint32())
		}
		if n > 0 {
			d[n-1] = -128
			d[0] = 127
		}
		for _, seed := range []int8{-128, -1, 0, 1, 127} {
			if got, want := runtime.MinInt8Kernel(d, seed), refMinInt8(d, seed); got != want {
				t.Errorf("minInt8(%d, len %d) = %d, want %d", seed, n, got, want)
			}
			if got, want := runtime.MaxInt8Kernel(d, seed), refMaxInt8(d, seed); got != want {
				t.Errorf("maxInt8(%d, len %d) = %d, want %d", seed, n, got, want)
			}
		}
	}
}

func TestMinMaxUint16Kernel(t *testing.T) {
	r := rand.New(rand.NewSource(6))
	for _, n := range minmaxSizes {
		d := make([]uint16, n)
		for i := range d {
			d[i] = uint16(r.Uint32())
		}
		if n > 0 {
			d[n-1] = 0
			d[0] = 65535
		}
		for _, seed := range []uint16{0, 1, 32768, 65534, 65535} {
			if got, want := runtime.MinUint16Kernel(d, seed), refMinUint16(d, seed); got != want {
				t.Errorf("minUint16(%d, len %d) = %d, want %d", seed, n, got, want)
			}
			if got, want := runtime.MaxUint16Kernel(d, seed), refMaxUint16(d, seed); got != want {
				t.Errorf("maxUint16(%d, len %d) = %d, want %d", seed, n, got, want)
			}
		}
	}
}

// TestMinMaxKernelExtremePositions sweeps the extreme element through
// every position so overlapping tail loads and accumulator merging are
// all exercised.
func TestMinMaxKernelExtremePositions(t *testing.T) {
	for _, n := range []int{64, 65, 96, 127, 128} {
		d := make([]uint8, n)
		for i := range d {
			d[i] = 100
		}
		for pos := range d {
			d[pos] = 7
			if got := runtime.MinUint8Kernel(d, 200); got != 7 {
				t.Fatalf("minUint8: extreme at %d of %d: got %d, want 7", pos, n, got)
			}
			d[pos] = 201
			if got := runtime.MaxUint8Kernel(d, 200); got != 201 {
				t.Fatalf("maxUint8: extreme at %d of %d: got %d, want 201", pos, n, got)
			}
			d[pos] = 100
		}
	}
}

func BenchmarkMinUint8Kernel(b *testing.B) {
	for _, n := range []int{64, 1024, 65536} {
		d := make([]uint8, n)
		r := rand.New(rand.NewSource(3))
		for i := range d {
			d[i] = uint8(r.Uint32())
		}
		b.Run(sizeName(n), func(b *testing.B) {
			b.SetBytes(int64(n))
			for b.Loop() {
				runtime.MinUint8Kernel(d, 255)
			}
		})
	}
}

func BenchmarkMinInt16Kernel(b *testing.B) {
	for _, n := range []int{64, 1024, 65536} {
		d := make([]int16, n)
		r := rand.New(rand.NewSource(4))
		for i := range d {
			d[i] = int16(r.Uint32())
		}
		b.Run(sizeName(n), func(b *testing.B) {
			b.SetBytes(int64(2 * n))
			for b.Loop() {
				runtime.MinInt16Kernel(d, 32767)
			}
		})
	}
}

func sizeName(n int) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return itoa(n>>20) + "M"
	case n >= 1<<10 && n%(1<<10) == 0:
		return itoa(n>>10) + "K"
	}
	return itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
