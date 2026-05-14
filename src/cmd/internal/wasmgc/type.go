// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package wasmgc defines the in-memory model of the WebAssembly 3.0
// garbage-collection types that GOARCH=wasm3 emits, and the algorithm
// that partitions them into recursion groups for the module's type
// section.
//
// The package is the shared, Go-type-agnostic half of milestone M2's
// object model (doc/wasm3-m2-design.md §3): Go heap objects are lowered
// to host-GC structs and arrays following the six rules of
// doc/wasm3-design.md §6. The compiler's wasm3 backend builds a Table
// from a program's *types.Type values; the linker merges per-package
// tables, runs RecGroups, and serializes the result with
// EncodeTypeSection. Keeping the model here lets both the compiler and
// the linker share one definition.
//
// RecGroups is the part the design flags as fiddly, so it is validated
// against fixtures in type_test.go.
package wasmgc

// Kind discriminates the WebAssembly GC type forms wasm3 emits.
type Kind uint8

const (
	KindStruct Kind = iota // (struct ...)
	KindArray              // (array ...)
)

// Prim is a primitive WebAssembly storage type. I8 and I16 are packed
// storage types, valid only as struct fields and array elements (e.g.
// the (array (mut i8)) backing of a string).
type Prim uint8

const (
	I8 Prim = iota
	I16
	I32
	I64
	F32
	F64
)

// Storage is the storage type of a struct field or array element. It is
// a primitive when RefType < 0, otherwise a reference to the wasm type
// at index RefType in the module type table.
type Storage struct {
	Prim    Prim // valid when RefType < 0
	RefType int  // index into the type table, or -1 for a primitive
	RefNull bool // (ref null $t) vs (ref $t); only meaningful when RefType >= 0
}

// PrimStorage builds a primitive storage type.
func PrimStorage(p Prim) Storage {
	return Storage{Prim: p, RefType: -1}
}

// RefStorage builds a reference storage type pointing at table index t.
// A nullable reference (ref null $t) lowers a Go pointer that can be
// nil; a non-nullable reference (ref $t) lowers a header field
// guaranteed to be populated, such as a string's backing array.
func RefStorage(t int, null bool) Storage {
	return Storage{RefType: t, RefNull: null}
}

// IsRef reports whether s is a reference rather than a primitive.
func (s Storage) IsRef() bool { return s.RefType >= 0 }

// Field is one field of a struct type.
type Field struct {
	Storage Storage
	Mutable bool
}

// Type is one entry in the module's type table.
type Type struct {
	Name string // human-readable label, e.g. "go.string"
	Kind Kind

	// Super is the index of the supertype, or -1 for a top type. Every
	// Go heap object subtypes go.object (doc/wasm3-design.md §6.1).
	Super int

	Fields  []Field // set when Kind == KindStruct
	Elem    Storage // set when Kind == KindArray
	ElemMut bool    // set when Kind == KindArray
}

// Fixed type-table indices for the prelude types. These are emitted by
// every wasm3 module ahead of the program's own types.
const (
	TypeGoObject = iota // go.object: the open base every heap object subtypes
	TypeGoBytes         // go.bytes:  (array (mut i8)), string/[]byte backing
	TypeGoString        // go.string: {backing, offset, length}
	NumPreludeTypes
)

// PreludeTypes returns the fixed type table that every wasm3 module
// starts with, following doc/wasm3-design.md §6.3. The indices match the
// TypeGo* constants above; the program's collected types are appended
// after these.
func PreludeTypes() []Type {
	t := make([]Type, NumPreludeTypes)

	// (type $go.object (sub (struct))) — the open base supertype.
	t[TypeGoObject] = Type{
		Name:  "go.object",
		Kind:  KindStruct,
		Super: -1,
	}

	// (type $go.bytes (array (mut i8))) — string and []byte backing.
	t[TypeGoBytes] = Type{
		Name:    "go.bytes",
		Kind:    KindArray,
		Super:   -1,
		Elem:    PrimStorage(I8),
		ElemMut: true,
	}

	// (type $go.string (sub $go.object (struct
	//   (field (ref $go.bytes)) (field i32) (field i32))))
	// The offset field is required because substrings share a backing.
	t[TypeGoString] = Type{
		Name:  "go.string",
		Kind:  KindStruct,
		Super: TypeGoObject,
		Fields: []Field{
			{Storage: RefStorage(TypeGoBytes, false)}, // backing
			{Storage: PrimStorage(I32)},               // offset
			{Storage: PrimStorage(I32)},               // length
		},
	}

	return t
}

// DependsOn returns the type-table indices that t directly depends on:
// its supertype and every reference-typed field or element. A type must
// be emitted in the same recursion group as, or a later group than,
// each of its dependencies.
func (t Type) DependsOn() []int {
	var deps []int
	if t.Super >= 0 {
		deps = append(deps, t.Super)
	}
	switch t.Kind {
	case KindStruct:
		for _, f := range t.Fields {
			if f.Storage.IsRef() {
				deps = append(deps, f.Storage.RefType)
			}
		}
	case KindArray:
		if t.Elem.IsRef() {
			deps = append(deps, t.Elem.RefType)
		}
	}
	return deps
}

// Table is a module's complete wasm type table: the prelude types
// followed by the program's collected types.
type Table []Type

// SelfRecursive reports whether the type at index i references itself,
// either through its supertype (never, in practice) or a field/element.
// A self-recursive type must be emitted inside a rec group even when it
// is the group's only member.
func (table Table) SelfRecursive(i int) bool {
	for _, d := range table[i].DependsOn() {
		if d == i {
			return true
		}
	}
	return false
}

// RecGroups partitions the type table into WebAssembly recursion groups.
// Two types share a group iff they are mutually recursive — in the same
// strongly-connected component of the "depends on" graph. The groups are
// returned in an order in which every group references only itself and
// earlier groups, which is the order the module's type section must emit
// them in. A group with a single, non-self-recursive member can be
// emitted as a plain type; every other group must be emitted as a rec
// group (see SelfRecursive).
//
// The implementation is Tarjan's strongly-connected-components algorithm,
// whose natural output order is already the required emit order: a
// component is finished, and appended, only after every component it
// depends on.
func (table Table) RecGroups() [][]int {
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

		for _, w := range table[v].DependsOn() {
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
