// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasmgc

// typesec.go encodes a wasm GC type table into the bytes of a
// WebAssembly type section (section id 1). This is the data the linker
// splices into the module ahead of the function section; see
// doc/wasm3-m2-design.md §3.
//
// The encoding follows the WebAssembly 3.0 binary format:
//
//	typesec   := vec(rectype)
//	rectype   := 0x4E vec(subtype)
//	subtype   := 0x50 vec(typeidx) comptype          ; sub, non-final
//	comptype  := 0x5F vec(fieldtype)                 ; struct
//	           | 0x5E fieldtype                      ; array
//	fieldtype := storagetype mut
//	storagetype := valtype | 0x78 (i8) | 0x77 (i16)
//	reftype   := 0x63 heaptype  (ref null ht)
//	           | 0x64 heaptype  (ref ht)
//	heaptype  := s33 typeidx    (for a concrete type)
//
// Every recursion group is emitted as a 0x4E rec group, even a
// non-recursive singleton: a rec group of one is always valid, and it
// keeps the encoder uniform.

// Binary opcodes for the type section.
const (
	opRec       = 0x4E // rec group
	opSubFinal  = 0x4F // sub type, final (no subtype allowed)
	opSub       = 0x50 // sub type, non-final
	opStruct    = 0x5F // struct composite type
	opArray     = 0x5E // array composite type
	opFunc      = 0x60 // func composite type
	opRefNull   = 0x63 // (ref null ht)
	opRef       = 0x64 // (ref ht)
	valI32      = 0x7F
	valI64      = 0x7E
	valF32      = 0x7D
	valF64      = 0x7C
	packedI8    = 0x78
	packedI16   = 0x77
	fieldConst  = 0x00
	fieldVar    = 0x01
	SectionType = 0x01 // section id
)

// AppendUleb appends the unsigned LEB128 encoding of v to b.
func AppendUleb(b []byte, v uint64) []byte {
	for {
		c := byte(v & 0x7F)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		b = append(b, c)
		if v == 0 {
			return b
		}
	}
}

// AppendSleb appends the signed LEB128 encoding of v to b.
func AppendSleb(b []byte, v int64) []byte {
	for {
		c := byte(v & 0x7F)
		v >>= 7
		signBit := c & 0x40
		if (v == 0 && signBit == 0) || (v == -1 && signBit != 0) {
			b = append(b, c)
			return b
		}
		b = append(b, c|0x80)
	}
}

// EncodeTypeSection encodes the whole type table as the payload of a
// WebAssembly type section (the bytes after the section id and size).
//
// Types are renumbered into recursion-group emission order: the table's
// own indices are array positions, but a type's wasm index is its
// position in the flattened RecGroups order. RecGroups guarantees a
// group references only itself and earlier groups, so every reference
// resolves to an already-declared (or same-rec-group) type.
func (table Table) EncodeTypeSection() []byte {
	groups := table.RecGroups()

	// wasmIndex[tableIdx] is the type's index in the emitted section.
	wasmIndex := make([]int, len(table))
	pos := 0
	for _, group := range groups {
		for _, t := range group {
			wasmIndex[t] = pos
			pos++
		}
	}

	heaptype := func(b []byte, tableIdx int) []byte {
		return AppendSleb(b, int64(wasmIndex[tableIdx]))
	}

	storage := func(b []byte, s Storage) []byte {
		if s.IsRef() {
			if s.RefNull {
				b = append(b, opRefNull)
			} else {
				b = append(b, opRef)
			}
			return heaptype(b, s.RefType)
		}
		switch s.Prim {
		case I8:
			return append(b, packedI8)
		case I16:
			return append(b, packedI16)
		case I32:
			return append(b, valI32)
		case I64:
			return append(b, valI64)
		case F32:
			return append(b, valF32)
		case F64:
			return append(b, valF64)
		}
		panic("wasmgc: unknown primitive storage type")
	}

	fieldtype := func(b []byte, s Storage, mut bool) []byte {
		b = storage(b, s)
		if mut {
			return append(b, fieldVar)
		}
		return append(b, fieldConst)
	}

	comptype := func(b []byte, t Type) []byte {
		switch t.Kind {
		case KindStruct:
			b = append(b, opStruct)
			b = AppendUleb(b, uint64(len(t.Fields)))
			for _, f := range t.Fields {
				b = fieldtype(b, f.Storage, f.Mutable)
			}
		case KindArray:
			b = append(b, opArray)
			b = fieldtype(b, t.Elem, t.ElemMut)
		case KindFunc:
			b = append(b, opFunc)
			b = AppendUleb(b, uint64(len(t.Params)))
			for _, p := range t.Params {
				b = storage(b, p)
			}
			b = AppendUleb(b, uint64(len(t.Results)))
			for _, r := range t.Results {
				b = storage(b, r)
			}
		default:
			panic("wasmgc: unknown wasm type kind")
		}
		return b
	}

	subtype := func(b []byte, t Type) []byte {
		// Function types don't participate in subtyping in our model
		// — emit them as `sub final` so wasmtime accepts them as exact
		// matches against host import signatures (declared as plain
		// `func`). Struct/array types stay non-final so the
		// $go.object subtype hierarchy works.
		if t.Kind == KindFunc {
			b = append(b, opSubFinal)
		} else {
			b = append(b, opSub)
		}
		if t.Super >= 0 {
			b = AppendUleb(b, 1)
			b = AppendUleb(b, uint64(wasmIndex[t.Super]))
		} else {
			b = AppendUleb(b, 0)
		}
		return comptype(b, t)
	}

	var payload []byte
	payload = AppendUleb(payload, uint64(len(groups)))
	for _, group := range groups {
		payload = append(payload, opRec)
		payload = AppendUleb(payload, uint64(len(group)))
		for _, t := range group {
			payload = subtype(payload, table[t])
		}
	}
	return payload
}
