// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// printstring for GOARCH=wasm3 is a temporary no-op during the Stage
// J cutover. The previous body did
//
//	write1(2, unsafe.Pointer(unsafe.StringData(s)), int32(len(s)))
//
// which worked while strings travelled as (i64 data, i64 len) — the
// data pointer was a linear-memory address fd_write could read
// directly. After J.1's signature flip strings travel as (anyref
// data, i64 len); the data is a wasmgc array ref that fd_write
// cannot consume without first being materialised into linear-memory
// scratch. The materialisation path needs SSA support for byte-level
// indexing on a wasmgc-backed string (array.get_u on the data ref),
// which is not yet wired up — see doc/wasm3-stage-j-progress.md
// Piece γ.
//
// During the cutover the bring-up tests use wasmtime's trap
// backtrace rather than print() for observability; the test harness
// asserts exit codes and module validation, not stdout. Once the
// wasmgc byte-indexing intrinsic lands, the body re-acquires its
// fd_write call site via a wasmgc → linear scratch copy.
func printstring(s string) {
	_ = s
}
