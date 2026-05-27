// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// newobject is the runtime entry for `new(T)`. On wasm3, every
// reachable allocation site has been intrinsified at the SSA layer
// to struct.new / struct.new_default / array.new_default, so this
// body is unreachable in well-formed wasm3 programs. The function
// must still exist as a link-time symbol — internal/runtime/maps
// references it via //go:linkname — but its body is a trap so any
// future allocation site that fails to intrinsify surfaces as a
// runtime panic, not a silent fallback into a quietly-resurrected
// linear-memory bump arena. (The M2-era wasm3Heap / wasm3BumpAlloc
// scaffolding it used to call is deleted in this commit; see
// Phase 0 of the M3.5 plan in
// .claude/plans/retire-go-runtime-wat-static-link.md.)
//
//go:nosplit
func newobject(typ *_type) unsafe.Pointer {
	_ = typ
	throw("wasm3: runtime.newobject called — allocation site failed to intrinsify to struct.new")
	return nil
}
