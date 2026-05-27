// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

package runtime

import "runtime/wasm"

// newprocJSWasm3 is the js/wasm3 dispatch entry for `go fn()`. The
// wasm3 compiler routes OGO directly to this symbol instead of the
// standard runtime.newproc, sidestepping the scheduler that doesn't
// exist on js/wasm3.
//
// fn arrives as a Go func() value — i.e. a `(ref $closureCtx)` at
// the wasm level — exactly what runtime/wasm.Spawn expects. No
// *funcval→func() unsafe cast needed (the cast can't lower on wasm3:
// typed-ref locals aren't address-takeable, so the standard
// `*(*func())(unsafe.Pointer(&fn))` idiom compiles to `unreachable`).
//
//go:nowritebarrier
//go:nosplit
func newprocJSWasm3(fn func()) {
	wasm.Spawn(fn)
}
