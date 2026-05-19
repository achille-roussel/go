// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// memmove for GOARCH=wasm3 is implemented in Go rather than assembly.
// The wasm/wasm3 fork's obj backend can't lower the legacy
// memmove_wasm3.s body — that asm reads its arguments via
// `MOVD <name>+<off>(FP)` (the wasm linear-memory frame ABI), but
// the wasm3 calling convention passes args in registers. The asm
// function ends up emitted as a bare `unreachable` in the wasm,
// trapping every caller of memmove (growslice, append's fast path,
// makeslicecopy, etc.).
//
// This Go version uses the standard register ABI, so the wasm3
// backend lowers the parameters correctly. The body itself is the
// canonical "memory.copy with overlap handling": forward copy when
// the destination is before the source (or fully outside the source
// range), backward copy otherwise. Both directions are byte-by-byte
// — the wasm3 backend doesn't have an `OpMove`-with-dynamic-size
// intrinsic yet, and adding one is a separate workstream. Byte-by-
// byte is correct and uses the linear-memory load/store path the
// backend already supports.
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
	if uintptr(to) > uintptr(from) && uintptr(to) < uintptr(from)+n {
		// Overlapping with dst after src — copy backward.
		for i := n; i > 0; i-- {
			*(*byte)(unsafe.Add(to, i-1)) = *(*byte)(unsafe.Add(from, i-1))
		}
		return
	}
	for i := uintptr(0); i < n; i++ {
		*(*byte)(unsafe.Add(to, i)) = *(*byte)(unsafe.Add(from, i))
	}
}
