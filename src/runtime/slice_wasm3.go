// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// wasm3 mirror of slice.go.
//
// slice.go is tagged !wasm3 because growslice / makeslice /
// makeslicecopy / growsliceNoAlias all do pointer arithmetic
// (`add(p, off)`, `memclrNoHeapPointers` with a sub-buffer
// pointer) that anyref cannot represent. On wasm3 the compiler
// intrinsifies make() / append() / copy() to direct
// `array.new_default` / `array.copy` ops on the WasmGC backing,
// so the runtime growslice path is unreachable.
//
// What this file provides is the small surface other files in
// the wasm3 build still reference from slice.go:
//
//   - `type slice` — the runtime slice header layout. metrics.go,
//     string.go, arena.go (if reachable on wasm3) all construct
//     `slice{ptr, len, cap}` literals to reinterpret an allocation
//     as a slice. The struct stays compile-compatible.
//   - `slicecopy` — mbarrier.go's typedslicecopy fallback calls
//     it for pointer-element slice copies. The body is verbatim
//     from slice.go (no ptr-arith — just a memmove forwarder)
//     and works unchanged on wasm3.

type slice struct {
	array unsafe.Pointer
	len   int
	cap   int
}

// growslice is unreachable on wasm3: appendSlice in
// cmd/compile/internal/ssagen emits OpWasm3MakeSlice +
// OpWasm3ArrayCopy inline instead of calling this function
// (commit fb46a62080). The symbol must still link though —
// runtime references it via ir.Syms.Growslice for relocation
// purposes even when no SSA edge reaches it.
//
// The body explicitly constructs a zero slice header before the
// throw rather than relying on `return slice{}`. The runtime.slice
// return type lowers to a 3-result wasm signature
// (anyref, i64, i64) per flatPrimitiveFields's TSTRUCT recursion;
// the SSA codegen for `return slice{}` produces only one boxed-
// slice value, mismatching the signature with "expected 3
// elements on the stack for return, found 1". The named-return
// form sidesteps that path: each component lives in its own
// local that the function epilogue pushes individually.
func growslice(oldPtr unsafe.Pointer, newLen, oldCap, num int, et *_type) (data unsafe.Pointer, len, cap int) {
	throw("runtime.growslice unreachable on wasm3 — append() is intrinsified to wasmgc array.new_default + array.copy")
	return
}

// slicecopy is verbatim from slice.go. Used by mbarrier.go's
// typedslicecopy when the element type has pointers. memmove is
// already wasm3-friendly (overlap-safe via unsafe.Slice + builtin
// copy), so this works on wasm3.
func slicecopy(toPtr unsafe.Pointer, toLen int, fromPtr unsafe.Pointer, fromLen int, width uintptr) int {
	if fromLen == 0 || toLen == 0 {
		return 0
	}

	n := fromLen
	if toLen < n {
		n = toLen
	}

	if width == 0 {
		return n
	}

	size := uintptr(n) * width

	if size == 1 {
		*(*byte)(toPtr) = *(*byte)(fromPtr)
	} else {
		memmove(toPtr, fromPtr, size)
	}
	return n
}
