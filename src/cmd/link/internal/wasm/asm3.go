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
	"sort"
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

	// closureSingletons records, per (function symbol, closureCtx
	// type index) pair, the assigned wasm-global index of the
	// closure-singleton for that pair. Built lazily during
	// writeWasm3FuncBody as R_WASMCLOSURESINGLETON relocations are
	// processed; consumed by writeGlobalSec3 to emit one
	// `(struct.new $closureCtx (ref.func $sym) (i64.const 0))`
	// global per pair, and again at reloc resolution time so the
	// `global.get` operand patches to the right index.
	closureSingletons     map[wasm3SingletonKey]uint64
	closureSingletonOrder []wasm3SingletonKey
}

// wasm3SingletonKey identifies a closure-singleton uniquely by the
// function it references and the closureCtx struct type.
type wasm3SingletonKey struct {
	sym         loader.Sym
	globalCtxIx int
}

// getOrAllocSingleton returns the wasm global index assigned to the
// closure-singleton for (sym, globalCtxIx). The global is allocated
// the first time the pair is seen; subsequent calls return the same
// index. The wasm bump-pointer global occupies index 0 and CTXT
// occupies 1, so singletons start at index 2.
func (m *wasm3Module) getOrAllocSingleton(sym loader.Sym, globalCtxIx int) uint64 {
	key := wasm3SingletonKey{sym: sym, globalCtxIx: globalCtxIx}
	if m.closureSingletons == nil {
		m.closureSingletons = map[wasm3SingletonKey]uint64{}
	}
	if idx, ok := m.closureSingletons[key]; ok {
		return idx
	}
	idx := uint64(3 + len(m.closureSingletonOrder)) // 0=bump, 1=CTXT, 2=CTXT_REF, then singletons
	m.closureSingletons[key] = idx
	m.closureSingletonOrder = append(m.closureSingletonOrder, key)
	return idx
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
	case obj.WasmAnyref:
		return wasmgc.AnyRefStorage()
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
	// Single pass: for each non-prelude type in dependency order,
	// compute its remapped form (refs into already-merged indices),
	// then either dedupe against an existing structurally-identical
	// entry or append. The per-package table is emitted in
	// dependency order by typeCollector — every type's refs point
	// to earlier per-pkg indices, all of which have already been
	// remapped on this pass — so the deduper can see the fully
	// canonical form before deciding.
	//
	// Self-referential types (the box-via-ref shape a recursive Go
	// struct produces) lose dedup on the self-ref leg: the type's
	// remapped form contains a forward ref to its own about-to-be-
	// assigned module index. We detect this with a tombstone
	// sentinel placed before remapping; if a self-ref is detected,
	// we append unconditionally rather than risk a wrong dedup.
	const selfRefSentinel = -42
	for i := wasmgc.NumPreludeTypes; i < len(pkg); i++ {
		remap[i] = selfRefSentinel
		t := pkg[i]
		remappedT := wasmgc.Type{
			Name:    t.Name,
			Kind:    t.Kind,
			Super:   -1,
			ElemMut: t.ElemMut,
		}
		if t.Super >= 0 {
			remappedT.Super = int(remap[int(t.Super)])
		}
		hasSelfRef := remappedT.Super == selfRefSentinel
		for _, f := range t.Fields {
			f.Storage = remapStorage(f.Storage, remap)
			if f.Storage.IsRef() && f.Storage.RefType == selfRefSentinel {
				hasSelfRef = true
			}
			remappedT.Fields = append(remappedT.Fields, f)
		}
		remappedT.Elem = remapStorage(t.Elem, remap)
		if remappedT.Elem.IsRef() && remappedT.Elem.RefType == selfRefSentinel {
			hasSelfRef = true
		}
		for _, p := range t.Params {
			pr := remapStorage(p, remap)
			if pr.IsRef() && pr.RefType == selfRefSentinel {
				hasSelfRef = true
			}
			remappedT.Params = append(remappedT.Params, pr)
		}
		for _, r := range t.Results {
			rr := remapStorage(r, remap)
			if rr.IsRef() && rr.RefType == selfRefSentinel {
				hasSelfRef = true
			}
			remappedT.Results = append(remappedT.Results, rr)
		}
		if hasSelfRef {
			// Patch self-refs and append without dedup. The
			// self-ref pattern is rare (only recursive Go
			// structs); skipping dedup keeps the encoding
			// correct.
			dst := len(m.table)
			fixupSelfRefs(&remappedT, selfRefSentinel, dst)
			m.table = append(m.table, remappedT)
			remap[i] = dst
			continue
		}
		// Dedupe: scan m.table for a structurally-identical entry
		// (ignoring the Name field, which is debug-only). Linear
		// search is O(N*M) but the table is small in practice — a
		// few hundred entries per module — and avoiding a hash
		// keeps the comparison logic close to typesEqual.
		if existing, ok := findEqualType(m.table, remappedT); ok {
			remap[i] = existing
			continue
		}
		remap[i] = len(m.table)
		m.table = append(m.table, remappedT)
	}
	return remap
}

