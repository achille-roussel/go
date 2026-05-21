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

// wasm3TestInteriorByteAt and wasm3TestInteriorByteSet are smoke
// tests that exercise the fat-pointer pipeline end to end from
// runtime code: a direct call to wasm3InteriorPtrByte intrinsifies
// to OpWasm3InteriorPtr, and the *p / *p = v dereferences rewrite
// through the load/store-through-InteriorPtr rules in Wasm3.rules.
// Exposed via linkname so a wasm3 test program (which can't trigger
// the intrinsic on a linkname'd call from its own package) can
// drive them.
//
//go:linkname wasm3TestInteriorByteAt
//go:nosplit
func wasm3TestInteriorByteAt(s []byte, i int) byte {
	p := wasm3InteriorPtrByte(s, i)
	return *p
}

//go:linkname wasm3TestInteriorByteSet
//go:nosplit
func wasm3TestInteriorByteSet(s []byte, i int, v byte) {
	p := wasm3InteriorPtrByte(s, i)
	*p = v
}

// wasm3TestInteriorCopy copies n bytes from src to dst element by
// element through fat pointers. This is the exact loop shape the
// Stage J string/byte materialisation helpers (printstring, gwrite)
// use — `for i { dst[i] = src[i] }` — but kept entirely within
// wasmgc `(array i8)` backings so it exercises load-and-store
// through interior pointers without the wasip1 linear-memory
// boundary. Each iteration materialises a fresh InteriorPtr for the
// read and another for the write, which is what the (I64Load* /
// I64Store* (InteriorPtr ...)) rewrite rules match.
//
//go:linkname wasm3TestInteriorCopy
//go:nosplit
func wasm3TestInteriorCopy(dst, src []byte, n int) {
	for i := 0; i < n; i++ {
		d := wasm3InteriorPtrByte(dst, i)
		s := wasm3InteriorPtrByte(src, i)
		*d = *s
	}
}
