// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// _rt0_wasm3_wasip1 is the wasip1 entry point for GOARCH=wasm3. The
// linker exports it as `_start`; the host calls it once at module
// instantiation. On wasm3 there is no linear-memory stack to set up
// and no Go-side scheduler bootstrap yet, so the entry is a thin
// wrapper that calls into main.main directly. Real bootstrap
// (continuations-based scheduler, g0/m0 init) will land here during
// M4.
//
// _rt0_wasm3_wasip1 replaces the .s-defined entry point used by
// GOARCH=wasm. The wasm linear-memory ABI required hand-written
// assembly (stack-pointer init, manual call into rt0_go); wasm3 with
// WasmGC + typed functions needs neither, so the entry is a normal
// Go function. cmd/link/internal/wasm/asm.go and
// cmd/internal/obj/wasm/wasm3obj.go look up the symbol under its
// runtime-package-qualified name.
//
//go:nosplit
func _rt0_wasm3_wasip1() {
	main_main()
}

// _rt0_wasm3_wasip1_lib is the BuildModeCShared entry (exported as
// `_initialize`). For now it forwards to the executable entry.
//
//go:nosplit
func _rt0_wasm3_wasip1_lib() {
	_rt0_wasm3_wasip1()
}
