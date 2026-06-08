// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 mirror of minmax.go. The only divergence from the !wasm3
// original is forbits / fandbits: the shared file does the bit-
// op via `*(*uint32)(unsafe.Pointer(&x)) |= *(*uint32)(unsafe
// .Pointer(&y))` (and the uint64 equivalent), which the wasm3
// SSA layer classifies as a polymorphic-pointer deref and emits
// anyref-typed locals for. The mirror routes the float32 arm
// through runtime.float32bits / float32frombits and the float64
// arm through runtime.float64bits / float64frombits, both of
// which are intrinsified to single-instruction wasm reinterprets
// (cmd/compile/internal/ssagen/intrinsics.go). No call, no frame,
// no linear-memory bridge — just an i64.or / i32.or sandwiched
// between two reinterprets.

package runtime

import "unsafe"

func strmin(x, y string) string {
	if y < x {
		return y
	}
	return x
}

func strmax(x, y string) string {
	if y > x {
		return y
	}
	return x
}

func fmin32(x, y float32) float32 { return fmin(x, y) }
func fmin64(x, y float64) float64 { return fmin(x, y) }
func fmax32(x, y float32) float32 { return fmax(x, y) }
func fmax64(x, y float64) float64 { return fmax(x, y) }

type floaty interface{ ~float32 | ~float64 }

func fmin[F floaty](x, y F) F {
	if y != y || y < x {
		return y
	}
	if x != x || x < y || x != 0 {
		return x
	}
	// x and y are both ±0
	// if either is -0, return -0; else return +0
	return forbits(x, y)
}

func fmax[F floaty](x, y F) F {
	if y != y || y > x {
		return y
	}
	if x != x || x > y || x != 0 {
		return x
	}
	// x and y are both ±0
	// if both are -0, return -0; else return +0
	return fandbits(x, y)
}

func forbits[F floaty](x, y F) F {
	switch unsafe.Sizeof(x) {
	case 4:
		// F instantiated at float32; cast through the float32
		// helpers, which intrinsify to i32.reinterpret_f32.
		return F(float32frombits(float32bits(float32(x)) | float32bits(float32(y))))
	case 8:
		return F(float64frombits(float64bits(float64(x)) | float64bits(float64(y))))
	}
	return x
}

func fandbits[F floaty](x, y F) F {
	switch unsafe.Sizeof(x) {
	case 4:
		return F(float32frombits(float32bits(float32(x)) & float32bits(float32(y))))
	case 8:
		return F(float64frombits(float64bits(float64(x)) & float64bits(float64(y))))
	}
	return x
}
