// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// Stage F (doc/wasm3-m3-stage-f-interfaces.md) — early stub.
//
// The default `interequal` / `nilinterequal` / `efaceeq` / `ifaceeq`
// (in alg_iface_default.go) read a func-pointer field off a linear-
// memory `*_type` struct; the resulting func-valued SSA value is
// anyref-typed by wasm3's per-value-local lowering (TFUNC →
// wasm3ValAnyref) while the load itself is `i64.load`, so wasm
// validation rejects the body. Until the interface machinery moves
// onto a wasmgc itab / `$go.type` representation, the stubs below
// trap rather than execute. Programs that don't use interface
// equality (almost all of them, in practice) link these in via the
// runtime's keep-alive list but never call them.

func interequal(p, q unsafe.Pointer) bool {
	throw("wasm3: interequal not implemented (Stage F)")
	return false
}
func nilinterequal(p, q unsafe.Pointer) bool {
	throw("wasm3: nilinterequal not implemented (Stage F)")
	return false
}
func efaceeq(t *_type, x, y unsafe.Pointer) bool {
	throw("wasm3: efaceeq not implemented (Stage F)")
	return false
}
func ifaceeq(tab *itab, x, y unsafe.Pointer) bool {
	throw("wasm3: ifaceeq not implemented (Stage F)")
	return false
}
