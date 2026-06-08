// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 archFloor/Ceil/Trunc become compiler intrinsics
// (cmd/compile/internal/ssagen/intrinsics.go), each lowering to
// a single wasm `f64.floor` / `f64.ceil` / `f64.trunc`
// instruction emitted inline into the caller's body. The Go
// bodies kept here are defensive fallbacks for any caller that
// somehow reaches us without the intrinsic firing — they call
// the equivalent wasm op via a math-package primitive on every
// platform, so the fallback is correct if slow. In practice the
// intrinsic always fires and the bodies never execute.
//
// Previous wasm3 archFloor lived in math/floor_wasm3.s using
// `Get SP; F64Load x+0(FP); F64Floor; F64Store ret+8(FP)` — the
// standard wasm asm calling convention threads every call
// through a linear-memory frame. wasm3 design exiles linear
// memory to the host boundary; the asm path re-introduced it.
// Replaced with the intrinsic pattern that wasm3MakeSlice /
// float64bits use.

package math

const haveArchFloor = true
const haveArchCeil = true
const haveArchTrunc = true

func archFloor(x float64) float64 { return floor(x) }
func archCeil(x float64) float64  { return ceil(x) }
func archTrunc(x float64) float64 { return trunc(x) }
