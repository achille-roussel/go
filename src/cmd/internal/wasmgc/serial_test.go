// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasmgc

import (
	"bytes"
	"reflect"
	"testing"
)

func TestTableRoundTrip(t *testing.T) {
	// A table exercising every Type kind and both storage forms: the
	// prelude, a recursive struct, an array of references, a boxed
	// scalar, and two function types.
	st := NumPreludeTypes
	arr := NumPreludeTypes + 1
	box := NumPreludeTypes + 2
	table := Table(append(PreludeTypes(),
		Type{
			Name:  "go.Point",
			Kind:  KindStruct,
			Super: TypeGoObject,
			Fields: []Field{
				{Storage: PrimStorage(F64), Mutable: true},
				{Storage: RefStorage(st, true), Mutable: true}, // self-reference
			},
		},
		Type{
			Name:    "go.array.PtrPoint",
			Kind:    KindArray,
			Super:   -1,
			Elem:    RefStorage(st, true),
			ElemMut: true,
		},
		Type{
			Name:   "go.box.scalar",
			Kind:   KindStruct,
			Super:  TypeGoObject,
			Fields: []Field{{Storage: PrimStorage(I64), Mutable: true}},
		},
		Type{
			Name:    "go.func.f",
			Kind:    KindFunc,
			Super:   -1,
			Params:  []Storage{RefStorage(st, true), PrimStorage(I32)},
			Results: []Storage{RefStorage(arr, true)},
		},
		Type{
			Name:  "go.func.main",
			Kind:  KindFunc,
			Super: -1,
		},
	))
	_ = box

	var buf bytes.Buffer
	table.Write(&buf)
	got := ReadTable(buf.Bytes())

	if !reflect.DeepEqual(got, table) {
		t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", got, table)
	}

	// The deserialized table must still encode and validate.
	ValidateModule(t, "roundtrip", WrapModule(got.EncodeTypeSection()))
}

func TestEmptyTableRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	Table(nil).Write(&buf)
	if got := ReadTable(buf.Bytes()); len(got) != 0 {
		t.Fatalf("empty table round-tripped to %d entries", len(got))
	}
}