// fixupSelfRefs rewrites every selfRefSentinel inside t to the given
// concrete index. Used by the self-referential append path in
// mergeTable.
func fixupSelfRefs(t *wasmgc.Type, sentinel, real int) {
	if t.Super == sentinel {
		t.Super = real
	}
	for i := range t.Fields {
		if t.Fields[i].Storage.IsRef() && t.Fields[i].Storage.RefType == sentinel {
			t.Fields[i].Storage.RefType = real
		}
	}
	if t.Elem.IsRef() && t.Elem.RefType == sentinel {
		t.Elem.RefType = real
	}
	for i := range t.Params {
		if t.Params[i].IsRef() && t.Params[i].RefType == sentinel {
			t.Params[i].RefType = real
		}
	}
	for i := range t.Results {
		if t.Results[i].IsRef() && t.Results[i].RefType == sentinel {
			t.Results[i].RefType = real
		}
	}
}

// findEqualType returns the index of a wasmgc.Type structurally
// equivalent to want (ignoring the Name field), or false if none.
func findEqualType(table wasmgc.Table, want wasmgc.Type) (int, bool) {
	for i, t := range table {
		if t.Kind != want.Kind || t.Super != want.Super || t.ElemMut != want.ElemMut {
			continue
		}
		if !storagesEqual(t.Params, want.Params) || !storagesEqual(t.Results, want.Results) {
			continue
		}
		if t.Elem != want.Elem {
			continue
		}
		if len(t.Fields) != len(want.Fields) {
			continue
		}
		match := true
		for j := range t.Fields {
			if t.Fields[j] != want.Fields[j] {
				match = false
				break
			}
		}
		if match {
			return i, true
		}
	}
	return -1, false
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
			writeWasm3FuncBody(ctxt, ldr, fn, wfn, hostImportMap, remap, m)
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
	writeGlobalSec3(ctxt, ldr, m, hostImportMap)
	writeExportSec(ctxt, ldr, len(m.imports))
	if refFns := collectWasm3RefFuncs(ctxt, ldr, len(m.imports)); len(refFns) > 0 {
		writeElementSec3DeclaredFuncs(ctxt, refFns)
	}
	writeCodeSec3(ctxt, m.funcs)
	writeDataSec(ctxt)
	writeProducerSec(ctxt)
	if !*ld.FlagS {
		writeNameSec3(ctxt, len(m.imports), m.funcs)
	}
}

