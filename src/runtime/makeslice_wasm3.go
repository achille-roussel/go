// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import (
	"internal/runtime/math"
	"unsafe"
)

// makeslice / makeslice64 for GOARCH=wasm3 bypass mallocgc and route
// through newobject (the runtime fork's bump allocator). The standard
// makeslice in slice.go ends in mallocgc which pulls in the wasm3-
// incompatible page-allocator + trace-locker chain (postMallocgcDebug
// takes a traceLocker struct return whose collector lowering uses a
// ref-typed wasm field while the SSA call site pushes i64 — see
// doc/wasm3-m3-notes.md "Stage E ABI surfaces").
//
// Stage E proper will lower the slice header to wasmgc
// (ref backing, i32 off, i32 len, i32 cap) and intrinsify make() to
// array.new_default at SSA time, retiring this shim. Until then, the
// slice header stays the standard (ptr, len, cap) shape with a linear-
// memory pointer from the bump heap.
//
//go:linkname makeslice
func makeslice(et *_type, len, cap int) unsafe.Pointer {
	mem, overflow := math.MulUintptr(et.Size_, uintptr(cap))
	if overflow || mem > maxAlloc || len < 0 || len > cap {
		mem, overflow := math.MulUintptr(et.Size_, uintptr(len))
		if overflow || mem > maxAlloc || len < 0 {
			panicmakeslicelen()
		}
		panicmakeslicecap()
	}
	return wasm3HeapAlloc(mem)
}

func makeslice64(et *_type, len64, cap64 int64) unsafe.Pointer {
	len := int(len64)
	if int64(len) != len64 {
		panicmakeslicelen()
	}
	cap := int(cap64)
	if int64(cap) != cap64 {
		panicmakeslicecap()
	}
	return makeslice(et, len, cap)
}

// wasm3HeapAlloc carves nbytes from the wasm3 bump heap (see
// newobject_wasm3.go) and zeros the returned region. Shared with
// newobject — keeping a single allocator means one debug trap and
// one watermark to grow.
//
// Exposed via linkname for the wasm3 maps shim
// (internal/runtime/maps/runtime_faststr_wasm3.go); the maps package
// can't import "runtime" directly.
//
//go:linkname wasm3HeapAlloc
//go:nosplit
func wasm3HeapAlloc(nbytes uintptr) unsafe.Pointer {
	aligned := (nbytes + 7) &^ 7
	if wasm3HeapNext+aligned > uintptr(len(wasm3Heap)) {
		zero := int32(wasm3HeapNext - wasm3HeapNext)
		_ = int32(1) / zero
	}
	off := wasm3HeapNext
	wasm3HeapNext = off + aligned
	for i := uintptr(0); i < aligned; i++ {
		wasm3Heap[off+i] = 0
	}
	return unsafe.Pointer(&wasm3Heap[off])
}
