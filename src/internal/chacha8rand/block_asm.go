// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm3

package chacha8rand

// block is the chacha8rand block function. On most architectures it is
// implemented in assembly (chacha8_$GOARCH.s); the non-assembly
// architectures get an assembly thunk (chacha8_stub.s) that tail-calls
// block_generic. GOARCH=wasm3 cannot use the linear-ABI assembly thunk —
// it has a pure-Go block in block_wasm3.go instead.
func block(seed *[4]uint64, blocks *[32]uint64, counter uint32)
