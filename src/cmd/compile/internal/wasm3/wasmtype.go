// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm3

import (
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/wasmgc"
)

// wasmtype.go is the *types.Type half of milestone M2's object model: it
// walks a program's Go types and, following the six lowering rules of
// doc/wasm3-design.md §6, collects the WebAssembly 3.0 GC types the
// module must declare. The GC type model itself — wasmgc.Type, the fixed
// prelude, recursion-group ordering, and the type-section encoder —
// lives in cmd/internal/wasmgc so the linker can share it; this file
// only builds a wasmgc.Table from compiled Go types.

// typeCollector accumulates the wasm GC types a program needs. It walks
// Go *types.Type values and, following the six lowering rules of
// doc/wasm3-design.md §6, assigns each type that requires a named wasm
// type declaration (heap structs, slice backing arrays, boxed scalars)
// an index in the table. The table always starts with the fixed prelude
// (wasmgc.TypeGoObject, TypeGoBytes, TypeGoString).
//
// Memoization is keyed on the *types.Type pointer; the compiler
// canonicalizes types, so identical Go types share an entry. Struct
// entries are reserved before their fields are walked, so a struct that
// refers to itself (directly or through a cycle) resolves to the
// in-progress index rather than recursing forever — wasmgc.RecGroups
// later places such cycles in a single rec group.
type typeCollector struct {
	table          wasmgc.Table
	structs        map[*types.Type]int // Go struct type -> table index
	backing        map[*types.Type]int // slice/array element type -> backing array index
	slices         map[*types.Type]int // Go slice type -> boxed slice-header struct index (doc/wasm3-slice-boxing.md)
	boxed          map[wasmgc.Prim]int // primitive -> boxed-scalar struct index
	funcs          map[*types.Type]int // Go func type -> func-type table index
	closureCtxs    map[*types.Type]int // Go func type -> per-signature closure-struct table index
	perClosureCtxs map[*obj.LSym]int   // closure body LSym -> per-closure closure-struct subtype index (doc/wasm3-m3-captures-in-struct.md)
}

func newTypeCollector() *typeCollector {
	return &typeCollector{
		table:          wasmgc.PreludeTypes(),
		structs:        make(map[*types.Type]int),
		backing:        make(map[*types.Type]int),
		slices:         make(map[*types.Type]int),
		boxed:          make(map[wasmgc.Prim]int),
		funcs:          make(map[*types.Type]int),
		closureCtxs:    make(map[*types.Type]int),
		perClosureCtxs: make(map[*obj.LSym]int),
	}
}

// packedArrayPrim returns the packed wasm storage primitive an array
// backing should use for sub-i32 element kinds. wasmgc allows i8 and
// i16 packed storage inside arrays (and structs), exposed through
// array.get_u / array.get_s on read. We pick it for byte / int16
// arrays so a Go `[N]byte` lays out as a byte-tight (array (mut i8))
// rather than wasting 3 bytes per element on i32 storage.
func packedArrayPrim(k types.Kind) (wasmgc.Prim, bool) {
	switch k {
	case types.TBOOL, types.TINT8, types.TUINT8:
		return wasmgc.I8, true
	case types.TINT16, types.TUINT16:
		return wasmgc.I16, true
	}
	return 0, false
}

// scalarPrim returns the primitive wasm storage type for a Go scalar
// kind, and whether the kind is in fact a scalar. Integers narrower than
// 64 bits use i32 storage (rule 1); 64-bit integers, int, uint and
// uintptr use i64. Packed i8/i16 storage is reserved for array elements
// such as go.bytes and is not produced here.
func scalarPrim(k types.Kind) (wasmgc.Prim, bool) {
	switch k {
	case types.TBOOL,
		types.TINT8, types.TUINT8,
		types.TINT16, types.TUINT16,
		types.TINT32, types.TUINT32:
		return wasmgc.I32, true
	case types.TINT64, types.TUINT64,
		types.TINT, types.TUINT, types.TUINTPTR:
		return wasmgc.I64, true
	case types.TFLOAT32:
		return wasmgc.F32, true
	case types.TFLOAT64:
		return wasmgc.F64, true
	}
	return 0, false
}

