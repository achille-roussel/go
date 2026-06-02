// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 has a single-threaded model (per the wasm3 design — see
// runtime/atomic_wasm.go comment), so the sub-word atomic xchg
// implemented for every other arch via 4-byte-aligned 32-bit CAS
// reduces here to a plain non-atomic read/write. The 4-byte
// alignment trick also can't compile on wasm3 because it uses
// `uintptr(unsafe.Pointer(addr)) &^ 3` — pointer arithmetic that
// the wasm3 backend rejects (anyref has no scalar form).

package atomic

//go:nosplit
func goXchg8(addr *uint8, v uint8) uint8 {
	old := *addr
	*addr = v
	return old
}
