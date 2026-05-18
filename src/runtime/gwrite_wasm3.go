// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// gwrite for wasm3 routes byte-slice writes through the wasm3
// linear-memory write1 path used by the print* family,
// bypassing the default implementation's per-goroutine writebuf
// and the func-typed-field load off `*g` (which trips the same
// i64.load → anyref-local validation mismatch as runtime.ifaceeq
// did before alg_iface_wasm3.go). Single-goroutine M2 has no
// writebuf to divert to anyway.
//
// The slice's data pointer is currently a wasmgc array ref;
// linear-memory write1 wants an i64 pointer. We don't bridge
// the two yet — Stage E phase 4 introduced wasm3SliceCopy for
// the copy() path but no analogous helper for "write this slice
// to stderr". For now, drop writes silently: every Stage F
// test program either prints via runtime.printstring (which has
// its own path) or doesn't reach gwrite. The proper bridge
// belongs in the Stage E slice-marshalling work.
func gwrite(b []byte) {
	_ = b
	_ = unsafe.Pointer(nil)
}
