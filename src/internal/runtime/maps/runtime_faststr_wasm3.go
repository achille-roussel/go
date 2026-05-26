// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 Go maps are a deliberately simple alternate implementation:
// a flat array of (string-key, value-slot) pairs in linear memory,
// scanned linearly on each access. Performance is O(n) for every
// operation; this is the bring-up rung, not the long-term home.
//
// The shared internal/runtime/maps implementation (Swiss-table groups,
// extendible hashing, control-byte SIMD probing, growing tables of
// tables) leans heavily on `unsafe.Pointer` arithmetic, pointer-
// receiver methods on stack-allocated wrappers (groupReference),
// and a memory model that assumes the Go heap is addressable linear
// memory. None of those assumptions hold for wasm3 (the heap is
// wasmgc, `&local` lands on an SP-relative auto the obj backend
// can't lower, and unsafe.Pointer arithmetic can't cross the
// wasmgc/linear-memory split). Compiling that code on wasm3
// produces a stack of `unreachable` stubs that trap every map call.
//
// The plan in doc/wasm3-design.md is to revisit this once the
// runtime, codegen, and ABI for wasmgc-backed slices/strings have
// matured (Stage J and later). Until then a simple, slow,
// correctness-only implementation is enough to bring up everything
// that uses maps (tests, the type system, reflect, the linker's
// own internal maps).
//
// This file replaces the runtime_faststr family only — the path the
// compiler picks for map[string]V. The other key-class entry points
// (runtime_fast32, runtime_fast64, and the generic runtime_*) are
// not yet ported and will trap if reached.

package maps

import (
	"internal/abi"
	"unsafe"
)

// wasm3MapData is the wasm3 map's backing storage, referenced via
// the existing Map.dirPtr field. Keeping the Map struct shape lets
// `len(m)` (which the compiler lowers to a load of Map.used at
// fixed offset 0) keep working, and lets the per-Map control fields
// (used, seed, writing, …) stay at their canonical offsets for any
// future wasm3-aware introspection.
//
// Layout: parallel host-GC backings for keys and values. `keys` is a
// []string whose backing is a WasmGC (array (ref $go.string))
// allocated via make() — the compiler intrinsifies the make call to
// OpWasm3MakeSlice → array.new_default, so no mallocgc/wasm3HeapAlloc.
// `values` is a []byte of cap*typ.Elem.Size_ bytes — a WasmGC
// (array (mut i8)) — that the caller indexes into at typed offsets.
// The caller's typed access via `*(*T)(p) = v` lowers through
// wasm3's interior-pointer (iptr) mechanism: &d.values[off] returns
// a (ref $go.iptr.<T>) fat pointer, and the typed store goes through
// the array.set $go.bytes accessor pair the compiler emits for that
// iptr class. No mallocgc, no wasm3HeapAlloc — fully host-GC.
type wasm3MapData struct {
	cap    uintptr
	keys   []string // WasmGC-backed array of string headers
	values []byte   // WasmGC-backed flat byte storage indexed at typed offsets
}

// wasm3MapDataAt loads m's wasm3 backing struct (allocated lazily on
// first insert by wasm3MapEnsureCap).
func wasm3MapDataAt(m *Map) *wasm3MapData {
	return (*wasm3MapData)(m.dirPtr)
}

// wasm3MapKeyAt returns a pointer to the string header at index i in
// d.keys. The []string backing is WasmGC, so indexing lowers to
// array.get $go.<string-array> via wasm3 ArrayElemRef and the address
// is a typed boxed-ref interior pointer.
func wasm3MapKeyAt(d *wasm3MapData, i uintptr) *string {
	return &d.keys[i]
}

// wasm3MapValueAt returns a pointer to the value slot at index i in
// d.values for elements of size valueSize. The returned unsafe.Pointer
// is the address of the byte at offset i*valueSize within the WasmGC
// (array (mut i8)) backing; the caller's typed access (*(*T)(p)) lowers
// through wasm3's interior-pointer accessor pair.
func wasm3MapValueAt(d *wasm3MapData, i, valueSize uintptr) unsafe.Pointer {
	return unsafe.Pointer(&d.values[i*valueSize])
}

