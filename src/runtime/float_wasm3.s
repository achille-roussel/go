// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

#include "textflag.h"

// func float64bits(f float64) uint64
TEXT ·float64bits(SB),NOSPLIT,$0-16
	Get SP
	F64Load f+0(FP)
	I64ReinterpretF64
	I64Store ret+8(FP)
	RET

// func float64frombits(b uint64) float64
TEXT ·float64frombits(SB),NOSPLIT,$0-16
	Get SP
	I64Load b+0(FP)
	F64ReinterpretI64
	F64Store ret+8(FP)
	RET
