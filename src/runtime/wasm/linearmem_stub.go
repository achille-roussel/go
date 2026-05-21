// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm3

package wasm

// The linear-memory boundary primitives only exist on GOARCH=wasm3,
// where Go data lives in WasmGC memory and a separate linear memory
// is the host-readable region. On every other target there is no
// such split, so these are unreachable stubs that keep the package
// buildable in the standard library for all architectures.

func WriteLinearMemory(mem int32, data []byte) uint32 {
	panic("runtime/wasm: WriteLinearMemory is only supported on GOARCH=wasm3")
}

func ReadLinearMemory(mem int32, off, length uint32) []byte {
	panic("runtime/wasm: ReadLinearMemory is only supported on GOARCH=wasm3")
}

func ResetLinearMemory(mem int32, off uint32) {
	panic("runtime/wasm: ResetLinearMemory is only supported on GOARCH=wasm3")
}
