// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// memmove for GOARCH=wasm3 is implemented in Go rather than
// assembly. The wasm/wasm3 fork's obj backend can't lower the
// legacy memmove_wasm3.s body — that asm reads its arguments via
// `MOVD <name>+<off>(FP)` (the wasm linear-memory frame ABI), but
// the wasm3 calling convention passes args in registers. The asm
// function ends up emitted as a bare `unreachable` in the wasm,
// trapping every caller of memmove (growslice, append's fast path,
// makeslicecopy, slicecopy, etc.).
//
// The body uses `unsafe.Slice` to materialise byte-slice views of
// the source and destination, then delegates to the builtin
// `copy`. Go's `copy` on a slice is specified to handle overlap
// correctly (memmove semantics: "If the source and destination
// overlap, the resulting slice will contain the correct values"),
// so we don't need to write an overlap-detection helper. This is
// load-bearing on wasm3: comparing two unsafe.Pointers for
// ordering would require `uintptr(unsafe.Pointer(to)) >
// uintptr(unsafe.Pointer(from))`, the exact ptr-arith pattern the
// wasm3 SSA-gen fatal rejects (anyref has no scalar address). The
// builtin-copy form lowers in the wasm3 backend to byte-stream
// memory load/stores that the existing linear-memory path already
// supports, with overlap handling baked into the lowering.
//
// The function is single-goroutine on wasm3 so the standard
// memmove-overlap discipline (the public memmove contract) is all
// we need to satisfy; no concurrent reads/writes to worry about.
//
//go:linkname memmove
//go:nosplit
func memmove(to, from unsafe.Pointer, n uintptr) {
	if n == 0 || to == from {
		return
	}
	dst := unsafe.Slice((*byte)(to), n)
	src := unsafe.Slice((*byte)(from), n)
	copy(dst, src)
}
