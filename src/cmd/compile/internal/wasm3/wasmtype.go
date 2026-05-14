// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm3

// wasmtype.go defines the in-memory model of the WebAssembly 3.0
// garbage-collection types that GOARCH=wasm3 emits, plus the algorithm
// that partitions them into recursion groups for the module's type
// section.
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
