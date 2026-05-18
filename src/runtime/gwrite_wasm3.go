// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// gwrite on wasm3 is a no-op for now. The default
// implementation reads a func-typed field off the linear-
// memory `*g` struct and calls writeErrData(*byte, int32) with
// the slice's backing pointer, but on wasm3 the slice's
// backing is a wasmgc-typed `(ref (array i8))`, not a `*byte`,
// so the call would fail wasm validation. Stage E's wasmgc-
// to-linear-memory slice-marshalling (the same workstream
// that's blocking composite captures and slice-in-struct args)
// has to land before gwrite can route bytes to the wasip1
// write1 path properly.
//
// Most test programs print via runtime.printstring (which has
// its own linear-memory path that works) and don't reach
// gwrite. Programs that *do* — primarily panic / fatal /
// some interface-printing paths — silently drop their writes.
// Marked TODO for the Stage E slice-marshalling arc.
func gwrite(b []byte) {
	_ = b
}
