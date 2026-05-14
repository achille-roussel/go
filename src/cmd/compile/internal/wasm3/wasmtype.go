// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm3

import "cmd/compile/internal/types"

// wasmtype.go defines the in-memory model of the WebAssembly 3.0
// garbage-collection types that GOARCH=wasm3 emits, the walk that
// collects them from a program's Go types, plus the algorithm that
// partitions them into recursion groups for the module's type section.
//
// This is the type-collection half of milestone M2's object model
// (doc/wasm3-m2-design.md §3): Go heap objects are lowered to host-GC
// structs and arrays following the six rules of doc/wasm3-design.md §6.
// recGroups is the part the design flags as fiddly, so it is validated
// against fixtures in wasmtype_test.go. The *types.Type walk that
// populates the table from a compiled program is wired in with the rest
// of the M2 cutover; this file only provides the representation, the
// fixed prelude, and the ordering algorithm.

// wasmTypeKind discriminates the WebAssembly GC type forms wasm3 emits.
type wasmTypeKind uint8

const (
	wasmStructType wasmTypeKind = iota // (struct ...)
	wasmArrayType                      // (array ...)
)

// wasmPrim is a primitive WebAssembly storage type. wasmI8 and wasmI16
// are packed storage types, valid only as struct fields and array
// elements (e.g. the (array (mut i8)) backing of a string).
type wasmPrim uint8

const (
	wasmI8 wasmPrim = iota
	wasmI16
	wasmI32
	wasmI64
	wasmF32
	wasmF64
)

// wasmStorage is the storage type of a struct field or array element.
// It is a primitive when refType < 0, otherwise a reference to the
// wasm type at index refType in the module type table.
type wasmStorage struct {
	prim    wasmPrim // valid when refType < 0
	refType int      // index into the type table, or -1 for a primitive
	refNull bool     // (ref null $t) vs (ref $t); only meaningful when refType >= 0
}

// prim builds a primitive storage type.
func prim(p wasmPrim) wasmStorage {
	return wasmStorage{prim: p, refType: -1}
}

// ref builds a reference storage type pointing at table index t. A
// nullable reference (ref null $t) lowers a Go pointer that can be nil;
// a non-nullable reference (ref $t) lowers a header field guaranteed to
// be populated, such as a string's backing array.
func ref(t int, null bool) wasmStorage {
	return wasmStorage{refType: t, refNull: null}
}

// isRef reports whether s is a reference rather than a primitive.
func (s wasmStorage) isRef() bool { return s.refType >= 0 }

// wasmField is one field of a struct type.
type wasmField struct {
	storage wasmStorage
	mutable bool
}

// wasmType is one entry in the module's type table.
type wasmType struct {
	name string // human-readable label, e.g. "go.string"
	kind wasmTypeKind

	// super is the index of the supertype, or -1 for a top type. Every
	// Go heap object subtypes go.object (doc/wasm3-design.md §6.1).
	super int

	fields  []wasmField // set when kind == wasmStructType
	elem    wasmStorage // set when kind == wasmArrayType
	elemMut bool        // set when kind == wasmArrayType
}

// Fixed type-table indices for the prelude types. These are emitted by
// every wasm3 module ahead of the program's own types.
const (
	typeGoObject = iota // go.object: the open base every heap object subtypes
	typeGoBytes         // go.bytes:  (array (mut i8)), string/[]byte backing
	typeGoString        // go.string: {backing, offset, length}
	numPreludeTypes
)

