// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm

// asm3.go is the module assembler for GOARCH=wasm3. Where asm.go emits
// the GOARCH=wasm module — a flat vector of (i32)->i32 function types, a
// funcref table replayed by the PC_F/PC_B scheme, and a CallIndirect
// dispatcher — asm3.go emits a WebAssembly 3.0 module: a single type
// section holding GC struct/array types and real per-function signatures
// (cmd/internal/wasmgc), native typed functions, direct calls, and no
// table or element section. See doc/wasm3-m2-design.md §3 and
// doc/wasm3-m2-cutover-notes.md §2.
//
// Status: work in progress on the wasm3-m2-cutover branch. asmb2_3
// produces the full module section structure and the GC-aware type
// section. It is wired in by Init when GOARCH=wasm3. What is NOT yet
// done — and is called out with TODO(wasm3) below — is consuming the
// per-package wasmgc.Table aux the compiler will emit: until that lands,
// only primitive-typed signatures (which covers the empty-main and
// arithmetic rungs of the bring-up ladder) are interned; a reference
// field panics. Reference-typed signatures land with the compiler-side
// table emission (cutover step 1/2 remainder).

import (
	"bytes"
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"cmd/internal/wasmgc"
	"cmd/link/internal/ld"
	"cmd/link/internal/loader"
	"fmt"
)

// asmb3 collects the linear-memory data sections, exactly as asmb does
// for GOARCH=wasm: wasm3 still has a linear memory for the WASI bump
// allocator (doc/wasm3-m2-design.md §12.6), so the data-section handling
// is unchanged.
func asmb3(ctxt *ld.Link, ldr *loader.Loader) {
	asmb(ctxt, ldr)
}

// wasm3Module accumulates the per-function data asmb2_3 needs after the
// type table has been built.
type wasm3Module struct {
	table   wasmgc.Table // the merged GC + function type table
	funcs   []*wasm3Func
	imports []*wasm3Func
}

type wasm3Func struct {
	Module string
	Name   string
	TypeIx uint32 // index into wasm3Module.table
	Code   []byte
}

// internFuncType appends a function type to the table if an identical
// one is not already present, and returns its table index. Function
// types are interned so the many ()->() signatures of an empty program
// collapse to one entry.
func (m *wasm3Module) internFuncType(params, results []wasmgc.Storage) uint32 {
	want := wasmgc.Type{Kind: wasmgc.KindFunc, Super: -1, Params: params, Results: results}
	for i, t := range m.table {
		if t.Kind != wasmgc.KindFunc {
			continue
		}
		if storagesEqual(t.Params, want.Params) && storagesEqual(t.Results, want.Results) {
			return uint32(i)
		}
	}
	m.table = append(m.table, want)
	return uint32(len(m.table) - 1)
}

