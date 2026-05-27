// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !(js && wasm3)

package runtime

func newprocJSWasm3(fn func()) {
	throw("newprocJSWasm3 called on non-js/wasm3 build")
}