// collectWasm3RefFuncs walks every reachable function's relocations
// for R_WASMREFFUNC entries and returns the deduplicated, sorted
// list of module-global funcidx values of their targets. Used by
// writeElementSec3DeclaredFuncs to emit the passive-declared element
// segment wasm 3.0 requires to validate `ref.func` instructions.
func collectWasm3RefFuncs(ctxt *ld.Link, ldr *loader.Loader, numImports int) []uint64 {
	seen := make(map[uint64]bool)
	for _, fn := range ctxt.Textp {
		relocs := ldr.Relocs(fn)
		for ri := 0; ri < relocs.Count(); ri++ {
			r := relocs.At(ri)
			if r.Type() != objabi.R_WASMREFFUNC {
				continue
			}
			rs := r.Sym()
			idx := uint64(int64(numImports) + ldr.SymValue(rs)>>16 - funcValueOffset)
			seen[idx] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(seen))
	for idx := range seen {
		out = append(out, idx)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// writeElementSec3DeclaredFuncs emits a single passive-declared
// element segment listing every funcidx referenced via ref.func.
// Encoding (wasm spec, section 9):
//   0x09 <size> 0x01 <num-elems>
//     0x03 <elemkind=0x00> <vec(funcidx)>
// where 0x03 is the "passive declared" flag and 0x00 is funcref.
func writeElementSec3DeclaredFuncs(ctxt *ld.Link, funcIndices []uint64) {
	sizeOffset := writeSecHeader(ctxt, sectionElement)
	writeUleb128(ctxt.Out, 1) // number of element segments
	ctxt.Out.WriteByte(0x03)  // flags: passive | declared
	ctxt.Out.WriteByte(0x00)  // elemkind: funcref
	writeUleb128(ctxt.Out, uint64(len(funcIndices)))
	for _, idx := range funcIndices {
		writeUleb128(ctxt.Out, idx)
	}
	writeSecSize(ctxt, sizeOffset)
}

// writeWasm3FuncBody copies a function's machine code into wfn,
// resolving relocations. Unlike GOARCH=wasm, an R_CALL becomes a direct
// function index (no funcValueOffset / PC_F encoding).
//
// `remap`, if non-nil, is the per-package→global type-index translation
// table for the function's wasmgc.Table — used by R_WASMTYPE
// relocations on 0xFB-prefixed GC opcodes (struct.new, struct.get,
// etc.). Empty for functions whose signature uses only primitive types.
//
// `m` provides the module-level singleton registry; R_WASMCLOSURESINGLETON
// relocs allocate (or look up) one wasm global per (function sym,
// closureCtx type) pair via m.getOrAllocSingleton.
func writeWasm3FuncBody(ctxt *ld.Link, ldr *loader.Loader, fn loader.Sym, wfn *bytes.Buffer, hostImportMap map[loader.Sym]int64, remap []int, m *wasm3Module) {
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
		case objabi.R_WASMREFFUNC:
			// Stage G: same encoding as R_CALL (the funcidx), but the
			// target was additionally registered as a declared
			// function by the earlier collectWasm3RefFuncs pass so
			// the passive-declared element segment validates the
			// ref.func instruction.
			writeSleb128(wfn, int64(len(hostImportMap))+ldr.SymValue(rs)>>16-funcValueOffset)
		case objabi.R_WASMIMPORT:
			writeSleb128(wfn, hostImportMap[rs])
		case objabi.R_WASMCLOSURESINGLETON:
			// Stage G singleton: r.Sym names the function whose
			// closure-singleton this global.get fetches; r.Add is
			// the per-package closureCtx struct type index, which
			// the function's wasmgc.Table remap translates to the
			// module-global type index. (sym, globalCtxIx) is the
			// dedup key for getOrAllocSingleton — first occurrence
			// allocates a new wasm global; subsequent occurrences
			// reuse it.
			pkgIx := int(r.Add())
			if pkgIx < 0 || pkgIx >= len(remap) {
				ldr.Errorf(fn, "R_WASMCLOSURESINGLETON per-package index %d out of range for remap len %d", pkgIx, len(remap))
				continue
			}
			globalCtxIx := remap[pkgIx]
			gidx := m.getOrAllocSingleton(rs, globalCtxIx)
			writeUleb128(wfn, gidx)
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
// wasm3 has typed functions and host-managed stacks, so it needs only:
//
//	global 0 (i32, mutable): the linear-memory bump-allocator pointer
//	  (doc/wasm3-m2-design.md §12.6). Starts at runtime.end, fixed up
//	  below once the compiler emits the allocator.
//
//	global 1 (i64, mutable): CTXT — used by closure-with-captures
//	  calls to pass the captures pointer from the call site to the
//	  closure body. Stage G prerequisite.
//
//	global 2 (anyref, mutable): CTXT_REF — reserved for the
//	  captures-inside-closureCtx optimisation (see
//	  doc/wasm3-m3-captures-in-struct.md). When that optimisation
//	  lands, indirect-call sites set this to the closureCtx ref
//	  itself so closure-body prologues can `global.get; ref.cast;
//	  struct.get` their captures directly. Initialised to
//	  `ref.null any`; today no body reads or writes it, so it's a
//	  preparatory addition that costs ~3 bytes of module text and
//	  no instructions.
//
//	globals 3..N (typed-ref `(ref $closureCtx_T)`, immutable): one
//	  per (function symbol, closureCtx type) pair referenced via
//	  OpWasm3FuncValue. Initialised by a constant expression
//	  `(struct.new $closureCtx (ref.func $sym) (i64.const 0))` so
//	  bare-function references compile to `global.get` instead of a
//	  per-evaluation `struct.new`. The singletons are populated by
//	  m.getOrAllocSingleton as writeWasm3FuncBody walks each
//	  function's R_WASMCLOSURESINGLETON relocations (must run before
//	  this).
func writeGlobalSec3(ctxt *ld.Link, ldr *loader.Loader, m *wasm3Module, hostImportMap map[loader.Sym]int64) {
	sizeOffset := writeSecHeader(ctxt, sectionGlobal)
	nGlobals := uint64(3 + len(m.closureSingletonOrder))
	writeUleb128(ctxt.Out, nGlobals)
	// global 0: bump pointer (i32, mutable).
	ctxt.Out.WriteByte(I32)
	ctxt.Out.WriteByte(0x01) // mutable
	writeI32Const(ctxt.Out, 0)
	ctxt.Out.WriteByte(0x0b) // end
	// global 1: CTXT (i64, mutable).
	ctxt.Out.WriteByte(I64)
	ctxt.Out.WriteByte(0x01) // mutable
	ctxt.Out.WriteByte(0x42) // i64.const
	ctxt.Out.WriteByte(0x00) // value 0
	ctxt.Out.WriteByte(0x0b) // end
	// global 2: CTXT_REF (anyref, mutable), init ref.null any.
	// valtype 0x6E = anyref (wasm 3.0 reference-types proposal).
	ctxt.Out.WriteByte(0x6E)
	ctxt.Out.WriteByte(0x01) // mutable
	ctxt.Out.WriteByte(0xD0) // ref.null
	ctxt.Out.WriteByte(0x6E) // heaptype = any
	ctxt.Out.WriteByte(0x0b) // end
	// globals 3..N: closure singletons.
	for _, key := range m.closureSingletonOrder {
		// valtype = (ref null $closureCtx). The "null" form (0x63)
		// is required because struct.new's result is non-null but
		// global initializers accept either form; (ref null $t) is
		// the more permissive declaration. Encoding:
		//   0x63 <typeidx-sleb33>
		ctxt.Out.WriteByte(0x63) // ref null typeidx
		writeSleb128(ctxt.Out, int64(key.globalCtxIx))
		ctxt.Out.WriteByte(0x00) // immutable
		// init expression: struct.new $closureCtx
		//                     (ref.func $sym)
		//                     (i64.const 0)
		funcidx := int64(len(hostImportMap)) + ldr.SymValue(key.sym)>>16 - funcValueOffset
		ctxt.Out.WriteByte(0xD2) // ref.func
		writeUleb128(ctxt.Out, uint64(funcidx))
		ctxt.Out.WriteByte(0x42) // i64.const
		writeSleb128(ctxt.Out, 0)
		ctxt.Out.WriteByte(0xFB) // GC prefix
		ctxt.Out.WriteByte(0x00) // struct.new sub-opcode
		writeUleb128(ctxt.Out, uint64(key.globalCtxIx))
		ctxt.Out.WriteByte(0x0b) // end
	}
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