// lowerFields explodes a Go type into the wasm struct fields it occupies
// inside an enclosing struct. Scalars take one primitive field; pointers
// take one reference field; composite values (string, slice, nested
// struct, array) are flattened in place, preserving Go value semantics
// without extra allocation (rules 3 and 6). Every field produced is
// mutable: a flattened field can be assigned through its enclosing
// struct.
func (c *typeCollector) lowerFields(t *types.Type) []wasmgc.Field {
	if p, ok := scalarPrim(t.Kind()); ok {
		return []wasmgc.Field{{Storage: wasmgc.PrimStorage(p), Mutable: true}}
	}

	switch t.Kind() {
	case types.TCOMPLEX64:
		return []wasmgc.Field{
			{Storage: wasmgc.PrimStorage(wasmgc.F32), Mutable: true},
			{Storage: wasmgc.PrimStorage(wasmgc.F32), Mutable: true},
		}
	case types.TCOMPLEX128:
		return []wasmgc.Field{
			{Storage: wasmgc.PrimStorage(wasmgc.F64), Mutable: true},
			{Storage: wasmgc.PrimStorage(wasmgc.F64), Mutable: true},
		}

	case types.TPTR:
		return []wasmgc.Field{{Storage: c.pointerStorage(t.Elem()), Mutable: true}}

	case types.TUNSAFEPTR:
		// unsafe.Pointer has no statically known pointee; it lowers to a
		// reference to the open base type. Pointer arithmetic through it
		// is part of the restricted subset that does not survive (see
		// doc/wasm3-design.md §2 goals).
		return []wasmgc.Field{{Storage: wasmgc.RefStorage(wasmgc.TypeGoObject, true), Mutable: true}}

	case types.TSTRING:
		// WasmGC memory model (doc/wasm3-slice-boxing.md): a string is a
		// single boxed $go.string ref ({backing, offset, length}), so a
		// string field is one ref, not three flattened fields. This 1:1
		// Go-field -> WasmGC-field mapping is what the field-access ABI
		// relies on (an offset into a flattened string's len word can't be
		// resolved). nullable so a zero-value string field is a null ref.
		return []wasmgc.Field{{Storage: wasmgc.RefStorage(wasmgc.TypeGoString, true), Mutable: true}}

	case types.TSLICE:
		// WasmGC memory model (doc/wasm3-slice-boxing.md): a slice is a
		// single boxed $go.slice.<T> ref, so a slice field is one ref
		// (nullable — a nil slice is a null ref), not four flattened
		// fields. This 1:1 Go-field -> WasmGC-field mapping is what the
		// field-access ABI relies on. (Was: {backing, off, len, cap}.)
		return []wasmgc.Field{{Storage: wasmgc.RefStorage(c.collectSliceStruct(t), true), Mutable: true}}

	case types.TSTRUCT:
		var fields []wasmgc.Field
		for _, f := range t.Fields() {
			fields = append(fields, c.lowerFields(f.Type)...)
		}
		return fields

	case types.TARRAY:
		// Stage D: every Go array — small or large — lowers to a
		// single `(ref (array T_elem))` field. The wasm engine's
		// 10 000-field cap on struct types rules out a threshold-
		// based "unroll if small, ref if large" lowering (see
		// doc/wasm3-m3-notes.md "Blocker for the wasip1 test
		// harness"), and a single representation keeps the SSA
		// backend and runtime helpers from having to handle two
		// shapes. Element accesses go through array.get / array.set
		// on the ref once Stage C's lowering rules emit those ops;
		// until then, struct fields containing arrays are typed
		// correctly but accessed through the i64-pointer path the
		// SSA backend still emits, which will mismatch and force
		// the function to fall back to the encodeWasm3Body stub.
		return []wasmgc.Field{{Storage: wasmgc.RefStorage(c.collectBacking(t.Elem()), true), Mutable: true}}

	case types.TMAP, types.TCHAN, types.TFUNC:
		// Pointer-shaped runtime types. Their concrete representation is
		// refined in later milestones (M3/M4); for now they occupy a
		// single reference to the open base type.
		return []wasmgc.Field{{Storage: wasmgc.RefStorage(wasmgc.TypeGoObject, true), Mutable: true}}

	case types.TINTER:
		// WasmGC memory model (doc/wasm3-slice-boxing.md): an interface is
		// a single boxed $go.iface ref ({itab, data}), so an interface
		// field is one ref, not two flattened words. nullable: a nil
		// interface is a null ref.
		return []wasmgc.Field{{Storage: wasmgc.RefStorage(wasmgc.TypeGoIface, true), Mutable: true}}
	}

	panic("wasm3: cannot lower Go type to wasm fields: " + t.Kind().String())
}

