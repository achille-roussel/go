// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 mirror of unsafe.go.
//
// Near-verbatim. The only adaptation: the `mem > -uintptr(ptr)`
// overflow check in unsafeslice / unsafeslice64 / unsafestring /
// unsafestring64 (line 66 etc.) tests whether the requested
// allocation would wrap past the maximum addressable space. On
// wasm3 there's no scalar address space to wrap past — anyref
// has no scalar form, and the uintptr(ptr) cast is rejected by
// the SSA-gen fatal. Drop that arm of the conjunction; the
// `overflow` half (from math.MulUintptr) still catches the
// size*len overflow case which is the actually-load-bearing
// check.

package runtime

import (
	"internal/runtime/math"
	"internal/runtime/sys"
	"unsafe"
)

func unsafestring(ptr unsafe.Pointer, len int) {
	if len < 0 {
		panicunsafestringlen()
	}

	// wasm3: the `uintptr(len) > -uintptr(ptr)` wrap check drops
	// here — anyref has no scalar address space to wrap past, and
	// `uintptr(ptr)` is rejected by the SSA-gen fatal. Keep the
	// nil-ptr-with-positive-len check separately.
	if ptr == nil && len > 0 {
		panicunsafestringnilptr()
	}
}

// Keep this code in sync with cmd/compile/internal/walk/builtin.go:walkUnsafeString
func unsafestring64(ptr unsafe.Pointer, len64 int64) {
	len := int(len64)
	if int64(len) != len64 {
		panicunsafestringlen()
	}
	unsafestring(ptr, len)
}

func unsafestringcheckptr(ptr unsafe.Pointer, len64 int64) {
	unsafestring64(ptr, len64)

	// Check that underlying array doesn't straddle multiple heap objects.
	// unsafestring64 has already checked for overflow.
	if checkptrStraddles(ptr, uintptr(len64)) {
		throw("checkptr: unsafe.String result straddles multiple allocations")
	}
}

func panicunsafestringlen() {
	panic(errorString("unsafe.String: len out of range"))
}

func panicunsafestringnilptr() {
	panic(errorString("unsafe.String: ptr is nil and len is not zero"))
}

// Keep this code in sync with cmd/compile/internal/walk/builtin.go:walkUnsafeSlice
func unsafeslice(et *_type, ptr unsafe.Pointer, len int) {
	if len < 0 {
		panicunsafeslicelen1(sys.GetCallerPC())
	}

	if et.Size_ == 0 {
		if ptr == nil && len > 0 {
			panicunsafeslicenilptr1(sys.GetCallerPC())
		}
	}

	// wasm3: drop the `mem > -uintptr(ptr)` wrap arm — anyref has
	// no scalar address space to wrap past. The size*len overflow
	// half stays, since that's the load-bearing check.
	_, overflow := math.MulUintptr(et.Size_, uintptr(len))
	if overflow {
		if ptr == nil {
			panicunsafeslicenilptr1(sys.GetCallerPC())
		}
		panicunsafeslicelen1(sys.GetCallerPC())
	}
}

// Keep this code in sync with cmd/compile/internal/walk/builtin.go:walkUnsafeSlice
func unsafeslice64(et *_type, ptr unsafe.Pointer, len64 int64) {
	len := int(len64)
	if int64(len) != len64 {
		panicunsafeslicelen1(sys.GetCallerPC())
	}
	unsafeslice(et, ptr, len)
}

func unsafeslicecheckptr(et *_type, ptr unsafe.Pointer, len64 int64) {
	unsafeslice64(et, ptr, len64)

	// Check that underlying array doesn't straddle multiple heap objects.
	// unsafeslice64 has already checked for overflow.
	if checkptrStraddles(ptr, uintptr(len64)*et.Size_) {
		throw("checkptr: unsafe.Slice result straddles multiple allocations")
	}
}

func panicunsafeslicelen() {
	// This is called only from compiler-generated code, so we can get the
	// source of the panic.
	panicunsafeslicelen1(sys.GetCallerPC())
}

//go:yeswritebarrierrec
func panicunsafeslicelen1(pc uintptr) {
	panicCheck1(pc, "unsafe.Slice: len out of range")
	panic(errorString("unsafe.Slice: len out of range"))
}

func panicunsafeslicenilptr() {
	// This is called only from compiler-generated code, so we can get the
	// source of the panic.
	panicunsafeslicenilptr1(sys.GetCallerPC())
}

//go:yeswritebarrierrec
func panicunsafeslicenilptr1(pc uintptr) {
	panicCheck1(pc, "unsafe.Slice: ptr is nil and len is not zero")
	panic(errorString("unsafe.Slice: ptr is nil and len is not zero"))
}

//go:linkname reflect_unsafeslice reflect.unsafeslice
func reflect_unsafeslice(et *_type, ptr unsafe.Pointer, len int) {
	unsafeslice(et, ptr, len)
}