func storagesEqual(a, b []wasmgc.Storage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// objStorage converts one obj.WasmField — the linker's view of a
// compiler-lowered parameter or result slot — to a wasmgc.Storage.
// Reference fields require a remap table (see objStorageRemapped); the
// no-remap form is used for host-import signatures, which never carry
// references.
func objStorage(f obj.WasmField) wasmgc.Storage {
	switch f.Type {
	case obj.WasmI32, obj.WasmPtr, obj.WasmBool:
		return wasmgc.PrimStorage(wasmgc.I32)
	case obj.WasmI64:
		return wasmgc.PrimStorage(wasmgc.I64)
	case obj.WasmF32:
		return wasmgc.PrimStorage(wasmgc.F32)
	case obj.WasmF64:
		return wasmgc.PrimStorage(wasmgc.F64)
	case obj.WasmRef:
		panic("wasm3: WasmRef in a host-import signature has no per-package table to remap against")
	default:
		panic(fmt.Sprintf("wasm3: unknown obj.WasmField type %d", f.Type))
	}
}

func objStorages(fields []obj.WasmField) []wasmgc.Storage {
	if len(fields) == 0 {
		return nil
	}
	out := make([]wasmgc.Storage, len(fields))
	for i, f := range fields {
		out[i] = objStorage(f)
	}
	return out
}

// objStorageRemapped is objStorage but for fields whose WasmRef.Offset
// is a per-package type index that has been remapped to a module-wide
// index via the remap table from mergeTable.
func objStorageRemapped(f obj.WasmField, remap []int) wasmgc.Storage {
	if f.Type == obj.WasmRef {
		pkgIx := int(f.Offset)
		if pkgIx < 0 || pkgIx >= len(remap) {
			panic(fmt.Sprintf("wasm3: WasmRef Offset %d out of range for per-package table of size %d", pkgIx, len(remap)))
		}
		return wasmgc.RefStorage(int(remap[pkgIx]), false)
	}
	return objStorage(f)
}

func objStoragesRemapped(fields []obj.WasmField, remap []int) []wasmgc.Storage {
	if len(fields) == 0 {
		return nil
	}
	out := make([]wasmgc.Storage, len(fields))
	for i, f := range fields {
		out[i] = objStorageRemapped(f, remap)
	}
	return out
}

// mergeTable folds a function's per-package wasmgc.Table (carried in
// the WasmType aux's tail bytes) into m's module-wide table and
// returns a remap table mapping each per-package index to the merged
// module index.
//
// The per-package table starts with the same wasmgc.PreludeTypes()
// the module-wide table starts with — both produced by the same
// constructors — so per-pkg indices 0 .. NumPreludeTypes-1 always map
// to the matching prelude indices in the module table. Program types
// (per-pkg index NumPreludeTypes onward) are appended to the module
// table, with their internal references remapped to their new module
// positions.
//
// Today the merge appends each package's program types unconditionally
// — duplicates across packages produce duplicate type-section entries,
// which is valid wasm but redundant. Structural deduplication across
// packages is a later refinement; for the M2 cutover the priority is
// correctness, not table compactness.
//
// An empty pkgTableBytes — a primitive-only signature with no
// reference fields — is the fast path with a nil remap.
func (m *wasm3Module) mergeTable(pkgTableBytes []byte) []int {
	if len(pkgTableBytes) == 0 {
		return nil
	}
	pkg := wasmgc.ReadTable(pkgTableBytes)
	remap := make([]int, len(pkg))
	// Prelude indices map identically to the module table's prelude.
	for i := 0; i < wasmgc.NumPreludeTypes && i < len(pkg); i++ {
		remap[i] = i
	}
	// Reserve module slots for every program type in the per-package
	// table first, so pass 2's nested-ref remapping always lands.
	for i := wasmgc.NumPreludeTypes; i < len(pkg); i++ {
		remap[i] = len(m.table)
		m.table = append(m.table, wasmgc.Type{})
	}
	// Pass 2: fill the reserved slots with remapped types.
	for i := wasmgc.NumPreludeTypes; i < len(pkg); i++ {
		t := pkg[i]
		dst := remap[i]
		remappedT := wasmgc.Type{
			Name:    t.Name,
			Kind:    t.Kind,
			Super:   -1,
			ElemMut: t.ElemMut,
		}
		if t.Super >= 0 {
			remappedT.Super = int(remap[int(t.Super)])
		}
		for _, f := range t.Fields {
			f.Storage = remapStorage(f.Storage, remap)
			remappedT.Fields = append(remappedT.Fields, f)
		}
		remappedT.Elem = remapStorage(t.Elem, remap)
		for _, p := range t.Params {
			remappedT.Params = append(remappedT.Params, remapStorage(p, remap))
		}
		for _, r := range t.Results {
			remappedT.Results = append(remappedT.Results, remapStorage(r, remap))
		}
		m.table[dst] = remappedT
	}
	return remap
}

// remapStorage rewrites a wasmgc.Storage's RefType from a per-package
// index to a module-wide index using remap. Primitive storages pass
// through.
func remapStorage(s wasmgc.Storage, remap []int) wasmgc.Storage {
	if !s.IsRef() {
		return s
	}
	pkgIx := int(s.RefType)
	if pkgIx < 0 || pkgIx >= len(remap) {
		// Negative or out-of-range indices are reserved/sentinel
		// values (e.g. wasmgc.TypeGoObject = -1). Pass through.
		return s
	}
	return wasmgc.Storage{
		Prim:    s.Prim,
		RefType: int(remap[pkgIx]),
		RefNull: s.RefNull,
	}
}

// asmb2_3 writes the final WebAssembly 3.0 module binary for
// GOARCH=wasm3.
func asmb2_3(ctxt *ld.Link, ldr *loader.Loader) {
	m := &wasm3Module{table: wasmgc.PreludeTypes()}

	// Host imports: WASI functions the module imports. Their signatures
	// are still primitive (linear-memory pointers as i32), so they
	// intern without needing the GC type table.
	hostImportMap := make(map[loader.Sym]int64)
	for _, fn := range ctxt.Textp {
		relocs := ldr.Relocs(fn)
		for ri := 0; ri < relocs.Count(); ri++ {
			r := relocs.At(ri)
			if r.Type() != objabi.R_WASMIMPORT {
				continue
			}
			wsym := ldr.WasmImportSym(fn)
			if wsym == 0 {
				panic(fmt.Sprintf("missing wasm import symbol for %s", ldr.SymName(r.Sym())))
			}
			wi := readWasmImport(ldr, wsym)
			hostImportMap[fn] = int64(len(m.imports))
			m.imports = append(m.imports, &wasm3Func{
				Module: wi.Module,
				Name:   wi.Name,
				TypeIx: m.internFuncType(objStorages(wi.Params), objStorages(wi.Results)),
			})
		}
	}

	// Native functions: each carries an obj.WasmType aux with its typed
	// signature. Functions emitted before the compiler attaches one
	// (e.g. hand-written assembly stubs during bring-up) fall back to
	// the degenerate ()->() signature. //go:wasmimport stubs are kept
	// in m.funcs alongside imports — their body (via
	// assembleWasm3ImportWrapper) forwards directly to the host import,
	// matching the wasm linker's convention. Their type comes from the
	// WasmImport aux rather than WasmType.
	//
	// Order matters: the per-function wasmgc.Table is merged into the
	// module table *before* writeWasm3FuncBody runs, so the resulting
	// per-package→global remap is available when the body's
	// R_WASMTYPE relocations need to be resolved.
	var buildid []byte
	m.funcs = make([]*wasm3Func, len(ctxt.Textp))
	for i, fn := range ctxt.Textp {
		var typeIx uint32
		var remap []int
		if ldr.SymName(fn) != "go:buildid" {
			if wsym := ldr.WasmImportSym(fn); wsym != 0 {
				wi := readWasmImport(ldr, wsym)
				typeIx = m.internFuncType(objStorages(wi.Params), objStorages(wi.Results))
			} else if s := ldr.WasmTypeSym(fn); s != 0 {
				var wt obj.WasmType
				wt.Read(ldr.Data(s))
				remap = m.mergeTable(wt.Table)
				params := objStoragesRemapped(wt.Params, remap)
				results := objStoragesRemapped(wt.Results, remap)
				typeIx = m.internFuncType(params, results)
			} else {
				typeIx = m.internFuncType(nil, nil)
			}
		} else {
			typeIx = m.internFuncType(nil, nil)
		}

		wfn := new(bytes.Buffer)
		if ldr.SymName(fn) == "go:buildid" {
			writeUleb128(wfn, 0) // number of sets of locals
			wfn.WriteByte(0x0b)  // end
			buildid = ldr.Data(fn)
		} else {
			writeWasm3FuncBody(ctxt, ldr, fn, wfn, hostImportMap, remap)
		}

		name := nameRegexp.ReplaceAllString(ldr.SymName(fn), "_")
		m.funcs[i] = &wasm3Func{Name: name, TypeIx: typeIx, Code: wfn.Bytes()}
	}

	ctxt.Out.Write([]byte{0x00, 0x61, 0x73, 0x6d}) // magic
	ctxt.Out.Write([]byte{0x01, 0x00, 0x00, 0x00}) // version

	if len(buildid) != 0 {
		writeBuildID(ctxt, buildid)
	}

	writeTypeSec3(ctxt, m.table)
	writeImportSec3(ctxt, m.imports)
	writeFunctionSec3(ctxt, m.funcs)
	writeMemorySec(ctxt, ldr)
	writeGlobalSec3(ctxt)
	writeExportSec(ctxt, ldr, len(m.imports))
	writeCodeSec3(ctxt, m.funcs)
	writeDataSec(ctxt)
	writeProducerSec(ctxt)
	if !*ld.FlagS {
		writeNameSec3(ctxt, len(m.imports), m.funcs)
	}
}

// writeWasm3FuncBody copies a function's machine code into wfn,
// resolving relocations. Unlike GOARCH=wasm, an R_CALL becomes a direct
// function index (no funcValueOffset / PC_F encoding).
//
// `remap`, if non-nil, is the per-package→global type-index translation
// table for the function's wasmgc.Table — used by R_WASMTYPE
// relocations on 0xFB-prefixed GC opcodes (struct.new, struct.get,
// etc.). Empty for functions whose signature uses only primitive types.
func writeWasm3FuncBody(ctxt *ld.Link, ldr *loader.Loader, fn loader.Sym, wfn *bytes.Buffer, hostImportMap map[loader.Sym]int64, remap []int) {
	relocs := ldr.Relocs(fn)
	P := ldr.Data(fn)
	off := int32(0)
	for ri := 0; ri < relocs.Count(); ri++ {
		r := relocs.At(ri)
		if r.Siz() == 0 {
			continue // marker relocation
		}
		wfn.Write(P[off:r.Off()])
		off = r.Off()
		rs := r.Sym()
		switch r.Type() {
		case objabi.R_ADDR:
			writeSleb128(wfn, ldr.SymValue(rs)+r.Add())
		case objabi.R_CALL:
			// Direct function index: imports first (in import-section
			// order), then native functions in Textp order. The >>16
			// recovers the function ordinal from the address
			// assignAddress encoded. A call to a wasmimport stub goes
			// through the stub's wasm function index — the stub's body
			// is itself a one-line forwarder to the host import via
			// R_WASMIMPORT, so the extra hop is the only cost.
			writeSleb128(wfn, int64(len(hostImportMap))+ldr.SymValue(rs)>>16-funcValueOffset)
		case objabi.R_WASMIMPORT:
			writeSleb128(wfn, hostImportMap[rs])
		case objabi.R_WASMTYPE:
			// The relocation's Add carries the per-package type
			// index; remap to the module-global index using the
			// function's merged-table remap. Emitted as uleb128 (not
			// sleb128) — wasm type indices are unsigned in the binary
			// format.
			pkgIx := int(r.Add())
			if pkgIx < 0 || pkgIx >= len(remap) {
				ldr.Errorf(fn, "R_WASMTYPE per-package index %d out of range for remap len %d", pkgIx, len(remap))
				continue
			}
			writeUleb128(wfn, uint64(remap[pkgIx]))
		default:
			ldr.Errorf(fn, "bad reloc type %d for wasm3", r.Type())
		}
	}
	wfn.Write(P[off:])
}

// writeTypeSec3 writes the WebAssembly 3.0 type section: a single
// section holding the GC struct/array types and every function
// signature, encoded by cmd/internal/wasmgc.
func writeTypeSec3(ctxt *ld.Link, table wasmgc.Table) {
	sizeOffset := writeSecHeader(ctxt, sectionType)
	ctxt.Out.Write(table.EncodeTypeSection())
	writeSecSize(ctxt, sizeOffset)
}

// writeImportSec3 writes the import section for wasm3. It mirrors
// writeImportSec but indexes the wasm3 type table.
func writeImportSec3(ctxt *ld.Link, imports []*wasm3Func) {
	sizeOffset := writeSecHeader(ctxt, sectionImport)
	writeUleb128(ctxt.Out, uint64(len(imports)))
	for _, fn := range imports {
		writeName(ctxt.Out, fn.Module)
		writeName(ctxt.Out, fn.Name)
		ctxt.Out.WriteByte(0x00) // func import
		writeUleb128(ctxt.Out, uint64(fn.TypeIx))
	}
	writeSecSize(ctxt, sizeOffset)
}

// writeFunctionSec3 declares each native function's type index.
func writeFunctionSec3(ctxt *ld.Link, fns []*wasm3Func) {
	sizeOffset := writeSecHeader(ctxt, sectionFunction)
	writeUleb128(ctxt.Out, uint64(len(fns)))
	for _, fn := range fns {
		writeUleb128(ctxt.Out, uint64(fn.TypeIx))
	}
	writeSecSize(ctxt, sizeOffset)
}

// writeGlobalSec3 writes the global section for wasm3. GOARCH=wasm needs
// eight globals for its virtual machine (SP, CTXT, g, RET0-3, PAUSE);
// wasm3 has typed functions and host-managed stacks, so it needs only
// the linear-memory bump-allocator pointer (doc/wasm3-m2-design.md
// §12.6). The bump pointer starts at runtime.end, fixed up below once
// the compiler emits the allocator; for now it is a single mutable i32.
func writeGlobalSec3(ctxt *ld.Link) {
	sizeOffset := writeSecHeader(ctxt, sectionGlobal)
	writeUleb128(ctxt.Out, 1) // number of globals
	ctxt.Out.WriteByte(I32)
	ctxt.Out.WriteByte(0x01) // mutable
	writeI32Const(ctxt.Out, 0)
	ctxt.Out.WriteByte(0x0b) // end
	writeSecSize(ctxt, sizeOffset)
}

// writeCodeSec3 writes the function bodies.
func writeCodeSec3(ctxt *ld.Link, fns []*wasm3Func) {
	sizeOffset := writeSecHeader(ctxt, sectionCode)
	writeUleb128(ctxt.Out, uint64(len(fns)))
	for _, fn := range fns {
		writeUleb128(ctxt.Out, uint64(len(fn.Code)))
		ctxt.Out.Write(fn.Code)
	}
	writeSecSize(ctxt, sizeOffset)
}

// writeNameSec3 writes the name custom section for wasm3.
func writeNameSec3(ctxt *ld.Link, firstFnIndex int, fns []*wasm3Func) {
	sizeOffset := writeSecHeader(ctxt, sectionCustom)
	writeName(ctxt.Out, "name")
	sizeOffset2 := writeSecHeader(ctxt, 0x01) // function names
	writeUleb128(ctxt.Out, uint64(len(fns)))
	for i, fn := range fns {
		writeUleb128(ctxt.Out, uint64(firstFnIndex+i))
		writeName(ctxt.Out, fn.Name)
	}
	writeSecSize(ctxt, sizeOffset2)
	writeSecSize(ctxt, sizeOffset)
}
