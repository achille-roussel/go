// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasmgc

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// WrapModule wraps a type-section payload in an otherwise empty but
// valid WebAssembly module: the magic, the version, and a single type
// section carrying the payload. It is exported so the compiler's wasm3
// collector tests can validate their tables too.
func WrapModule(typeSectionPayload []byte) []byte {
	mod := []byte{
		0x00, 0x61, 0x73, 0x6d, // \0asm
		0x01, 0x00, 0x00, 0x00, // version 1
	}
	mod = append(mod, SectionType)
	mod = AppendUleb(mod, uint64(len(typeSectionPayload)))
	mod = append(mod, typeSectionPayload...)
	return mod
}

func TestAppendLeb128(t *testing.T) {
	// A few values against known minimal LEB128 encodings.
	uTests := []struct {
		v    uint64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{63, []byte{0x3F}},
		{64, []byte{0x40}},
		{127, []byte{0x7F}},
		{128, []byte{0x80, 0x01}},
		{300, []byte{0xAC, 0x02}},
	}
	for _, tc := range uTests {
		if got := AppendUleb(nil, tc.v); !bytes.Equal(got, tc.want) {
			t.Errorf("AppendUleb(%d) = % x, want % x", tc.v, got, tc.want)
		}
	}
	sTests := []struct {
		v    int64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{63, []byte{0x3F}},
		{64, []byte{0xC0, 0x00}},
		{-1, []byte{0x7F}},
		{-64, []byte{0x40}},
	}
	for _, tc := range sTests {
		if got := AppendSleb(nil, tc.v); !bytes.Equal(got, tc.want) {
			t.Errorf("AppendSleb(%d) = % x, want % x", tc.v, got, tc.want)
		}
	}
}

func TestEncodePreludeTypeSection(t *testing.T) {
	payload := Table(PreludeTypes()).EncodeTypeSection()

	// Prelude: 10 singleton rec groups in table order: object, bytes,
	// string, then the seven go.iptr.<class> fat-pointer wrappers
	// (i8, i16, i32, i64, f32, f64, ref). Pin the exact bytes — this is
	// the module preamble every wasm3 binary starts with.
	want := []byte{
		0x0a, // 10 rec groups

		// rec { go.object }: sub, 0 supertypes, struct with 0 fields.
		opRec, 0x01,
		opSub, 0x00, opStruct, 0x00,

		// rec { go.bytes }: sub, 0 supertypes, array of (mut i8).
		opRec, 0x01,
		opSub, 0x00, opArray, packedI8, fieldVar,

		// rec { go.string }: sub, supertype go.object (index 0), struct
		// of { (ref go.bytes)=index 1 const, i32 const, i32 const }.
		// The standalone go.string is immutable (doc/wasm3-design.md
		// §6.3); its backing array is a non-null reference.
		opRec, 0x01,
		opSub, 0x01, 0x00, // 1 supertype: index 0
		opStruct, 0x03,
		opRef, 0x01, fieldConst, // (ref 1) const
		valI32, fieldConst,
		valI32, fieldConst,

		// rec { go.iptr.<class> } x7: each is a singleton sub group of
		// `(struct (anyref container) (i32 offset))` keyed on the
		// pointee storage class. See doc/wasm3-fat-pointers-design.md.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x02, valAnyref, fieldConst, valI32, fieldConst, // go.iptr.i8
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x02, valAnyref, fieldConst, valI32, fieldConst, // go.iptr.i16
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x02, valAnyref, fieldConst, valI32, fieldConst, // go.iptr.i32
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x02, valAnyref, fieldConst, valI32, fieldConst, // go.iptr.i64
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x02, valAnyref, fieldConst, valI32, fieldConst, // go.iptr.f32
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x02, valAnyref, fieldConst, valI32, fieldConst, // go.iptr.f64
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x02, valAnyref, fieldConst, valI32, fieldConst, // go.iptr.ref
	}
	if !bytes.Equal(payload, want) {
		t.Fatalf("prelude type section mismatch:\n got % x\nwant % x", payload, want)
	}
}

// ValidateModule writes mod to a temp file and runs `wasm-tools
// validate` with the GC feature enabled. The test is skipped if
// wasm-tools is not installed. It is exported so the compiler's wasm3
// collector tests can validate their encoded tables too.
func ValidateModule(t *testing.T, name string, mod []byte) {
	t.Helper()
	tool, err := exec.LookPath("wasm-tools")
	if err != nil {
		t.Skip("wasm-tools not found in PATH; skipping module validation")
	}
	path := filepath.Join(t.TempDir(), name+".wasm")
	if err := os.WriteFile(path, mod, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(tool, "validate", "--features", "gc", path).CombinedOutput()
	if err != nil {
		t.Fatalf("wasm-tools validate failed: %v\n%s\nmodule bytes: % x", err, out, mod)
	}
}

func TestEncodedTypeSectionValidates(t *testing.T) {
	ValidateModule(t, "prelude", WrapModule(Table(PreludeTypes()).EncodeTypeSection()))
}

func TestFuncTypeEncoding(t *testing.T) {
	// A wasm3 module's type section holds function types alongside the
	// GC types. Append a struct and two function types: one that takes a
	// reference to the struct and returns an i64, and the degenerate
	// () -> () signature of an empty main.
	st := NumPreludeTypes
	table := Table(append(PreludeTypes(),
		Type{
			Name:   "go.S",
			Kind:   KindStruct,
			Super:  TypeGoObject,
			Fields: []Field{{Storage: PrimStorage(I64), Mutable: true}},
		},
		Type{
			Name:    "go.func.getv",
			Kind:    KindFunc,
			Super:   -1,
			Params:  []Storage{RefStorage(st, true)},
			Results: []Storage{PrimStorage(I64)},
		},
		Type{
			Name:  "go.func.main",
			Kind:  KindFunc,
			Super: -1,
		},
	))

	// The function type referencing go.S must be emitted after it.
	groups := table.RecGroups()
	if groupOf(groups, st+1) <= groupOf(groups, st) {
		t.Errorf("func type referencing go.S emitted before it")
	}

	ValidateModule(t, "functypes", WrapModule(table.EncodeTypeSection()))
}
