// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package abi

const (
	// See abi_generic.go.

	// R0 - R15. Matches paramIntRegWasm3 in cmd/compile/internal/ssa.
	IntArgRegs = 16

	// F0 - F31. Matches paramFloatRegWasm3 in cmd/compile/internal/ssa.
	FloatArgRegs = 32

	EffectiveFloatRegSize = 8
)
