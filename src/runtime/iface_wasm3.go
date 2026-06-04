// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

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

// convT* helpers — emitted by the compiler from walk/convert.go's
// dataWord when boxing a non-static value into an interface.
//
// On non-wasm3, each helper mallocgc's a heap copy of val and returns
// its address as unsafe.Pointer; the IMake call site uses that
// address as the iface's data word.
//
// On wasm3 the genuine implementation needs to construct a
// $go.box.<T> WasmGC cell wrapping val and return it as the iface
// data ref. That hits two blockers: (a) `&val` for a string/slice
// parameter currently lowers to a mix of $go.box.string struct.new
// AND linear-memory stack writes (the legacy multi-word return ABI
// shape), which wasm-emission rejects and degrades to `unreachable`;
// (b) the wasm3 runtime doesn't have mallocgc or the staticuint64s
// small-int cache. Solving (a) requires extending the wasm3 backend's
// addressable-locals lowering to cover the convT* return-slot path
// (work tracked under #128 / M4-completion).
//
// These stubs return ref.null any so the static-data panic+recover+
// assert path (the #218 / #220 repro) and any program that doesn't
// dynamically box continue to link and run. Programs that DO call
// convT* at runtime trap with "illegal cast" at the assertion site
// because the data slot ends up nil — surfacing the gap clearly
// instead of corrupting silently.

//go:linkname convT
func convT(t *_type, v unsafe.Pointer) unsafe.Pointer {
	// `v` already names a heap-allocated value on wasm3 (the caller's
	// `&heap_local` already boxes via addressable-locals). Forward it.
	return v
}

//go:linkname convTnoptr
func convTnoptr(t *_type, v unsafe.Pointer) unsafe.Pointer {
	return v
}

//go:linkname convT16
func convT16(val uint16) unsafe.Pointer {
	return nil
}

//go:linkname convT32
func convT32(val uint32) unsafe.Pointer {
	return nil
}

//go:linkname convT64
func convT64(val uint64) unsafe.Pointer {
	return nil
}

//go:linkname convTstring
func convTstring(val string) unsafe.Pointer {
	return nil
}

//go:linkname convTslice
func convTslice(val []byte) unsafe.Pointer {
	return nil
}
