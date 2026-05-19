// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// mallocgc on wasm3 redirects every allocation to the bump-heap
// arena. The standard mallocgc walks the per-P mcache, the mheap
// span lists, and the fixalloc free lists for metadata, none of
// which the wasm3 backend can lower today (the M2 cutover to host
// WasmGC has not been completed for those subsystems; see
// doc/wasm3-m3-stage-f-interfaces.md).
//
// Callers can be split into two groups by which argument carries
// the requested byte count:
//   - convT* (interface boxing): pass `typ` and trust typ.Size_;
//     size==typ.Size_.
//   - growslice / persistentalloc / etc: pass the byte count in
//     `size` and may pass `typ==nil` (e.g. the noscan growslice
//     branch).
//
// Routing through wasm3BumpAlloc(size) honours both shapes — it
// uses the explicit byte count instead of dereferencing typ — so
// `mallocgc(capmem, nil, false)` from growslice no longer faults.
//
//go:linkname mallocgc
func mallocgc(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	_ = typ
	_ = needzero
	return wasm3BumpAlloc(size)
}