// preludeTypes returns the fixed type table that every wasm3 module
// starts with, following doc/wasm3-design.md §6.3. The indices match the
// typeGo* constants above; the program's collected types are appended
// after these.
func preludeTypes() []wasmType {
	t := make([]wasmType, numPreludeTypes)

	// (type $go.object (sub (struct))) — the open base supertype.
	t[typeGoObject] = wasmType{
		name:  "go.object",
		kind:  wasmStructType,
		super: -1,
	}

	// (type $go.bytes (array (mut i8))) — string and []byte backing.
	t[typeGoBytes] = wasmType{
		name:    "go.bytes",
		kind:    wasmArrayType,
		super:   -1,
		elem:    prim(wasmI8),
		elemMut: true,
	}

	// (type $go.string (sub $go.object (struct
	//   (field (ref $go.bytes)) (field i32) (field i32))))
	// The offset field is required because substrings share a backing.
	t[typeGoString] = wasmType{
		name:  "go.string",
		kind:  wasmStructType,
		super: typeGoObject,
		fields: []wasmField{
			{storage: ref(typeGoBytes, false)}, // backing
			{storage: prim(wasmI32)},           // offset
			{storage: prim(wasmI32)},           // length
		},
	}

	return t
}

// dependsOn returns the type-table indices that t directly depends on:
// its supertype and every reference-typed field or element. A type must
// be emitted in the same recursion group as, or a later group than,
// each of its dependencies.
func (t wasmType) dependsOn() []int {
	var deps []int
	if t.super >= 0 {
		deps = append(deps, t.super)
	}
	switch t.kind {
	case wasmStructType:
		for _, f := range t.fields {
			if f.storage.isRef() {
				deps = append(deps, f.storage.refType)
			}
		}
	case wasmArrayType:
		if t.elem.isRef() {
			deps = append(deps, t.elem.refType)
		}
	}
	return deps
}

// selfRecursive reports whether t references itself, either through its
// supertype (never, in practice) or a field/element. A self-recursive
// type must be emitted inside a rec group even when it is the group's
// only member.
func (table wasmTable) selfRecursive(i int) bool {
	for _, d := range table[i].dependsOn() {
		if d == i {
			return true
		}
	}
	return false
}

// wasmTable is a module's complete wasm type table: the prelude types
// followed by the program's collected types.
type wasmTable []wasmType

// recGroups partitions the type table into WebAssembly recursion groups.
// Two types share a group iff they are mutually recursive — in the same
// strongly-connected component of the "depends on" graph. The groups are
// returned in an order in which every group references only itself and
// earlier groups, which is the order the module's type section must emit
// them in. A group with a single, non-self-recursive member can be
// emitted as a plain type; every other group must be emitted as a rec
// group (see selfRecursive).
//
// The implementation is Tarjan's strongly-connected-components algorithm,
// whose natural output order is already the required emit order: a
// component is finished, and appended, only after every component it
// depends on.
func (table wasmTable) recGroups() [][]int {
	const unvisited = -1
	n := len(table)

	index := make([]int, n)
	lowlink := make([]int, n)
	onStack := make([]bool, n)
	for i := range index {
		index[i] = unvisited
	}

	var stack []int
	var groups [][]int
	next := 0

	var strongConnect func(v int)
	strongConnect = func(v int) {
		index[v] = next
		lowlink[v] = next
		next++
		stack = append(stack, v)
		onStack[v] = true

		for _, w := range table[v].dependsOn() {
			switch {
			case index[w] == unvisited:
				strongConnect(w)
				lowlink[v] = min(lowlink[v], lowlink[w])
			case onStack[w]:
				lowlink[v] = min(lowlink[v], index[w])
			}
		}

		if lowlink[v] == index[v] {
			var group []int
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				group = append(group, w)
				if w == v {
					break
				}
			}
			groups = append(groups, group)
		}
	}

	for v := 0; v < n; v++ {
		if index[v] == unvisited {
			strongConnect(v)
		}
	}
	return groups
}

