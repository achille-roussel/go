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

	// Prelude: 3 singleton rec groups in table order (object, bytes,
	// string). Pin the exact bytes — this is the module preamble every
	// wasm3 binary starts with.
	want := []byte{
		0x03, // 3 rec groups

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