// wasm3MapEnsureCap makes sure m has room for at least one more
// element, growing the backing arrays if needed (and allocating
// them on the first insert).
func wasm3MapEnsureCap(m *Map, valueSize uintptr) {
	d := wasm3MapDataAt(m)
	if d == nil {
		// Bump-heap-free: new(wasm3MapData) intrinsifies on wasm3 to
		// OpWasm3StructNewDefault, allocating a WasmGC struct via
		// struct.new_default $go.wasm3MapData — no mallocgc, no
		// wasm3HeapAlloc, just a typed host-GC allocation. The other
		// two wasm3HeapAlloc calls below are migrated next; this is
		// the first step of [[wasm3-no-linear-malloc]]'s plan to
		// retire wasm3Heap entirely.
		d = new(wasm3MapData)
		m.dirPtr = unsafe.Pointer(d)
	}
	if uintptr(m.used) < d.cap {
		return
	}
	newCap := d.cap * 2
	if newCap < 8 {
		newCap = 8
	}
	newKeys := make([]string, newCap)
	newValues := make([]byte, newCap*valueSize)
	if d.cap > 0 {
		// Both backings are WasmGC arrays — copy intrinsifies to
		// OpWasm3ArrayCopy / array.copy, no memmove needed.
		copy(newKeys, d.keys[:uintptr(m.used)])
		copy(newValues, d.values[:uintptr(m.used)*valueSize])
	}
	d.keys = newKeys
	d.values = newValues
	d.cap = newCap
}

// wasm3MapFind returns the index of key in m, or -1 if not present.
func wasm3MapFind(m *Map, key string) int {
	d := wasm3MapDataAt(m)
	if d == nil {
		return -1
	}
	for i := uintptr(0); i < uintptr(m.used); i++ {
		if *wasm3MapKeyAt(d, i) == key {
			return int(i)
		}
	}
	return -1
}

//go:linkname runtime_mapaccess1_faststr runtime.mapaccess1_faststr
func runtime_mapaccess1_faststr(typ *abi.MapType, m *Map, key string) unsafe.Pointer {
	if m == nil || m.used == 0 {
		return unsafe.Pointer(&zeroVal[0])
	}
	i := wasm3MapFind(m, key)
	if i < 0 {
		return unsafe.Pointer(&zeroVal[0])
	}
	return wasm3MapValueAt(wasm3MapDataAt(m), uintptr(i), typ.Elem.Size_)
}

//go:linkname runtime_mapaccess2_faststr runtime.mapaccess2_faststr
func runtime_mapaccess2_faststr(typ *abi.MapType, m *Map, key string) (unsafe.Pointer, bool) {
	if m == nil || m.used == 0 {
		return unsafe.Pointer(&zeroVal[0]), false
	}
	i := wasm3MapFind(m, key)
	if i < 0 {
		return unsafe.Pointer(&zeroVal[0]), false
	}
	return wasm3MapValueAt(wasm3MapDataAt(m), uintptr(i), typ.Elem.Size_), true
}

//go:linkname runtime_mapassign_faststr runtime.mapassign_faststr
func runtime_mapassign_faststr(typ *abi.MapType, m *Map, key string) unsafe.Pointer {
	if m == nil {
		panic(errNilAssign)
	}
	if i := wasm3MapFind(m, key); i >= 0 {
		// Refresh the key in place — the caller's storage may have
		// changed even when the comparison matched. Mirrors the
		// shared implementation's "key needs update" branch.
		d := wasm3MapDataAt(m)
		*wasm3MapKeyAt(d, uintptr(i)) = key
		return wasm3MapValueAt(d, uintptr(i), typ.Elem.Size_)
	}
	wasm3MapEnsureCap(m, typ.Elem.Size_)
	d := wasm3MapDataAt(m)
	idx := uintptr(m.used)
	*wasm3MapKeyAt(d, idx) = key
	m.used++
	// The value slot was zeroed by wasm3HeapAlloc (or by the prior
	// grow's memmove from a zeroed region for new tail slots — the
	// new buffer is allocated fresh and zeroed on each grow).
	return wasm3MapValueAt(d, idx, typ.Elem.Size_)
}

//go:linkname runtime_mapdelete_faststr runtime.mapdelete_faststr
func runtime_mapdelete_faststr(typ *abi.MapType, m *Map, key string) {
	if m == nil || m.used == 0 {
		return
	}
	i := wasm3MapFind(m, key)
	if i < 0 {
		return
	}
	d := wasm3MapDataAt(m)
	last := uintptr(m.used) - 1
	if uintptr(i) != last {
		// Swap-remove: move the last entry into the deleted slot to
		// keep the array dense. Iteration order is undefined for Go
		// maps, so this is observationally indistinguishable from
		// any other removal strategy.
		d.keys[i] = d.keys[last]
		// Both src and dst are byte slices into the same WasmGC backing.
		iOff := uintptr(i) * typ.Elem.Size_
		lastOff := last * typ.Elem.Size_
		copy(d.values[iOff:iOff+typ.Elem.Size_], d.values[lastOff:lastOff+typ.Elem.Size_])
	}
	// Zero the (now unused) tail slot so a future assignment sees
	// the standard zero value when it lands there.
	d.keys[last] = ""
	lastOff := last * typ.Elem.Size_
	clear(d.values[lastOff : lastOff+typ.Elem.Size_])
	m.used--
}

// zeroVal and errNilAssign are shared with the !wasm3 implementation —
// runtime.go defines them unconditionally.
