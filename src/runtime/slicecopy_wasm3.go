// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// wasm3SliceCopy is the wasm3-only slice-to-slice memcpy that
// walkCopy on GOARCH=wasm3 emits in place of runtime.memmove.
// The compiler intrinsifies the call at the SSA layer into
// OpWasm3ArrayCopy (`array.copy` on the two wasmgc backings);
// the body here is a panic-stub because the call should never
// reach the runtime — if it did, the intrinsic failed and
// something else is wrong.
//
// Generic-instantiated like runtime.memmove (matched via
// LookupRuntime with the elem type substituted into *any) so
// the call sites' &dst[0] / &src[0] *T arguments check out at
// the Go type level. n is the element count (NOT byte count).
//
//go:nosplit
func wasm3SliceCopy(et *_type, dst, src unsafe.Pointer, n int) {
	_ = et
	_ = dst
	_ = src
	_ = n
	throw("wasm3SliceCopy: not intrinsified by the wasm3 backend")
}
