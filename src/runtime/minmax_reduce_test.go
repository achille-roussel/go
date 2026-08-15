// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime_test

import (
	"math"
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

var minmaxSizes = []int{0, 1, 2, 15, 16, 31, 32, 33, 63, 64, 65, 96, 127, 128, 129, 191, 192, 255, 256, 1000, 4096}

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

func refMinInt32(d []int32, m int32) int32 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] < m {
			m = d[i]
		}
	}
	return m
}

func refMaxInt32(d []int32, m int32) int32 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] > m {
			m = d[i]
		}
	}
	return m
}

func refMinUint32(d []uint32, m uint32) uint32 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] < m {
			m = d[i]
		}
	}
	return m
}

func refMaxUint32(d []uint32, m uint32) uint32 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] > m {
			m = d[i]
		}
	}
	return m
}

func refMinInt64(d []int64, m int64) int64 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] < m {
			m = d[i]
		}
	}
	return m
}

func refMaxInt64(d []int64, m int64) int64 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] > m {
			m = d[i]
		}
	}
	return m
}

func refMinUint64(d []uint64, m uint64) uint64 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] < m {
			m = d[i]
		}
	}
	return m
}

func refMaxUint64(d []uint64, m uint64) uint64 {
	for i := len(d) - 1; i >= 0; i-- {
		if d[i] > m {
			m = d[i]
		}
	}
	return m
}

func TestMinMaxInt32Kernel(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for _, n := range minmaxSizes {
		d := make([]int32, n)
		for i := range d {
			d[i] = int32(r.Uint64())
		}
		if n > 0 {
			d[n-1] = math.MinInt32
			d[0] = math.MaxInt32
		}
		for _, seed := range []int32{math.MinInt32, -1, 0, 1, math.MaxInt32} {
			if got, want := runtime.MinInt32Kernel(d, seed), refMinInt32(d, seed); got != want {
				t.Errorf("minInt32(%d, len %d) = %d, want %d", seed, n, got, want)
			}
			if got, want := runtime.MaxInt32Kernel(d, seed), refMaxInt32(d, seed); got != want {
				t.Errorf("maxInt32(%d, len %d) = %d, want %d", seed, n, got, want)
			}
		}
	}
}

func TestMinMaxUint32Kernel(t *testing.T) {
	r := rand.New(rand.NewSource(8))
	for _, n := range minmaxSizes {
		d := make([]uint32, n)
		for i := range d {
			d[i] = uint32(r.Uint64())
		}
		if n > 0 {
			d[n-1] = 0
			d[0] = math.MaxUint32
		}
		for _, seed := range []uint32{0, 1, 1 << 31, math.MaxUint32 - 1, math.MaxUint32} {
			if got, want := runtime.MinUint32Kernel(d, seed), refMinUint32(d, seed); got != want {
				t.Errorf("minUint32(%d, len %d) = %d, want %d", seed, n, got, want)
			}
			if got, want := runtime.MaxUint32Kernel(d, seed), refMaxUint32(d, seed); got != want {
				t.Errorf("maxUint32(%d, len %d) = %d, want %d", seed, n, got, want)
			}
		}
	}
}

func TestMinMaxInt64Kernel(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	for _, n := range minmaxSizes {
		d := make([]int64, n)
		for i := range d {
			d[i] = int64(r.Uint64())
		}
		if n > 0 {
			d[n-1] = math.MinInt64
			d[0] = math.MaxInt64
		}
		for _, seed := range []int64{math.MinInt64, -1, 0, 1, math.MaxInt64} {
			if got, want := runtime.MinInt64Kernel(d, seed), refMinInt64(d, seed); got != want {
				t.Errorf("minInt64(%d, len %d) = %d, want %d", seed, n, got, want)
			}
			if got, want := runtime.MaxInt64Kernel(d, seed), refMaxInt64(d, seed); got != want {
				t.Errorf("maxInt64(%d, len %d) = %d, want %d", seed, n, got, want)
			}
		}
	}
}

