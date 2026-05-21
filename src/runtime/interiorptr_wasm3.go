// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import _ "unsafe" // for go:linkname

// wasm3InteriorPtrByte is the fat-pointer constructor for a `*byte`
// pointing at element i of a wasmgc `[]byte`. The compiler
// intrinsifies the call at the SSA layer into OpWasm3InteriorPtr
// (allocating a (struct (ref any) i32) wrapper of type
// $go.iptr.i8); the body here is a panic-stub because the call
// should never reach the runtime — if it did, the intrinsic
// failed.
//
// Each pointee-class needs its own typed wrapper because Go's type
// system doesn't allow `[]byte` to flow through a `[]any` parameter
// without explicit conversion; the conversion would lose the elem
// type the intrinsic recovers from n.Args[0].Type(). So we expose
// one typed helper per scalar class as needed.
//
// Loading and storing through the returned `*T` is handled by the
// Wasm3.rules patterns that rewrite
// (I64Load* / I64Store* (Wasm3InteriorPtr ...) ...) into
// Wasm3LoadInterior / Wasm3StoreInterior — so user code just writes
// `*p` / `*p = v` after this constructor.
//
// Exposed via linkname so helpers in other packages (Piece 4) can
// reach the intrinsic. No production caller as of Piece 3; the
// codegen sits dormant until Piece 4 wires up helpers like
// runtime.printstring or internal/runtime/maps faststr to use it.
//
//go:linkname wasm3InteriorPtrByte
//go:nosplit
func wasm3InteriorPtrByte(s []byte, i int) *byte {
	_ = s
	_ = i
	throw("wasm3InteriorPtrByte: not intrinsified by the wasm3 backend")
	return nil
}
