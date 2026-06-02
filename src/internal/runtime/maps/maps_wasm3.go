// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 stub for internal/runtime/maps.
//
// The rest of the package (map.go, group.go, table.go, runtime*.go,
// runtime_alg.go, runtime_hash32.go) is tagged !wasm3 because its
// swiss-table implementation walks linker-emitted trailing bytes via
// `unsafe.Pointer(uintptr(p) + n)` pointer arithmetic, which the
// wasm3 backend rejects (anyref has no scalar form). wasm3 maps live
// in the compiler-intrinsified per-T trampoline path
// (cmd/compile/internal/wasm3 + internal/runtime/maps/runtime_faststr_wasm3.go
// for string keys); the runtime-side mapaccess/mapassign/mapdelete
// functions below should never actually be called on wasm3, and the
// stubs panic if they are.
//
// AlgInit is a no-op: the AES hashkey it seeds isn't used by the
// wasm3 per-T map ops, which carry compiler-emitted hash functions.
// MemHash / MemHash32 / MemHash64 / StrHash / StrHashByValue still
// have to exist as symbols for runtime/alg.go's interface-equality
// hashing — the implementations are degraded (return the seed) for
// now; replace with a real per-byte hash when interface-hash-using
// code is exercised on wasm3.

package maps

import (
	"internal/abi"
	"unsafe"
)

// Map is the runtime representation of a Go map on wasm3.
// runtime_faststr_wasm3.go's flat-array string-keyed implementation
// stores its data behind dirPtr; the other map kinds (intrinsified
// at compile time) read/write fields directly via the wasm3 backend.
type Map struct {
	used   uint64
	dirPtr unsafe.Pointer // *wasm3MapData for string-keyed maps
}

func (m *Map) Used() uint64 { return m.used }

// Iter is a placeholder — wasm3 map iteration is compiler-
// intrinsified through the wasm3 per-T trampoline path and never
// constructs one of these.
type Iter struct{}

func (it *Iter) Init(typ *abi.MapType, m *Map) {
	panic("maps.Iter unreachable on wasm3 (per-T iteration is compiler-intrinsified)")
}

func (it *Iter) Next() {
	panic("maps.Iter unreachable on wasm3")
}

func (it *Iter) Key() unsafe.Pointer  { panic("maps.Iter unreachable on wasm3") }
func (it *Iter) Elem() unsafe.Pointer { panic("maps.Iter unreachable on wasm3") }
func (it *Iter) Map() *Map            { panic("maps.Iter unreachable on wasm3") }

// AlgInit is the runtime hash-table init the non-wasm3 path uses to
// seed the AES hashkey. wasm3 doesn't use that table.
func AlgInit() {}

const wasm3HashStub = "internal/runtime/maps: hash function reached on wasm3 — interface-equality / hash/maphash paths need a real wasm3 hash impl"

// MemHash / MemHash32 / MemHash64 / StrHash / StrHashByValue exist
// so runtime/alg.go's memhash / memhash32 / memhash64 / strhash
// wrappers link. On wasm3 there's no scalar-pointer form to read N
// bytes at, so a real byte-stream hash needs either a polyptr-aware
// compiler primitive or runtime/wasm linear-memory staging. Return
// the seed for now — correctness-breaking for hash-table use but
// safe for the link.
func MemHash(p unsafe.Pointer, seed, s uintptr) uintptr {
	_ = p
	_ = s
	return seed
}

func MemHash32(p unsafe.Pointer, seed uintptr) uintptr {
	_ = p
	return seed
}

func MemHash64(p unsafe.Pointer, seed uintptr) uintptr {
	_ = p
	return seed
}

func StrHash(p unsafe.Pointer, seed uintptr) uintptr {
	_ = p
	return seed
}

func StrHashByValue(s string, seed uintptr) uintptr {
	_ = s
	return seed
}

// NewMap / NewEmptyMap / clone are runtime/map.go entry points; on
// wasm3 the compiler intrinsifies make(map[K]V) and clone() so these
// shouldn't be called, but they need to exist for the link.
func NewMap(typ *abi.MapType, hint uintptr, m *Map, maxAlloc uintptr) *Map {
	panic("maps.NewMap unreachable on wasm3 (compiler intrinsifies make(map))")
}

func NewEmptyMap() *Map {
	panic("maps.NewEmptyMap unreachable on wasm3")
}

// errNilAssign is referenced by runtime/map.go via go:linkname; it's
// also used by runtime_faststr_wasm3.go's assign path on wasm3.
var errNilAssign = errorString("assignment to entry in nil map")

type errorString string

func (e errorString) RuntimeError() {}

func (e errorString) Error() string {
	return "runtime error: " + string(e)
}

// zeroVal is the zero-value buffer runtime_faststr_wasm3.go returns
// for a missing key. Sized for the maximum value the wasm3 stub map
// can hold (matches the !wasm3 maxZero constant in runtime.go).
const maxZero = 1024 // must match the !wasm3 maxZero in runtime.go

var zeroVal [maxZero]byte
