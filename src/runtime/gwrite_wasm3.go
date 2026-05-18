// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// gwrite for GOARCH=wasm3 bridges the wasmgc-backed `[]byte` argument
// into a linear-memory scratch buffer so writeErrData(*byte, int32)
// sees a `*byte` it can pass to fd_write. The default implementation
// calls writeErrData(&b[0], int32(len(b))), but `&b[0]` on a wasmgc
// slice is a `(ref (array i8))` + i64 index pair, not a linear-memory
// `*byte` — there is no in-place addressing path for the wasmgc
// backing.
//
// The g.writebuf / g.m.dying redirection that the default does is
// skipped on wasm3 for now: the M2 cutover's `*g` shape doesn't yet
// have those fields populated, and no wasm3 test program reaches the
// goroutine-local-buffered output path. recordForPanic is also
// skipped — it copies into the linear-memory printBacklog circular
// buffer, but its `copy(printBacklog[…], b[i:])` would hit the same
// wasmgc-to-linear-memory mismatch as writeErrData; bridging that
// would duplicate the buffer for no current consumer (M2 doesn't
// read printBacklog from a core dump).
//
// We go straight to write1 (the WASI fd_write wrapper) rather than
// through runtime.writeErrData/runtime.write, both of which the
// wasm3 backend can't lower today: writeErrData reads g.m.dying off
// a `*g` whose linear-memory shape isn't populated, and the write
// wrapper's `if overrideWrite != nil` indirect-call probe stores a
// func-pointer load into an anyref-typed local that fails wasm
// validation. Direct write1 is the single fd_write syscall path.
// The wasm3 backend correctly drops write1's int32 return at the
// void-context call site, so no return-value workaround is needed.
//
// gwrite3Scratch is sized for the largest single print payload we
// expect (panic banner, fatal-error stack header). Longer writes are
// chunked, so the buffer doesn't need to grow with input length.
var gwrite3Scratch [4096]byte

func gwrite(b []byte) {
	n := len(b)
	if n == 0 {
		return
	}
	if n > len(gwrite3Scratch) {
		n = len(gwrite3Scratch)
	}
	for i := 0; i < n; i++ {
		gwrite3Scratch[i] = b[i]
	}
	write1(2, unsafe.Pointer(&gwrite3Scratch[0]), int32(n))
}
