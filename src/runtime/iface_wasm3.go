// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import _ "unsafe" // for go:linkname

// wasm3 mirror of iface.go.
//
// iface.go is tagged !wasm3 because the type-assert / interface-
// switch caches (`abi.TypeAssertCache`, `abi.InterfaceSwitchCache`)
// store their (Typ, Itab) entries as `uintptr(unsafe.Pointer(typ))`
// — write-barrier-bypass uintptr storage that anyref cannot
// represent. The 4 ptr-arith sites (iface.go:545, 546, 651, 653,
// 554, 661) are all in the cache rehash paths.
//
// On wasm3 the interface dispatch machinery is owned by the
// compiler: per-(I,T) vtab singletons emitted at link time
// (commits 157-160) replace the runtime itab table and getitab,
// and interface assertions / switches lower through wasm3-specific
// SSA ops instead of calling typeAssert / interfaceSwitch. The
// runtime functions in iface.go are therefore unreachable on
// wasm3 — this file provides only the symbols the rest of the
// runtime build still references after gating.

// itabsinit is called from proc.go's schedinit. On non-wasm3 it
// scans loaded modules for itabs to install in the global table.
// Wasm3 has no global itab table — per-(I,T) vtab singletons live
// in the compiler-emitted output — so the body is a no-op.
func itabsinit() {}

// unreachableMethod is the dead-method-replacement target the
// linker emits in the non-wasm3 itab table. Wasm3's per-(I,T)
// vtabs don't go through the linker's iface DCE pass yet, but
// the symbol must exist so external callers that linkname it
// resolve. Throw if reached.
func unreachableMethod() {
	throw("runtime.unreachableMethod: called on wasm3 (per-(I,T) vtab dispatch should never resolve to this)")
}

// panicdottypeE / I / Nil — type-assertion failure helpers
// emitted by the compiler for `x.(T)` conversions. Verbatim
// from iface.go (no ptr-arith).

func panicdottypeE(have, want, iface *_type) {
	panic(&TypeAssertionError{iface, have, want, ""})
}

func panicdottypeI(have *itab, want, iface *_type) {
	var t *_type
	if have != nil {
		t = have.Type
	}
	panicdottypeE(t, want, iface)
}

func panicnildottype(want *_type) {
	panic(&TypeAssertionError{nil, nil, want, ""})
}

// staticuint64s is the small-int cache `convT*` uses on non-wasm3
// to avoid an allocation when boxing an integer < 256 into an
// interface. Wasm3 boxes integers through ref.cast / struct.new
// directly (no linear-memory backing), so the cache is unused —
// but `getStaticuint64s` is //go:linkname'd to reflect.Value
// integer-boxing, so the symbol must exist. Keep the array at
// the same size for layout parity with the non-wasm3 file.
var staticuint64s [256]uint64

//go:linkname getStaticuint64s
func getStaticuint64s() *[256]uint64 {
	return &staticuint64s
}