// pointerStorage returns the reference storage for a Go pointer *elem.
// A pointer to a struct references that struct's type directly; a
// pointer to a scalar references a boxed-scalar wrapper, since a wasm
// reference must point at a heap object, not a primitive. Pointers to
// other composites box through a single-field wrapper as well.
func (c *typeCollector) pointerStorage(elem *types.Type) wasmgc.Storage {
	if elem.IsStruct() {
		return wasmgc.RefStorage(c.collectStruct(elem), true)
	}
	if p, ok := scalarPrim(elem.Kind()); ok {
		return wasmgc.RefStorage(c.collectBoxedScalar(p), true)
	}
	// Pointer to a composite value (array, string, slice, ...). Box the
	// pointee into its own go.object subtype so the pointer is a single
	// reference (doc/wasm3-design.md §7, selective boxing).
	return wasmgc.RefStorage(c.collectBox(elem), true)
}

// collectStruct reserves and returns the table index of the wasm struct
// type for a Go struct. The index is recorded before the fields are
// walked so recursive structs terminate.
func (c *typeCollector) collectStruct(t *types.Type) int {
	if !t.IsStruct() {
		panic("wasm3: collectStruct on non-struct type " + t.Kind().String())
	}
	if idx, ok := c.structs[t]; ok {
		return idx
	}
	idx := len(c.table)
	c.table = append(c.table, wasmgc.Type{
		Name:  typeName(t),
		Kind:  wasmgc.KindStruct,
		Super: wasmgc.TypeGoObject,
	})
	c.structs[t] = idx

	var fields []wasmgc.Field
	for _, f := range t.Fields() {
		fields = append(fields, c.lowerFields(f.Type)...)
	}
	c.table[idx].Fields = fields
	return idx
}

// collectBacking reserves and returns the table index of the array type
// backing a []elem slice. A scalar or pointer element stores inline; a
// composite element is stored as an array of boxed references, since a
// wasm array element is a single storage slot (rule 5). Sub-i32
// integer / bool element kinds use packed i8 / i16 storage so the
// backing array is byte-/halfword-tight — wasmgc allows packed
// element storage on arrays even though field storage rounds up.
func (c *typeCollector) collectBacking(elem *types.Type) int {
	if idx, ok := c.backing[elem]; ok {
		return idx
	}
	idx := len(c.table)
	c.backing[elem] = idx

	var st wasmgc.Storage
	if p, ok := packedArrayPrim(elem.Kind()); ok {
		st = wasmgc.PrimStorage(p)
	} else if p, ok := scalarPrim(elem.Kind()); ok {
		st = wasmgc.PrimStorage(p)
	} else if elem.Kind() == types.TPTR {
		st = c.pointerStorage(elem.Elem())
	} else if k := wasm3FlatStride(elem); k > 0 {
		// Multi-i64 composite elements (string=2, interface=2,
		// slice=3) lay out flat in an (array i64): the body's
		// SSA-level access pattern for these is
		// `(I64Load [off] (I64Add slice.array (I64Shl idx const)))`
		// — linear-memory shape — which lowers cleanly to an
		// `array.get_u` on (array i64) once the slice-of-stride
		// rewrite rules fire. The wasmgc backing has K * len
		// slots per slice, where K is the per-Go-elem field count
		// (string = 2, interface = 2, slice = 3). The contiguous
		// layout matches what Go code expects when it computes
		// element addresses by hand (`&s[0]`-style), and avoids
		// the per-element heap allocation the box path would do.
		// See wasm3-m3-stage-f-interfaces.md for slice-of-composite
		// notes.
		st = wasmgc.PrimStorage(wasmgc.I64)
	} else {
		// Composite element: an array of boxed elements (rule 5). This
		// loses contiguous value layout — one allocation per element —
		// which doc/wasm3-design.md §6.3 accepts as the M2 default.
		st = wasmgc.RefStorage(c.collectBox(elem), true)
	}
	c.table = append(c.table, wasmgc.Type{
		Name:    "go.array." + typeName(elem),
		Kind:    wasmgc.KindArray,
		Super:   -1,
		Elem:    st,
		ElemMut: true,
	})
	return idx
}