// TestMinMaxUint64Kernel is particularly interested in values on both
// sides of 1<<63: the AVX2 tier has no unsigned 64-bit compare and
// relies on archsimd's sign-bias emulation.
func TestMinMaxUint64Kernel(t *testing.T) {
	r := rand.New(rand.NewSource(10))
	for _, n := range minmaxSizes {
		d := make([]uint64, n)
		for i := range d {
			d[i] = r.Uint64()
		}
		if n > 0 {
			d[n-1] = 0
			d[0] = math.MaxUint64
		}
		if n > 2 {
			d[n/2] = 1<<63 - 1
			d[n/2+1] = 1 << 63
		}
		for _, seed := range []uint64{0, 1, 1<<63 - 1, 1 << 63, math.MaxUint64} {
			if got, want := runtime.MinUint64Kernel(d, seed), refMinUint64(d, seed); got != want {
				t.Errorf("minUint64(%d, len %d) = %d, want %d", seed, n, got, want)
			}
			if got, want := runtime.MaxUint64Kernel(d, seed), refMaxUint64(d, seed); got != want {
				t.Errorf("maxUint64(%d, len %d) = %d, want %d", seed, n, got, want)
			}
		}
	}
}

// Float references use the min/max builtins; their semantics are
// order-independent, so a backward loop computes the same value
// bit-for-bit.

func refMinFloat64(d []float64, m float64) float64 {
	for i := len(d) - 1; i >= 0; i-- {
		m = min(m, d[i])
	}
	return m
}

func refMaxFloat64(d []float64, m float64) float64 {
	for i := len(d) - 1; i >= 0; i-- {
		m = max(m, d[i])
	}
	return m
}

func refMinFloat32(d []float32, m float32) float32 {
	for i := len(d) - 1; i >= 0; i-- {
		m = min(m, d[i])
	}
	return m
}

func refMaxFloat32(d []float32, m float32) float32 {
	for i := len(d) - 1; i >= 0; i-- {
		m = max(m, d[i])
	}
	return m
}

// eqFloat64 compares float results treating all NaNs as equal and
// distinguishing -0 from +0.
func eqFloat64(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	return math.Float64bits(a) == math.Float64bits(b)
}

func eqFloat32(a, b float32) bool {
	if a != a || b != b {
		return a != a && b != b
	}
	return math.Float32bits(a) == math.Float32bits(b)
}

func TestMinMaxFloat64Kernel(t *testing.T) {
	r := rand.New(rand.NewSource(12))
	for _, n := range minmaxSizes {
		d := make([]float64, n)
		for i := range d {
			d[i] = r.NormFloat64()
		}
		if n > 0 {
			d[n-1] = math.Inf(-1)
			d[0] = math.Inf(1)
		}
		for _, seed := range []float64{math.Inf(-1), -1, 0, 1, math.Inf(1), math.NaN()} {
			if got, want := runtime.MinFloat64Kernel(d, seed), refMinFloat64(d, seed); !eqFloat64(got, want) {
				t.Errorf("minFloat64(%v, len %d) = %v, want %v", seed, n, got, want)
			}
			if got, want := runtime.MaxFloat64Kernel(d, seed), refMaxFloat64(d, seed); !eqFloat64(got, want) {
				t.Errorf("maxFloat64(%v, len %d) = %v, want %v", seed, n, got, want)
			}
		}
	}
}

func TestMinMaxFloat32Kernel(t *testing.T) {
	r := rand.New(rand.NewSource(13))
	for _, n := range minmaxSizes {
		d := make([]float32, n)
		for i := range d {
			d[i] = float32(r.NormFloat64())
		}
		if n > 0 {
			d[n-1] = float32(math.Inf(-1))
			d[0] = float32(math.Inf(1))
		}
		for _, seed := range []float32{float32(math.Inf(-1)), -1, 0, 1, float32(math.Inf(1)), float32(math.NaN())} {
			if got, want := runtime.MinFloat32Kernel(d, seed), refMinFloat32(d, seed); !eqFloat32(got, want) {
				t.Errorf("minFloat32(%v, len %d) = %v, want %v", seed, n, got, want)
			}
			if got, want := runtime.MaxFloat32Kernel(d, seed), refMaxFloat32(d, seed); !eqFloat32(got, want) {
				t.Errorf("maxFloat32(%v, len %d) = %v, want %v", seed, n, got, want)
			}
		}
	}
}

