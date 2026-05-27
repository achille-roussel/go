// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// _rt0_wasm3_js is the GOOS=js entry point for GOARCH=wasm3. The
// linker exports it as `run` so the JS-side wasm_exec_wasm3.js shim
// can call it. On JS hosts the entry will eventually be JSPI-
// promising (Phase 2) so it returns a Promise the shim awaits; for
// now (Phase 0 minimum) it just calls main.main directly and
// returns synchronously. Filename `rt0_jswasm3` (no implicit
// GOOS_GOARCH suffix) lets the explicit //go:build tag stand alone.

package runtime

//go:nosplit
func _rt0_wasm3_js() {
	main_main()
}
