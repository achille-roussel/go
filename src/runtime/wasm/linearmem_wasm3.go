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
// Implementation: the function bodies below are dead. The SSA
// intrinsic registration in cmd/compile/internal/ssagen/intrinsics.go
// replaces every call site with the corresponding
// OpWasm3{Write,Read,Reset}LinearMemory op, which lowers to an
// inline byte-copy loop in cmd/compile/internal/wasm3/ssa.go. The
// linear-memory arena's bump pointer lives in wasm global 0
// (declared by cmd/link/internal/wasm/asm3.go writeGlobalSec3); the
// arena itself grows on demand via memory.grow, so a program that
// never touches linear memory has zero pages allocated.
//
// Bridge call discipline: nested marshalling must save/restore the
// bump pointer by remembering the offset WriteLinearMemory returned
// and Reset'ing back to it after the syscall. A caller that grows
// the arena monotonically (no resets) eats linear memory across the
// program's lifetime — fine for one-shot programs, not for long-
// running ones. M4 (goroutines) will save/restore wasm global 0 at
// suspend/resume points.
package wasm

// WriteLinearMemory copies data into the linear-memory scratch arena
// and returns the absolute linear-memory offset of the copy. The
// caller passes that offset to a host import; after the call it
// should ResetLinearMemory back to a previously-saved offset to free
// the space. mem selects the target linear memory (wasip1 has one,
// so it is currently ignored).
//
// Intrinsic'd at SSA layer (OpWasm3WriteLinearMemory). The body
// below is dead — the compiler replaces the call before codegen.
// On any path where the intrinsic doesn't fire, panic so the bug
// surfaces immediately.
func WriteLinearMemory(mem int32, data []byte) uint32 {
	panic("runtime/wasm: WriteLinearMemory intrinsic not registered (ssagen.intrinsics, sys.ArchWasm3)")
}

// ReadLinearMemory copies len(data) bytes from the absolute linear-
// memory offset off into the caller-provided WasmGC slice data. The
// signature avoids an allocation on every read (the caller can reuse
// a single scratch buffer across many reads); callers that need a
// fresh slice each time can allocate one up front.
//
// Intrinsic'd at SSA layer (OpWasm3ReadLinearMemory).
func ReadLinearMemory(mem int32, data []byte, off uint32) {
	panic("runtime/wasm: ReadLinearMemory intrinsic not registered (ssagen.intrinsics, sys.ArchWasm3)")
}

// ResetLinearMemory rewinds the arena's bump pointer to off, freeing
// everything allocated above it. Pass an offset previously returned
// by WriteLinearMemory (the first allocation of a marshalling
// sequence) to release the whole sequence at once.
//
// Intrinsic'd at SSA layer (OpWasm3ResetLinearMemory).
func ResetLinearMemory(mem int32, off uint32) {
	panic("runtime/wasm: ResetLinearMemory intrinsic not registered (ssagen.intrinsics, sys.ArchWasm3)")
}