// collectSliceStruct reserves and returns the table index of the
// boxed slice-header struct for a Go slice type t (doc/wasm3-slice-
// boxing.md). The header is
//
//	(type go.slice.<T> (struct
//	    (field (mut (ref go.array.<T>)))  ;; data backing
//	    (field (mut i64))                 ;; off
//	    (field (mut i64))                 ;; len
//	    (field (mut i64))))               ;; cap
//
// off/len/cap are i64 to match the wasm3 per-value-local width for Go
// ints (no i32<->i64 conversion on struct.get/set; one i32 wrap only
// at array indexing). The backing array type is collected first so the
// struct's data field can reference it by index. cap is a stored field
// (Go-faithful 3-index slices); fields are mutable so the backend may
// rewrite a header in place. nil slices are a null slice ref, so
// the data field is non-nullable (a non-nil slice always has a real,
// possibly zero-length, backing array — mirroring go.string).
func (c *typeCollector) collectSliceStruct(t *types.Type) int {
	if !t.IsSlice() {
		panic("wasm3: collectSliceStruct on non-slice type " + t.Kind().String())
	}
	if idx, ok := c.slices[t]; ok {
		return idx
	}
	arrIdx := c.collectBacking(t.Elem())
	idx := len(c.table)
	c.slices[t] = idx
	c.table = append(c.table, wasmgc.Type{
		Name:  "go.slice." + typeName(t.Elem()),
		Kind:  wasmgc.KindStruct,
		Super: wasmgc.TypeGoObject,
		Fields: []wasmgc.Field{
			{Storage: wasmgc.RefStorage(arrIdx, false), Mutable: true}, // data
			{Storage: wasmgc.PrimStorage(wasmgc.I64), Mutable: true},   // off
			{Storage: wasmgc.PrimStorage(wasmgc.I64), Mutable: true},   // len
			{Storage: wasmgc.PrimStorage(wasmgc.I64), Mutable: true},   // cap
		},
	})
	return idx
}

// wasm3FlatStride reports the number of i64 slots per Go element
// when elem is laid out flat in an (array i64) wasmgc backing.
// Returns 0 if elem is not eligible for flat layout (scalars and
// pointers go through their own primitive/ref storage; arrays of
// arrays go through the box path).
//
// The flat representation is keyed to the SSA-level access pattern
// the wasm3 compiler emits today for slices of composite elements:
// each Go element is read/written as a fixed number of i64 fields
// at multiples of 8 bytes within a 16-byte (string, interface) or
// 24-byte (slice) stride. The wasmgc backing's (array i64) lets
// `array.get_u $arr_T idx` consume the same indexing the body would
// otherwise have lowered to `i64.load [off] (anyref + shifted)`.
func wasm3FlatStride(t *types.Type) int {
	switch t.Kind() {
	case types.TSTRING:
		return 2 // (data ptr, len)
	case types.TINTER:
		return 2 // (type ptr, data ptr)
	case types.TSLICE:
		return 3 // (data ptr, len, cap)
	}
	return 0
}

// collectBoxedScalar reserves and returns the table index of the
// single-field struct that boxes a primitive so a pointer can reference
// it. doc/wasm3-design.md §7 calls this selective boxing.
func (c *typeCollector) collectBoxedScalar(p wasmgc.Prim) int {
	if idx, ok := c.boxed[p]; ok {
		return idx
	}
	idx := len(c.table)
	c.boxed[p] = idx
	c.table = append(c.table, wasmgc.Type{
		Name:   "go.box.scalar",
		Kind:   wasmgc.KindStruct,
		Super:  wasmgc.TypeGoObject,
		Fields: []wasmgc.Field{{Storage: wasmgc.PrimStorage(p), Mutable: true}},
	})
	return idx
}

// collectBox reserves and returns the table index of a struct that boxes
// a composite value so a pointer can reference it as a single reference.
// The boxed value is flattened into the wrapper's fields.
func (c *typeCollector) collectBox(t *types.Type) int {
	if idx, ok := c.structs[t]; ok {
		return idx
	}
	idx := len(c.table)
	c.table = append(c.table, wasmgc.Type{
		Name:  "go.box." + typeName(t),
		Kind:  wasmgc.KindStruct,
		Super: wasmgc.TypeGoObject,
	})
	c.structs[t] = idx
	c.table[idx].Fields = c.lowerFields(t)
	return idx
}

// typeName returns a human-readable label for a Go type, used only for
// debugging and the wasm name section. It is not required to be unique.
func typeName(t *types.Type) string {
	if sym := t.Sym(); sym != nil {
		return sym.Name
	}
	return t.Kind().String()
}
