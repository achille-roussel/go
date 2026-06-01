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

	// Prelude: 42 singleton rec groups in table order:
	//   * object, bytes, string
	//   * 7 go.iptr.<class> fat-pointer wrappers (i8..ref)
	//   * go.iface
	//   * 4 getter/setter/ptr accessor-pair triples (i64, ref, f64, f32)
	//   * M4 stack-switching pair go.goroutine.entry + go.cont
	//   * M4 chan base go.chan
	//   * polymorphic fat-pointer hierarchy
	//     (doc/wasm3-fat-ptr-class-prototype): 4 families x 4 types
	//     each — refclass.<X>, ptr.poly.<X>, ptr.array.<X>,
	//     ptr.struct.<X> — for X in {i64, ref, f32, f64}.
	// Typed-ref heap references emit the plain (non-exact) encoding
	// (0x64 / 0x63 followed by typeidx) — the exact-heap-type prefix
	// opExactHeap=0x62 was dropped to match wasmfxtime / wasmparser
	// 1.223, which predates the custom-descriptors extension. See
	// heaptype helper in typesec.go for rationale. Pin the exact
	// bytes — this is the module preamble every wasm3 binary starts with.
	want := []byte{
		0x2a, // 42 rec groups

		// rec { go.object }: sub, 0 supertypes, struct with 0 fields.
		opRec, 0x01,
		opSub, 0x00, opStruct, 0x00,

		// rec { go.bytes }: sub, 0 supertypes, array of (mut i8).
		opRec, 0x01,
		opSub, 0x00, opArray, packedI8, fieldVar,

		// rec { go.string }: sub, supertype go.object (index 0), struct
		// of { (ref (exact go.bytes))=index 1 const, i64 const, i64
		// const }. The standalone go.string is immutable; its backing is
		// a non-null exact reference.
		opRec, 0x01,
		opSub, 0x01, 0x00, // 1 supertype: index 0
		opStruct, 0x03,
		opRef,0x01, fieldConst, // (ref (exact 1)) const
		valI64, fieldConst,
		valI64, fieldConst,

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

		// rec { go.iface }: sub, supertype go.object (index 0), struct of
		// { anyref itab const, anyref data const } — a boxed interface.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x02, valAnyref, fieldConst, valAnyref, fieldConst,

		// rec { go.getter.i64 }: final func type (anyref, i32) -> i64.
		opRec, 0x01, opSubFinal, 0x00, opFunc, 0x02, valAnyref, valI32, 0x01, valI64,
		// rec { go.setter.i64 }: final func type (anyref, i32, i64) -> ().
		opRec, 0x01, opSubFinal, 0x00, opFunc, 0x03, valAnyref, valI32, valI64, 0x00,
		// rec { go.ptr.i64 }: struct { anyref base, i32 offset,
		// (ref (exact go.getter.i64))=index 11,
		// (ref (exact go.setter.i64))=index 12 }.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x04,
		valAnyref, fieldConst,
		valI32, fieldConst,
		opRef,0x0b, fieldConst,
		opRef,0x0c, fieldConst,

		// rec { go.getter.ref }: final func type (anyref, i32) -> anyref.
		opRec, 0x01, opSubFinal, 0x00, opFunc, 0x02, valAnyref, valI32, 0x01, valAnyref,
		// rec { go.setter.ref }: final func type (anyref, i32, anyref) -> ().
		opRec, 0x01, opSubFinal, 0x00, opFunc, 0x03, valAnyref, valI32, valAnyref, 0x00,
		// rec { go.ptr.ref }: struct { anyref base, i32 offset,
		// (ref (exact go.getter.ref))=index 14,
		// (ref (exact go.setter.ref))=index 15 }.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x04,
		valAnyref, fieldConst,
		valI32, fieldConst,
		opRef,0x0e, fieldConst,
		opRef,0x0f, fieldConst,

		// rec { go.getter.f64 }: final func type (anyref, i32) -> f64.
		opRec, 0x01, opSubFinal, 0x00, opFunc, 0x02, valAnyref, valI32, 0x01, valF64,
		// rec { go.setter.f64 }: final func type (anyref, i32, f64) -> ().
		opRec, 0x01, opSubFinal, 0x00, opFunc, 0x03, valAnyref, valI32, valF64, 0x00,
		// rec { go.ptr.f64 }: struct { anyref base, i32 offset,
		// (ref (exact go.getter.f64))=index 17,
		// (ref (exact go.setter.f64))=index 18 }.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x04,
		valAnyref, fieldConst,
		valI32, fieldConst,
		opRef,0x11, fieldConst,
		opRef,0x12, fieldConst,

		// rec { go.getter.f32 }: final func type (anyref, i32) -> f32.
		opRec, 0x01, opSubFinal, 0x00, opFunc, 0x02, valAnyref, valI32, 0x01, valF32,
		// rec { go.setter.f32 }: final func type (anyref, i32, f32) -> ().
		opRec, 0x01, opSubFinal, 0x00, opFunc, 0x03, valAnyref, valI32, valF32, 0x00,
		// rec { go.ptr.f32 }: struct { anyref base, i32 offset,
		// (ref (exact go.getter.f32))=index 20,
		// (ref (exact go.setter.f32))=index 21 }.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x04,
		valAnyref, fieldConst,
		valI32, fieldConst,
		opRef,0x14, fieldConst,
		opRef,0x15, fieldConst,

		// rec { go.goroutine.entry }: final func type () -> () — the
		// universal M4 goroutine entry shape (void). V8 December 2025
		// doesn't implement resume against (func anyref -> anyref);
		// args/results route through globals or tag payloads instead.
		opRec, 0x01, opSubFinal, 0x00, opFunc, 0x00, 0x00,

		// rec { go.cont }: final cont type wrapping go.goroutine.entry
		// (table index 23 → wasm index 23).
		opRec, 0x01, opSubFinal, 0x00, opCont, 0x17,

		// rec { go.chan }: sub go.object, 7 mut i32 fields: qcount,
		// dataqsiz, sendx, recvx, closed, sendParkID, recvParkID.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x07,
		valI32, fieldVar, valI32, fieldVar, valI32, fieldVar, valI32, fieldVar,
		valI32, fieldVar, valI32, fieldVar, valI32, fieldVar,

		// rec { go.refclass.i64 }: vtable struct {(ref getter.i64)=index 11,
		// (ref setter.i64)=index 12}. Carries the funcrefs that polymorphic
		// dereferences call_ref against.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x02,
		opRef, 0x0b, fieldConst,
		opRef, 0x0c, fieldConst,
		// rec { go.ptr.poly.i64 }: polymorphic base, sub go.object, struct
		// with one field (ref refclass.i64)=index 26.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x01,
		opRef, 0x1a, fieldConst,
		// rec { go.ptr.array.i64 }: sub ptr.poly.i64=index 27, struct
		// {class (inherited), anyref backing, i32 offset}.
		opRec, 0x01, opSub, 0x01, 0x1b, opStruct, 0x03,
		opRef, 0x1a, fieldConst, valAnyref, fieldConst, valI32, fieldConst,
		// rec { go.ptr.struct.i64 }: sub ptr.poly.i64=index 27, struct
		// {class (inherited), anyref container}.
		opRec, 0x01, opSub, 0x01, 0x1b, opStruct, 0x02,
		opRef, 0x1a, fieldConst, valAnyref, fieldConst,

		// rec { go.refclass.ref }: vtable {(ref getter.ref)=14, (ref setter.ref)=15}.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x02,
		opRef, 0x0e, fieldConst,
		opRef, 0x0f, fieldConst,
		// rec { go.ptr.poly.ref }: {(ref refclass.ref)=30}.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x01,
		opRef, 0x1e, fieldConst,
		// rec { go.ptr.array.ref }: sub poly.ref=31, {class, anyref, i32}.
		opRec, 0x01, opSub, 0x01, 0x1f, opStruct, 0x03,
		opRef, 0x1e, fieldConst, valAnyref, fieldConst, valI32, fieldConst,
		// rec { go.ptr.struct.ref }: sub poly.ref=31, {class, anyref}.
		opRec, 0x01, opSub, 0x01, 0x1f, opStruct, 0x02,
		opRef, 0x1e, fieldConst, valAnyref, fieldConst,

		// rec { go.refclass.f32 }: vtable {(ref getter.f32)=20, (ref setter.f32)=21}.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x02,
		opRef, 0x14, fieldConst,
		opRef, 0x15, fieldConst,
		// rec { go.ptr.poly.f32 }: {(ref refclass.f32)=34}.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x01,
		opRef, 0x22, fieldConst,
		// rec { go.ptr.array.f32 }: sub poly.f32=35, {class, anyref, i32}.
		opRec, 0x01, opSub, 0x01, 0x23, opStruct, 0x03,
		opRef, 0x22, fieldConst, valAnyref, fieldConst, valI32, fieldConst,
		// rec { go.ptr.struct.f32 }: sub poly.f32=35, {class, anyref}.
		opRec, 0x01, opSub, 0x01, 0x23, opStruct, 0x02,
		opRef, 0x22, fieldConst, valAnyref, fieldConst,

		// rec { go.refclass.f64 }: vtable {(ref getter.f64)=17, (ref setter.f64)=18}.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x02,
		opRef, 0x11, fieldConst,
		opRef, 0x12, fieldConst,
		// rec { go.ptr.poly.f64 }: {(ref refclass.f64)=38}.
		opRec, 0x01, opSub, 0x01, 0x00, opStruct, 0x01,
		opRef, 0x26, fieldConst,
		// rec { go.ptr.array.f64 }: sub poly.f64=39, {class, anyref, i32}.
		opRec, 0x01, opSub, 0x01, 0x27, opStruct, 0x03,
		opRef, 0x26, fieldConst, valAnyref, fieldConst, valI32, fieldConst,
		// rec { go.ptr.struct.f64 }: sub poly.f64=39, {class, anyref}.
		opRec, 0x01, opSub, 0x01, 0x27, opStruct, 0x02,
		opRef, 0x26, fieldConst, valAnyref, fieldConst,
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
	// "all" covers the GC, custom-descriptors, and stack-switching
	// extensions wasm3 uses. The default feature set is "stable proposals
	// at phase 4+", which excludes stack-switching.
	out, err := exec.Command(tool, "validate", "--features", "all", path).CombinedOutput()
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
