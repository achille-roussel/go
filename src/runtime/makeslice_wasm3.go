// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// makeslice / makeslice64 are the runtime entry points the standard
// `make([]T, len, cap)` lowers to. On wasm3 these calls are
// intrinsified at the SSA layer (OpWasm3MakeSlice →
// array.new_default $go.array.T (cap)), so a live call reaching
// either body indicates a missed intrinsification. Trap so it
// surfaces immediately rather than silently corrupting state
// against a defunct linear-memory bump arena. The symbols must stay
// — runtime/slice.go and other files linkname-bind them.
//
// wasm3HeapAlloc is the linkname target used by
// internal/runtime/maps/runtime_faststr_wasm3.go's grow path; that
// file's wasm3MapEnsureCap is itself dead under per-type-maps
// (see [[wasm3-per-type-maps]] DONE), so the symbol is unreachable
// in shipped binaries. Kept as a trapping stub so the linkname
// resolves at link time.

//go:linkname makeslice
//go:nosplit
func makeslice(et *_type, len, cap int) unsafe.Pointer {
	_ = et
	_ = len
	_ = cap
	throw("wasm3: runtime.makeslice called — allocation site failed to intrinsify to array.new_default")
	return nil
}

//go:nosplit
func makeslice64(et *_type, len64, cap64 int64) unsafe.Pointer {
	_ = et
	_ = len64
	_ = cap64
	throw("wasm3: runtime.makeslice64 called — allocation site failed to intrinsify to array.new_default")
	return nil
}

//go:linkname wasm3HeapAlloc
//go:nosplit
func wasm3HeapAlloc(nbytes uintptr) unsafe.Pointer {
	_ = nbytes
	throw("wasm3: runtime.wasm3HeapAlloc called — M2 bump arena retired in M3.5")
	return nil
}