// TestMinMaxFloatKernelNaNPositions plants a NaN at every position and
// checks that the kernels return NaN through the vector paths'
// overlapping tail loads.
func TestMinMaxFloatKernelNaNPositions(t *testing.T) {
	nan := math.NaN()
	for _, n := range []int{8, 9, 15, 16, 17, 31, 32, 33, 63, 64} {
		d := make([]float64, n)
		for i := range d {
			d[i] = float64(i)
		}
		for pos := range d {
			old := d[pos]
			d[pos] = nan
			if got := runtime.MinFloat64Kernel(d, 1000); !math.IsNaN(got) {
				t.Fatalf("minFloat64: NaN at %d of %d not propagated: got %v", pos, n, got)
			}
			if got := runtime.MaxFloat64Kernel(d, -1000); !math.IsNaN(got) {
				t.Fatalf("maxFloat64: NaN at %d of %d not propagated: got %v", pos, n, got)
			}
			d[pos] = old
		}
	}
}

// TestMinMaxFloatKernelSignedZero checks the builtin-form signed-zero
// preferences, which are order-independent: min prefers -0, max
// prefers +0.
func TestMinMaxFloatKernelSignedZero(t *testing.T) {
	negZero := math.Copysign(0, -1)
	for _, n := range []int{8, 16, 31, 32, 64} {
		for pos := 0; pos < n; pos++ {
			d := make([]float64, n) // all +0
			d[pos] = negZero
			if got := runtime.MinFloat64Kernel(d, 0); !math.Signbit(got) {
				t.Fatalf("minFloat64: -0 at %d of %d: got +0, want -0", pos, n)
			}
			// All zeros with one -0: max must return +0.
			if got := runtime.MaxFloat64Kernel(d, negZero); math.Signbit(got) {
				t.Fatalf("maxFloat64: +0 elements with -0 seed: got -0, want +0 (n=%d)", n)
			}
		}
	}
}

// TestMinMaxKernelExtremePositions sweeps the extreme element through
// every position so overlapping tail loads and accumulator merging are
// all exercised.
func TestMinMaxKernelExtremePositions(t *testing.T) {
	for _, n := range []int{64, 65, 96, 127, 128, 129, 191, 192, 255, 256} {
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

func BenchmarkMinInt64Kernel(b *testing.B) {
	for _, n := range []int{64, 1024, 65536} {
		d := make([]int64, n)
		r := rand.New(rand.NewSource(11))
		for i := range d {
			d[i] = int64(r.Uint64())
		}
		b.Run(sizeName(n), func(b *testing.B) {
			b.SetBytes(int64(8 * n))
			for b.Loop() {
				runtime.MinInt64Kernel(d, math.MaxInt64)
			}
		})
	}
}

func BenchmarkMinFloat32Kernel(b *testing.B) {
	for _, n := range []int{64, 1024, 65536} {
		d := make([]float32, n)
		r := rand.New(rand.NewSource(14))
		for i := range d {
			d[i] = float32(r.NormFloat64())
		}
		b.Run(sizeName(n), func(b *testing.B) {
			b.SetBytes(int64(4 * n))
			for b.Loop() {
				runtime.MinFloat32Kernel(d, float32(math.Inf(1)))
			}
		})
	}
}

func BenchmarkMinFloat64Kernel(b *testing.B) {
	for _, n := range []int{64, 1024, 65536} {
		d := make([]float64, n)
		r := rand.New(rand.NewSource(15))
		for i := range d {
			d[i] = r.NormFloat64()
		}
		b.Run(sizeName(n), func(b *testing.B) {
			b.SetBytes(int64(8 * n))
			for b.Loop() {
				runtime.MinFloat64Kernel(d, math.Inf(1))
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
