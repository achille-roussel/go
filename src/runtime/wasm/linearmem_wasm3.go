// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// Package wasm exposes the wasm3 host-boundary primitives: the three
// functions that move bytes between WasmGC memory (where all Go data
// lives on GOARCH=wasm3) and the module's linear memory (the only
// region a host — WASI, or another non-WasmGC module — can read).
//
// It is the FFI-boundary analog of runtime/cgo: third-party code
// writing //go:wasmimport / //go:wasmexport on wasm3 uses these to
// serialize arguments into linear memory and read results back,
// keeping the host ABI in terms of primitive numeric types only
// (a "pointer" handed to the host is a uint32 linear-memory offset).
//
// See doc/wasm3-stage-j-plan.md (Piece 5) for the model: linear
// memory is a serialization layer reached ONLY through these three
// primitives, which live in a separate hand-written wasm module (the
// "go runtime" module) that is dynamically linked to the program. That
// module declares the linear memory and the bump-pointer global and
// exports these functions; the program imports them. The compiler thus
// emits WasmGC only, end to end — it never sees linear memory. As the
// compiler sees these calls, the arguments and results are WasmGC-only
// (a []byte is a WasmGC (ref $go.bytes); mem/off/len/the return are
// i32); the runtime module reads the []byte with array.get_u and writes
// linear memory with i32.store8 (and the reverse).
//
// Once ref-typed //go:wasmimport lands (the []byte argument must cross
// the boundary as a (ref $go.bytes), not an i32), the bodies below
// become //go:wasmimport go_runtime signatures, e.g.
//
//	//go:wasmimport go_runtime WriteLinearMemory
//	func WriteLinearMemory(mem int32, data []byte) uint32
//
// NOT YET FUNCTIONAL. Until that backend support exists the bodies trap
// (a //go:wasmimport with a []byte parameter does not yet compile on
// wasm3). A pure-wat experiment has confirmed the dynamic-linking model
// works (cross-module (ref $go.bytes) matching, array.get_u, i32.store8,
// the bump global); see the de-risk note in the plan.
package wasm

// WriteLinearMemory copies data into the linear-memory scratch arena
// and returns the absolute linear-memory offset of the copy. The
// caller passes that offset to a host import; after the call it should
// ResetLinearMemory back to a previously-saved offset to free the
// space. mem selects the target linear memory (wasip1 has one, so it
// is currently ignored).
func WriteLinearMemory(mem int32, data []byte) uint32 {
	panic("runtime/wasm: WriteLinearMemory pending ref-typed go:wasmimport (dynamic-linked go_runtime module)")
}

// ReadLinearMemory returns a fresh WasmGC []byte holding a copy of
// length bytes at the absolute linear-memory offset off — used to pull
// a host-written result (an out-parameter cell, a filled buffer) back
// into Go memory.
func ReadLinearMemory(mem int32, off, length uint32) []byte {
	panic("runtime/wasm: ReadLinearMemory pending ref-typed go:wasmimport (dynamic-linked go_runtime module)")
}

// ResetLinearMemory rewinds the arena's bump pointer to off, freeing
// everything allocated above it. Pass an offset previously returned by
// WriteLinearMemory (the first allocation of a marshalling sequence)
// to release the whole sequence at once.
func ResetLinearMemory(mem int32, off uint32) {
	panic("runtime/wasm: ResetLinearMemory pending ref-typed go:wasmimport (dynamic-linked go_runtime module)")
}
