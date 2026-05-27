// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// Memory plumbing for GOOS=js GOARCH=wasm3. The wasm3 heap lives in
// WasmGC types (host-managed); only the M3.5 linear-memory bridge
// arena uses linear memory, and it grows via the wasm3 backend's
// memory.grow emission rather than via a host data-view reset.
//
// resetMemoryDataView signals the JS host that linear memory grew so
// the JS-side DataView can be re-bound. Mirrors mem_js.go's wasmimport
// shape — Phase 1 binds the JS handler.

package runtime

//go:wasmimport gojs runtime.resetMemoryDataView
func resetMemoryDataView()
