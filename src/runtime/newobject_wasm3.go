// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// wasm3Heap is the linear-memory backing for the M2 cutover's bump
// allocator. 1 MiB is large enough for the simple programs M2 targets;
// once exhausted, allocation traps via the int-divide-by-zero pattern
// below. The eventual M2 design uses WasmGC `struct.new` directly so
// heap objects live on the host GC heap; this allocator is a
// transitional stand-in.
var wasm3Heap [1 << 20]byte

// wasm3HeapNext is the byte offset within wasm3Heap of the next
// allocation. Starts at 0, monotonically increases. Single-goroutine —
// no concurrent allocations to worry about.
var wasm3HeapNext uintptr

// newobject implements `new(T)` for GOARCH=wasm3.
//
// The standard implementation calls mallocgc, which depends on the GC
// page-cache subsystem that wasm3 deletes outright. Until the design's
// WasmGC `struct.new` intrinsic lands (after which the SSA backend
// replaces the runtime.newobject call with a direct struct.new opcode),
// this bump allocator gives `new()` and any escape-analysis-promoted
// allocation a body that doesn't trap immediately.
//
//go:nosplit
func newobject(typ *_type) unsafe.Pointer {
	return wasm3BumpAlloc(typ.Size_)
}

// wasm3BumpAlloc carves `size` bytes (aligned up to 8) out of the
// wasm3Heap bump arena. The wasm3 fork's mallocgc shim and any
// other size-keyed allocation site routes through here; newobject
// is the type-keyed wrapper above.
//
// Single-goroutine wasm3 — no atomic / locking discipline needed.
//
//go:nosplit
func wasm3BumpAlloc(size uintptr) unsafe.Pointer {
	if size == 0 {
		return unsafe.Pointer(&zerobase)
	}
	aligned := (size + 7) &^ 7
	if wasm3HeapNext+aligned > uintptr(len(wasm3Heap)) {
		// Heap exhausted. Trap with i32.div_s by 0 — cheap, no
		// indirect call needed.
		zero := int32(wasm3HeapNext - wasm3HeapNext) // always 0
		_ = int32(1) / zero
	}
	off := wasm3HeapNext
	wasm3HeapNext = off + aligned
	// Zero the freshly allocated region. wasm3Heap is package-global so
	// indexing into it does not escape through SP.
	for i := uintptr(0); i < aligned; i++ {
		wasm3Heap[off+i] = 0
	}
	return unsafe.Pointer(&wasm3Heap[off])
}
