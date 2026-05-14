// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasmgc

import (
	"bytes"
	"encoding/binary"
)

// serial.go serializes a wasmgc.Table so it can travel from the
// compiler to the linker. Each wasm3 package's compiler builds a Table
// of the GC and function types that package needs and emits it as an
// object-file aux symbol; the linker reads every package's Table, merges
// them into one (remapping the per-package indices), and emits the
// module's single type section. See doc/wasm3-m2-cutover-notes.md §2.
//
// The format is deliberately plain — little-endian fixed-width fields,
// no LEB128 — because it is internal object-file data, not the wasm
// binary itself; EncodeTypeSection handles the wasm encoding separately.

// Write appends the serialization of the table to w.
func (table Table) Write(w *bytes.Buffer) {
	var b [8]byte
	writeUint32 := func(x uint32) {
		binary.LittleEndian.PutUint32(b[:], x)
		w.Write(b[:4])
	}
	writeInt64 := func(x int64) {
		binary.LittleEndian.PutUint64(b[:], uint64(x))
		w.Write(b[:])
	}
	writeString := func(s string) {
		writeUint32(uint32(len(s)))
		w.WriteString(s)
	}
	writeStorage := func(s Storage) {
		w.WriteByte(byte(s.Prim))
		writeInt64(int64(s.RefType))
		if s.RefNull {
			w.WriteByte(1)
		} else {
			w.WriteByte(0)
		}
	}

	writeUint32(uint32(len(table)))
	for _, t := range table {
		writeString(t.Name)
		w.WriteByte(byte(t.Kind))
		writeInt64(int64(t.Super))

		writeUint32(uint32(len(t.Fields)))
		for _, f := range t.Fields {
			writeStorage(f.Storage)
			if f.Mutable {
				w.WriteByte(1)
			} else {
				w.WriteByte(0)
			}
		}

		writeStorage(t.Elem)
		if t.ElemMut {
			w.WriteByte(1)
		} else {
			w.WriteByte(0)
		}

		writeUint32(uint32(len(t.Params)))
		for _, p := range t.Params {
			writeStorage(p)
		}
		writeUint32(uint32(len(t.Results)))
		for _, r := range t.Results {
			writeStorage(r)
		}
	}
}

// ReadTable deserializes a table written by Table.Write.
func ReadTable(b []byte) Table {
	readByte := func() byte {
		x := b[0]
		b = b[1:]
		return x
	}
	readUint32 := func() uint32 {
		x := binary.LittleEndian.Uint32(b)
		b = b[4:]
		return x
	}
	readInt64 := func() int64 {
		x := binary.LittleEndian.Uint64(b)
		b = b[8:]
		return int64(x)
	}
	readString := func() string {
		n := readUint32()
		s := string(b[:n])
		b = b[n:]
		return s
	}
	readStorage := func() Storage {
		var s Storage
		s.Prim = Prim(readByte())
		s.RefType = int(readInt64())
		s.RefNull = readByte() != 0
		return s
	}

	table := make(Table, readUint32())
	for i := range table {
		t := &table[i]
		t.Name = readString()
		t.Kind = Kind(readByte())
		t.Super = int(readInt64())

		t.Fields = make([]Field, readUint32())
		for j := range t.Fields {
			t.Fields[j].Storage = readStorage()
			t.Fields[j].Mutable = readByte() != 0
		}
		if len(t.Fields) == 0 {
			t.Fields = nil
		}

		t.Elem = readStorage()
		t.ElemMut = readByte() != 0

		t.Params = make([]Storage, readUint32())
		for j := range t.Params {
			t.Params[j] = readStorage()
		}
		if len(t.Params) == 0 {
			t.Params = nil
		}
		t.Results = make([]Storage, readUint32())
		for j := range t.Results {
			t.Results[j] = readStorage()
		}
		if len(t.Results) == 0 {
			t.Results = nil
		}
	}
	return table
}