// typeCollector accumulates the wasm GC types a program needs. It walks
// Go *types.Type values and, following the six lowering rules of
// doc/wasm3-design.md §6, assigns each type that requires a named wasm
// type declaration (heap structs, slice backing arrays, boxed scalars)
// an index in the table. The table always starts with the fixed prelude
// (typeGoObject, typeGoBytes, typeGoString).
//
// Memoization is keyed on the *types.Type pointer; the compiler
// canonicalizes types, so identical Go types share an entry. Struct
// entries are reserved before their fields are walked, so a struct that
// refers to itself (directly or through a cycle) resolves to the
// in-progress index rather than recursing forever — recGroups later
// places such cycles in a single rec group.
type typeCollector struct {
	table   wasmTable
	structs map[*types.Type]int // Go struct type -> table index
	backing map[*types.Type]int // slice/array element type -> backing array index
	boxed   map[wasmPrim]int    // primitive -> boxed-scalar struct index
}

func newTypeCollector() *typeCollector {
	return &typeCollector{
		table:   preludeTypes(),
		structs: make(map[*types.Type]int),
		backing: make(map[*types.Type]int),
		boxed:   make(map[wasmPrim]int),
	}
}

// scalarPrim returns the primitive wasm storage type for a Go scalar
// kind, and whether the kind is in fact a scalar. Integers narrower than
// 64 bits use i32 storage (rule 1); 64-bit integers, int, uint and
// uintptr use i64. Packed i8/i16 storage is reserved for array elements
// such as go.bytes and is not produced here.
func scalarPrim(k types.Kind) (wasmPrim, bool) {
	switch k {
	case types.TBOOL,
		types.TINT8, types.TUINT8,
		types.TINT16, types.TUINT16,
		types.TINT32, types.TUINT32:
		return wasmI32, true
	case types.TINT64, types.TUINT64,
		types.TINT, types.TUINT, types.TUINTPTR:
		return wasmI64, true
	case types.TFLOAT32:
		return wasmF32, true
	case types.TFLOAT64:
		return wasmF64, true
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
func (c *typeCollector) lowerFields(t *types.Type) []wasmField {
	if p, ok := scalarPrim(t.Kind()); ok {
		return []wasmField{{storage: prim(p), mutable: true}}
	}

	switch t.Kind() {
	case types.TCOMPLEX64:
		return []wasmField{
			{storage: prim(wasmF32), mutable: true},
			{storage: prim(wasmF32), mutable: true},
		}
	case types.TCOMPLEX128:
		return []wasmField{
			{storage: prim(wasmF64), mutable: true},
			{storage: prim(wasmF64), mutable: true},
		}

	case types.TPTR:
		return []wasmField{{storage: c.pointerStorage(t.Elem()), mutable: true}}

	case types.TUNSAFEPTR:
		// unsafe.Pointer has no statically known pointee; it lowers to a
		// reference to the open base type. Pointer arithmetic through it
		// is part of the restricted subset that does not survive (see
		// doc/wasm3-design.md §2 goals).
		return []wasmField{{storage: ref(typeGoObject, true), mutable: true}}

	case types.TSTRING:
		// {backing, offset, length} flattened in place (rule 3). The
		// offset is required because substrings share a backing array.
		return []wasmField{
			{storage: ref(typeGoBytes, false), mutable: true},
			{storage: prim(wasmI32), mutable: true},
			{storage: prim(wasmI32), mutable: true},
		}

	case types.TSLICE:
		// {backing, offset, length, capacity} flattened in place.
		return []wasmField{
			{storage: ref(c.collectBacking(t.Elem()), true), mutable: true},
			{storage: prim(wasmI32), mutable: true},
			{storage: prim(wasmI32), mutable: true},
			{storage: prim(wasmI32), mutable: true},
		}

	case types.TSTRUCT:
		var fields []wasmField
		for _, f := range t.Fields() {
			fields = append(fields, c.lowerFields(f.Type)...)
		}
		return fields

	case types.TARRAY:
		// A fixed-length array value flattens element-by-element (rule
		// 3). Large arrays are expected to be addressed through a slice;
		// the value form is kept simple here.
		var fields []wasmField
		elem := c.lowerFields(t.Elem())
		for i := int64(0); i < t.NumElem(); i++ {
			fields = append(fields, elem...)
		}
		return fields

	case types.TMAP, types.TCHAN, types.TFUNC:
		// Pointer-shaped runtime types. Their concrete representation is
		// refined in later milestones (M3/M4); for now they occupy a
		// single reference to the open base type.
		return []wasmField{{storage: ref(typeGoObject, true), mutable: true}}

	case types.TINTER:
		// {type descriptor, data} — doc/wasm3-design.md §6.3. The
		// descriptor type go.type is not yet modelled (interfaces are
		// milestone M3), so both words are open-base references for now.
		return []wasmField{
			{storage: ref(typeGoObject, true), mutable: true},
			{storage: ref(typeGoObject, true), mutable: true},
		}
	}

	panic("wasm3: cannot lower Go type to wasm fields: " + t.Kind().String())
}

// pointerStorage returns the reference storage for a Go pointer *elem.
// A pointer to a struct references that struct's type directly; a
// pointer to a scalar references a boxed-scalar wrapper, since a wasm
// reference must point at a heap object, not a primitive. Pointers to
// other composites box through a single-field wrapper as well.
func (c *typeCollector) pointerStorage(elem *types.Type) wasmStorage {
	if elem.IsStruct() {
		return ref(c.collectStruct(elem), true)
	}
	if p, ok := scalarPrim(elem.Kind()); ok {
		return ref(c.collectBoxedScalar(p), true)
	}
	// Pointer to a composite value (array, string, slice, ...). Box the
	// pointee into its own go.object subtype so the pointer is a single
	// reference (doc/wasm3-design.md §7, selective boxing).
	return ref(c.collectBox(elem), true)
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
	c.table = append(c.table, wasmType{
		name:  typeName(t),
		kind:  wasmStructType,
		super: typeGoObject,
	})
	c.structs[t] = idx

	var fields []wasmField
	for _, f := range t.Fields() {
		fields = append(fields, c.lowerFields(f.Type)...)
	}
	c.table[idx].fields = fields
	return idx
}

// collectBacking reserves and returns the table index of the array type
// backing a []elem slice. A scalar or pointer element stores inline; a
// composite element is stored as an array of boxed references, since a
// wasm array element is a single storage slot (rule 5).
func (c *typeCollector) collectBacking(elem *types.Type) int {
	if idx, ok := c.backing[elem]; ok {
		return idx
	}
	idx := len(c.table)
	c.backing[elem] = idx

	var st wasmStorage
	if p, ok := scalarPrim(elem.Kind()); ok {
		st = prim(p)
	} else if elem.Kind() == types.TPTR {
		st = c.pointerStorage(elem.Elem())
	} else {
		// Composite element: an array of boxed elements (rule 5). This
		// loses contiguous value layout — one allocation per element —
		// which doc/wasm3-design.md §6.3 accepts as the M2 default.
		st = ref(c.collectBox(elem), true)
	}
	c.table = append(c.table, wasmType{
		name:    "go.array." + typeName(elem),
		kind:    wasmArrayType,
		super:   -1,
		elem:    st,
		elemMut: true,
	})
	return idx
}

// collectBoxedScalar reserves and returns the table index of the
// single-field struct that boxes a primitive so a pointer can reference
// it. doc/wasm3-design.md §7 calls this selective boxing.
func (c *typeCollector) collectBoxedScalar(p wasmPrim) int {
	if idx, ok := c.boxed[p]; ok {
		return idx
	}
	idx := len(c.table)
	c.boxed[p] = idx
	c.table = append(c.table, wasmType{
		name:   "go.box.scalar",
		kind:   wasmStructType,
		super:  typeGoObject,
		fields: []wasmField{{storage: prim(p), mutable: true}},
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
	c.table = append(c.table, wasmType{
		name:  "go.box." + typeName(t),
		kind:  wasmStructType,
		super: typeGoObject,
	})
	c.structs[t] = idx
	c.table[idx].fields = c.lowerFields(t)
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
