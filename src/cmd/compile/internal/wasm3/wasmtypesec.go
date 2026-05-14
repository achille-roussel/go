// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm3

// wasmtypesec.go encodes a wasm GC type table into the bytes of a
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
	wasmOpRec       = 0x4E // rec group
	wasmOpSub       = 0x50 // sub type, non-final
	wasmOpStruct    = 0x5F // struct composite type
	wasmOpArray     = 0x5E // array composite type
	wasmOpRefNull   = 0x63 // (ref null ht)
	wasmOpRef       = 0x64 // (ref ht)
	wasmValI32      = 0x7F
	wasmValI64      = 0x7E
	wasmValF32      = 0x7D
	wasmValF64      = 0x7C
	wasmPackedI8    = 0x78
	wasmPackedI16   = 0x77
	wasmFieldConst  = 0x00
	wasmFieldVar    = 0x01
	wasmSectionType = 0x01 // section id
)

// appendUleb appends the unsigned LEB128 encoding of v to b.
func appendUleb(b []byte, v uint64) []byte {
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

// appendSleb appends the signed LEB128 encoding of v to b.
func appendSleb(b []byte, v int64) []byte {
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

// encodeTypeSection encodes the whole type table as the payload of a
// WebAssembly type section (the bytes after the section id and size).
//
// Types are renumbered into recursion-group emission order: the table's
// own indices are array positions, but a type's wasm index is its
// position in the flattened recGroups order. recGroups guarantees a
// group references only itself and earlier groups, so every reference
// resolves to an already-declared (or same-rec-group) type.
func (table wasmTable) encodeTypeSection() []byte {
	groups := table.recGroups()

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
		return appendSleb(b, int64(wasmIndex[tableIdx]))
	}

	storage := func(b []byte, s wasmStorage) []byte {
		if s.isRef() {
			if s.refNull {
				b = append(b, wasmOpRefNull)
			} else {
				b = append(b, wasmOpRef)
			}
			return heaptype(b, s.refType)
		}
		switch s.prim {
		case wasmI8:
			return append(b, wasmPackedI8)
		case wasmI16:
			return append(b, wasmPackedI16)
		case wasmI32:
			return append(b, wasmValI32)
		case wasmI64:
			return append(b, wasmValI64)
		case wasmF32:
			return append(b, wasmValF32)
		case wasmF64:
			return append(b, wasmValF64)
		}
		panic("wasm3: unknown primitive storage type")
	}

	fieldtype := func(b []byte, s wasmStorage, mut bool) []byte {
		b = storage(b, s)
		if mut {
			return append(b, wasmFieldVar)
		}
		return append(b, wasmFieldConst)
	}

	comptype := func(b []byte, t wasmType) []byte {
		switch t.kind {
		case wasmStructType:
			b = append(b, wasmOpStruct)
			b = appendUleb(b, uint64(len(t.fields)))
			for _, f := range t.fields {
				b = fieldtype(b, f.storage, f.mutable)
			}
		case wasmArrayType:
			b = append(b, wasmOpArray)
			b = fieldtype(b, t.elem, t.elemMut)
		default:
			panic("wasm3: unknown wasm type kind")
		}
		return b
	}

	subtype := func(b []byte, t wasmType) []byte {
		b = append(b, wasmOpSub)
		if t.super >= 0 {
			b = appendUleb(b, 1)
			b = appendUleb(b, uint64(wasmIndex[t.super]))
		} else {
			b = appendUleb(b, 0)
		}
		return comptype(b, t)
	}

	var payload []byte
	payload = appendUleb(payload, uint64(len(groups)))
	for _, group := range groups {
		payload = append(payload, wasmOpRec)
		payload = appendUleb(payload, uint64(len(group)))
		for _, t := range group {
			payload = subtype(payload, table[t])
		}
	}
	return payload
}
