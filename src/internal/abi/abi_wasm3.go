// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 mirror of abi.go. RegArgs and IntArgRegBitmap have identical
// shape; only the two helpers that did pointer arithmetic are
// rewritten:
//
//   - IntRegArgAddr: the non-wasm3 form adds `goarch.PtrSize -
//     argSize` to the register address on big-endian archs. wasm3
//     is little-endian, so the offset is always zero — just return
//     &r.Ints[reg].
//   - IntArgRegBitmap.Get: the non-wasm3 form skips the slice bounds
//     check via pointer arithmetic because the reflectcall return
//     path is nosplit. wasm3 carries the bounds check at the wasm
//     engine level either way (WasmGC array.get), so the plain
//     b[i/8] form is equivalent.

package abi

import "unsafe"

// wasm3 has regabiargs always-on (see internal/buildcfg/exp.go) but
// no Go-managed register ABI — its native typed functions pass args
// and results as wasm params/results. abi_generic.go is excluded
// because regabiargs is on, so define the zero-reg constants here.
const (
	IntArgRegs            = 0
	FloatArgRegs          = 0
	EffectiveFloatRegSize = 0
)

type RegArgs struct {
	Ints        [IntArgRegs]uintptr
	Floats      [FloatArgRegs]uint64
	Ptrs        [IntArgRegs]unsafe.Pointer
	ReturnIsPtr IntArgRegBitmap
}

func (r *RegArgs) Dump() {
	print("Ints:")
	for _, x := range r.Ints {
		print(" ", x)
	}
	println()
	print("Floats:")
	for _, x := range r.Floats {
		print(" ", x)
	}
	println()
	print("Ptrs:")
	for _, x := range r.Ptrs {
		print(" ", x)
	}
	println()
}

func (r *RegArgs) IntRegArgAddr(reg int, argSize uintptr) unsafe.Pointer {
	if argSize > 8 || argSize == 0 || argSize&(argSize-1) != 0 {
		panic("invalid argSize")
	}
	return unsafe.Pointer(&r.Ints[reg])
}

// wasm3 has no register ABI today (IntArgRegs and FloatArgRegs both
// come from abi_generic.go where they're 0), so the bitmap is just a
// placeholder. Use a 1-byte representation regardless of IntArgRegs
// so the Set/Get methods are syntactically valid; both methods panic
// because no caller should be touching this bitmap on a zero-reg
// ABI.
type IntArgRegBitmap [1]uint8

func (b *IntArgRegBitmap) Set(i int) {
	panic("internal/abi: IntArgRegBitmap.Set unreachable on wasm3 (no register ABI)")
}

//go:nosplit
func (b *IntArgRegBitmap) Get(i int) bool {
	return false
}
