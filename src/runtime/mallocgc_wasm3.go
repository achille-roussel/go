// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// mallocgc on wasm3 redirects every allocation to the bump-heap
// newobject. The standard mallocgc walks the per-P mcache, the
// mheap span lists, and the fixalloc free lists for metadata,
// none of which the wasm3 backend can lower today (the M2 cutover
// to host WasmGC has not been completed for those subsystems; see
// doc/wasm3-m3-stage-f-interfaces.md).
//
// Every convT* path — convT, convT16/32/64, convTstring, convTslice
// — funnels through mallocgc to box an interface payload, so this
// shim is what makes empty-interface boxing work on wasm3.
//
// The size==0 short-circuit matches the standard implementation: a
// zero-sized allocation returns &zerobase so distinct empty values
// have a stable, distinguishable address.
//
//go:linkname mallocgc
func mallocgc(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	if size == 0 {
		return unsafe.Pointer(&zerobase)
	}
	return newobject(typ)
}
