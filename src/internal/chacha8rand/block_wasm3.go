// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package chacha8rand

// block on GOARCH=wasm3 is pure Go: the chacha8_stub.s assembly thunk
// uses the linear wasm ABI, which cannot pass wasm3's boxed array refs
// (*[4]uint64 / *[32]uint64 are WasmGC array refs, not linear addresses).
// Call block_generic directly. See doc/wasm3-design.md.
func block(seed *[4]uint64, blocks *[32]uint64, counter uint32) {
	block_generic(seed, blocks, counter)
}
