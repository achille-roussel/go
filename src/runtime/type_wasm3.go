// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import (
	"internal/abi"
	"unsafe"
)

// wasm3 mirror of type.go.
//
// type.go is tagged !wasm3 because most of its body walks the
// moduledata.types blob via `(*_type)(unsafe.Pointer(td))` casts
// (line 541) and computes name/type offsets via `uintptr(
// ptrInModule) - md.types` arithmetic (lines 298, 334, 355).
// On wasm3 there's no real types blob in linear memory: type
// descriptors are emitted as WasmGC ref-globals (#167 in progress)
// and looked up by ref identity through compiler-emitted code,
// not the moduledata pclntab-walking path. The runtime walkers
// in type.go are unreachable on wasm3.
//
// What this file provides is just the type aliases other files
// in the wasm3 build pull from runtime/type.go (most of type.go
// is `type X = abi.Y` aliases — pure naming with no body). The
// pclntab walkers (resolveNameOff, resolveTypeOff, typelinksinit,
// moduleTypelinks) are not referenced from the cascade after
// gating, so they aren't mirrored here. Add stubs if the cascade
// surfaces a need later.

// ---- type aliases (same as the !wasm3 file) -------------------

type _type = abi.Type

type nameOff = abi.NameOff
type typeOff = abi.TypeOff
type textOff = abi.TextOff

type uncommontype = abi.UncommonType
type interfacetype = abi.InterfaceType
type arraytype = abi.ArrayType
type chantype = abi.ChanType
type slicetype = abi.SliceType
type functype = abi.FuncType
type ptrtype = abi.PtrType
type name = abi.Name
type structtype = abi.StructType

// ---- rtype methods (verbatim from type.go; no ptr-arith) ------

type rtype struct {
	*abi.Type
}

func toRType(t *abi.Type) rtype { return rtype{t} }

func (t rtype) string() string {
	s := t.nameOff(t.Str).Name()
	if t.TFlag&abi.TFlagExtraStar != 0 {
		return s[1:]
	}
	return s
}

func (t rtype) uncommon() *uncommontype { return t.Uncommon() }

func (t rtype) name() string {
	if t.TFlag&abi.TFlagNamed == 0 {
		return ""
	}
	s := t.string()
	i := len(s) - 1
	sqBrackets := 0
	for i >= 0 && (s[i] != '.' || sqBrackets != 0) {
		switch s[i] {
		case ']':
			sqBrackets++
		case '[':
			sqBrackets--
		}
		i--
	}
	return s[i+1:]
}

func (t rtype) pkgpath() string {
	if u := t.uncommon(); u != nil {
		return t.nameOff(u.PkgPath).Name()
	}
	switch t.Kind() {
	case abi.Struct:
		st := (*structtype)(unsafe.Pointer(t.Type))
		return st.PkgPath.Name()
	case abi.Interface:
		it := (*interfacetype)(unsafe.Pointer(t.Type))
		return it.PkgPath.Name()
	}
	return ""
}

// nameOff / typeOff / textOff on rtype look up names/types/text
// in the owning module's pclntab on non-wasm3. wasm3 has a stub
// pclntab (#166) — these resolve to empty name / nil type / nil
// text. The lookups are still called from alg/error/runtime1
// helpers, so the methods must exist.

func (t rtype) nameOff(off nameOff) name {
	return name{}
}

func (t rtype) typeOff(off typeOff) *_type {
	return nil
}

func (t rtype) textOff(off textOff) unsafe.Pointer {
	return nil
}

// ---- module-level reflectOffs (referenced by proc.go) ----------

var reflectOffs struct {
	lock mutex
	next int32
	m    map[int32]unsafe.Pointer
	minv map[unsafe.Pointer]int32
}

func reflectOffsLock()   { lock(&reflectOffs.lock) }
func reflectOffsUnlock() { unlock(&reflectOffs.lock) }

// ---- pclntab walker stubs --------------------------------------
//
// On wasm3 type descriptors live as WasmGC ref-globals (#167 in
// progress) and are looked up by ref identity through compiler-
// emitted code, not by walking md.types blobs. The walkers below
// return zero values to satisfy the link surface; their callers
// (reflect_typelinks, reflect_resolveNameOff, ...) degrade to
// "no info" cleanly.

func resolveNameOff(ptrInModule unsafe.Pointer, off nameOff) name {
	return name{}
}

func resolveTypeOff(ptrInModule unsafe.Pointer, off typeOff) *_type {
	return nil
}

func typelinksinit() {}

func moduleTypelinks(md *moduledata) []*_type { return nil }
