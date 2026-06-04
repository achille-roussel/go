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
	"strings"
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
	closureSingletons map[wasm3SingletonKey]uint64
	boxedGlobals      map[loader.Sym]uint64
	descriptorGlobals map[loader.Sym]uint64
	// globalEntries is the unified, index-ordered list of dynamically
	// allocated wasm globals (closure singletons + boxed package-level
	// vars). They are assigned indices 3,4,... after the three fixed
	// globals (bump, CTXT, CTXT_REF) and share ONE index space because
	// both kinds are allocated interleaved during the reloc walk; a
	// per-kind counter would collide.
	globalEntries []wasm3GlobalEntry
}

// wasm3GlobalEntry is one dynamically-allocated wasm global.
type wasm3GlobalEntry struct {
	boxed      bool              // true: boxed package-level var
	descriptor bool              // true: type-descriptor identity ref
	key        wasm3SingletonKey // closure singleton (when !boxed && !descriptor)
	boxSym     loader.Sym        // boxed package-level var (when boxed)
	descSym    loader.Sym        // type-descriptor symbol (when descriptor)
	// wasm3 iface phase D step 3: a descriptor entry whose descSym
	// is an itab and whose compiler-side Wasm3ItabVtab aux is present
	// gets a typed (ref $go.vtab.<I>) global instead of the opaque
	// struct.new_default $go.object. itabVtabTypeIdx holds the
	// MODULE-global wasm type index of $go.vtab.<I> (already
	// remapped); itabMethodSyms is the ordered list of method LSyms
	// (each one a ref.func target for one vtab field, in I's method
	// order) extracted from the itab's Fun array relocations.
	itabVtabTypeIdx int
	itabMethodSyms  []loader.Sym
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
	idx := uint64(3 + len(m.globalEntries)) // 0=bump, 1=CTXT, 2=CTXT_REF, then dynamic globals
	m.closureSingletons[key] = idx
	m.globalEntries = append(m.globalEntries, wasm3GlobalEntry{key: key})
	return idx
}

// getOrAllocBoxedGlobal returns the wasm global index assigned to the
// boxed package-level variable sym, allocating it on first use. Shares
// the dynamic-global index space with closure singletons (see
// globalEntries). See R_WASMGLOBAL / doc/wasm3-pointer-cutover.
func (m *wasm3Module) getOrAllocBoxedGlobal(sym loader.Sym) uint64 {
	if m.boxedGlobals == nil {
		m.boxedGlobals = map[loader.Sym]uint64{}
	}
	if idx, ok := m.boxedGlobals[sym]; ok {
		return idx
	}
	idx := uint64(3 + len(m.globalEntries))
	m.boxedGlobals[sym] = idx
	m.globalEntries = append(m.globalEntries, wasm3GlobalEntry{boxed: true, boxSym: sym})
	return idx
}

// getOrAllocDescriptorGlobal returns the wasm global index assigned to
// the type-descriptor symbol sym's opaque WasmGC identity ref, allocating
// it on first use. Each distinct descriptor gets one immutable
// (global (ref $go.object) (struct.new_default $go.object)), so distinct
// types compare distinct under ref.eq. Shares the dynamic-global index
// space with closure singletons and boxed vars (see globalEntries).
// See R_WASMDESCRIPTOR / doc/wasm3-iface-boxing-derisk.wat.
//
// Phase D step 3 of the wasm3 iface migration: when sym is an itab
// data symbol whose compiler-side Wasm3ItabVtab aux is present, the
// allocation also stores the per-(I,T) vtab type index and the
// ordered method LSym list on the entry, so writeGlobalSec3 emits a
// typed (ref $go.vtab.<I>) global initialised with the per-method
// ref.func operands instead of the opaque struct.new_default.
func (m *wasm3Module) getOrAllocDescriptorGlobal(ldr *loader.Loader, sym loader.Sym) uint64 {
	if m.descriptorGlobals == nil {
		m.descriptorGlobals = map[loader.Sym]uint64{}
	}
	if idx, ok := m.descriptorGlobals[sym]; ok {
		return idx
	}
	idx := uint64(3 + len(m.globalEntries))
	m.descriptorGlobals[sym] = idx
	entry := wasm3GlobalEntry{descriptor: true, descSym: sym, itabVtabTypeIdx: -1}
	if ldr != nil && strings.HasPrefix(ldr.SymName(sym), "go:itab.") {
		m.populateItabEntry(ldr, sym, &entry)
	}
	m.globalEntries = append(m.globalEntries, entry)
	return idx
}

// populateItabEntry fills in entry.itabVtabTypeIdx and
// entry.itabMethodSyms when the itab data symbol carries the wasm3
// per-itab vtab side-table aux (see obj.Wasm3ItabVtab,
// goobj.AuxWasm3ItabVtab). Without the aux the entry stays in the
// legacy opaque-global mode and writeGlobalSec3 emits the
// struct.new_default $go.object fallback (matches a Phase D step 3
// build that hasn't been rebuilt against the new compiler yet).
//
// Method LSyms come from the aux symbol's own R_ADDR relocations,
// one per slot in $go.vtab.<I> (in I.AllMethods order); they live
// after the (VtabTypeIdx, Table) data payload so the linker can
// recover them without scanning the itab data symbol's body. The
// wasm3 backend stopped emitting that body — only the LSym (zero
// data, zero relocs) survives — so the legacy reloc-scan path
// would always return empty.
func (m *wasm3Module) populateItabEntry(ldr *loader.Loader, sym loader.Sym, entry *wasm3GlobalEntry) {
	auxSym := ldr.Wasm3ItabVtabSym(sym)
	if auxSym == 0 {
		return
	}
	// Method LSyms are R_ADDR relocations on the aux sym; their order
	// is preserved by the writer (relocations are sorted by offset).
	auxRelocs := ldr.Relocs(auxSym)
	type methReloc struct {
		off int32
		sym loader.Sym
	}
	var methRelocs []methReloc
	for ri := 0; ri < auxRelocs.Count(); ri++ {
		r := auxRelocs.At(ri)
		if !ldr.SymType(r.Sym()).IsText() {
			continue
		}
		methRelocs = append(methRelocs, methReloc{off: r.Off(), sym: r.Sym()})
	}
	sort.Slice(methRelocs, func(i, j int) bool { return methRelocs[i].off < methRelocs[j].off })

	// The aux's data payload is (8-byte VtabTypeIdx + Table bytes +
	// one ptr-sized zero slot per method reloc). Strip the trailing
	// method slots before parsing the table.
	var w obj.Wasm3ItabVtab
	w.Read(ldr.Data(auxSym), len(methRelocs))
	// Merge the producer-package's wasmgc.Table into the module-wide
	// table, remap the vtab type index to the module-global index.
	remap := m.mergeTable(w.Table)
	pkgIx := int(w.VtabTypeIdx)
	if pkgIx < 0 || pkgIx >= len(remap) {
		return
	}
	entry.itabVtabTypeIdx = remap[pkgIx]
	entry.itabMethodSyms = make([]loader.Sym, 0, len(methRelocs))
	for _, mr := range methRelocs {
		entry.itabMethodSyms = append(entry.itabMethodSyms, mr.sym)
	}
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
		// A WasmRef whose Offset is a fixed prelude type index (e.g.
		// TypeGoString for a string parameter of a go_runtime import)
		// needs no per-package remap — the prelude types occupy the same
		// fixed low indices in every module's table. Nullable, matching
		// the boxed field representation (a nil string is a null ref) and
		// the go_runtime.wat import signatures. Non-prelude indices in a
		// no-remap context remain an error: a program type can only be
		// referenced through objStorageRemapped's per-package table.
		if int(f.Offset) < wasmgc.NumPreludeTypes {
			return wasmgc.RefStorage(int(f.Offset), true)
		}
		panic("wasm3: WasmRef program-type index in a no-remap signature has no per-package table to remap against")
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
		// Check RefType == selfRefSentinel directly (not via IsRef):
		// the sentinel is negative, so IsRef() returns false for a
		// remapped self-ref. The Storage was a ref at input (otherwise
		// remapStorage would not have touched RefType); the sentinel
		// faithfully marks "ref pointing at the in-progress type."
		for _, f := range t.Fields {
			f.Storage = remapStorage(f.Storage, remap)
			if f.Storage.RefType == selfRefSentinel {
				hasSelfRef = true
			}
			remappedT.Fields = append(remappedT.Fields, f)
		}
		remappedT.Elem = remapStorage(t.Elem, remap)
		if remappedT.Elem.RefType == selfRefSentinel {
			hasSelfRef = true
		}
		for _, p := range t.Params {
			pr := remapStorage(p, remap)
			if pr.RefType == selfRefSentinel {
				hasSelfRef = true
			}
			remappedT.Params = append(remappedT.Params, pr)
		}
		for _, r := range t.Results {
			rr := remapStorage(r, remap)
			if rr.RefType == selfRefSentinel {
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
	// Match by RefType == sentinel directly (not IsRef()-gated): the
	// sentinel is negative, so IsRef() returns false and the gate
	// would skip the patch, leaving Storage.RefType = sentinel in the
	// final table — which the encoder reads as a non-ref Prim=0 (i8).
	for i := range t.Fields {
		if t.Fields[i].Storage.RefType == sentinel {
			t.Fields[i].Storage.RefType = real
		}
	}
	if t.Elem.RefType == sentinel {
		t.Elem.RefType = real
	}
	for i := range t.Params {
		if t.Params[i].RefType == sentinel {
			t.Params[i].RefType = real
		}
	}
	for i := range t.Results {
		if t.Results[i].RefType == sentinel {
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

	// Downgrade the prelude $go.cont type to a benign (func) if no
	// assembled function body actually contains a cont.new / resume
	// referencing it. The downgrade lets V8 (and other engines)
	// instantiate the module under --experimental-wasm-stack-switching
	// alone, dropping the --experimental-wasm-wasmfx requirement for
	// programs that never spawn a goroutine. Goroutine-spawning
	// programs leave the slot as (cont $go.goroutine.entry) and still
	// require wasmfx.
	if !wasm3BodiesUseCont(m.funcs) {
		m.table.DowngradeContToFunc()
	}

	writeTypeSec3(ctxt, m.table)
	writeImportSec3(ctxt, m.imports)
	writeFunctionSec3(ctxt, m.funcs)
	// Stage F: emit a funcref table covering every function in
	// the module, used by call_indirect at interface-method
	// dispatch sites. Without this, the obj-encoder bails on
	// ACALL TYPE_NONE (indirect calls with codeptr on the
	// wasm stack) and the calling function's body becomes an
	// unreachable stub. See doc/wasm3-m3-stage-f-interfaces.md.
	writeTableSec3(ctxt, len(m.imports), len(m.funcs))
	writeMemorySec(ctxt, ldr)
	writeTagSec3(ctxt)
	writeGlobalSec3(ctxt, ldr, m, hostImportMap)
	writeExportSec(ctxt, ldr, len(m.imports))
	refFns := collectWasm3RefFuncs(ctxt, ldr, len(m.imports))
	writeElementSec3(ctxt, refFns, len(m.imports), len(m.funcs))
	patchWasm3RunInit(ctxt, ldr, m, len(m.imports))
	writeCodeSec3(ctxt, m.funcs)
	writeDataSec(ctxt)
	writeProducerSec(ctxt)
	if !*ld.FlagS {
		writeNameSec3(ctxt, ldr, len(m.imports), m)
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

// writeElementSec3 emits the element section: an active segment
// populating table 0 with every native function, plus (when any
// ref.func opcodes were emitted) a passive-declared segment
// listing the explicitly-referenced funcidxs. The active segment
// is mandatory for Stage F's call_indirect-based interface
// dispatch path — without it the table reads return ref.null and
// every interface call would trap.
//
// Encoding for the active segment (flags=0x00):
//   0x00 <expr> <vec(funcidx)>
// where <expr> = i32.const 0 + end. The offset begins at table
// index 0; entry positions match wasm function indices. Imports
// occupy entries [0..numImports); they're initialised to ref.null
// funcref via a separate flags=0x05 passive-typed-elem segment
// rather than being placed in the table at module init, because
// imports can't appear in an active funcref segment unless they
// are first declared, which is itself the role of the declared
// segment below.
//
// For the declared segment (flags=0x03):
//   0x03 <elemkind=0x00> <vec(funcidx)>
func writeElementSec3(ctxt *ld.Link, declaredFuncs []uint64, numImports, numFns int) {
	sizeOffset := writeSecHeader(ctxt, sectionElement)
	nSegments := uint64(1) // active segment populating native funcs
	if len(declaredFuncs) > 0 {
		nSegments++
	}
	writeUleb128(ctxt.Out, nSegments)

	// Active segment: table 0, offset = funcValueOffset, vec =
	// every native function. Offset == funcValueOffset matches Go's
	// per-function symbol-value encoding (Value = (funcOrdinal +
	// funcValueOffset) << 16), so an interface-method codeptr
	// loaded from an itab + `>> 16` yields the table index
	// directly with no additional arithmetic. Table entries
	// [0, funcValueOffset) and [funcValueOffset+numFns, len) stay
	// ref.null funcref from the table's zero-initialisation —
	// they correspond to imports (never call_indirect'd today) and
	// out-of-range codeptrs (a runtime trap is the right behaviour
	// for those anyway).
	ctxt.Out.WriteByte(0x00) // flags: active, table 0, funcref
	writeI32Const(ctxt.Out, funcValueOffset)
	ctxt.Out.WriteByte(0x0b) // end of offset expression
	writeUleb128(ctxt.Out, uint64(numFns))
	for i := 0; i < numFns; i++ {
		writeUleb128(ctxt.Out, uint64(numImports+i))
	}

	if len(declaredFuncs) > 0 {
		// Passive-declared segment: validates ref.func operands.
		ctxt.Out.WriteByte(0x03) // flags: passive | declared
		ctxt.Out.WriteByte(0x00) // elemkind: funcref
		writeUleb128(ctxt.Out, uint64(len(declaredFuncs)))
		for _, idx := range declaredFuncs {
			writeUleb128(ctxt.Out, idx)
		}
	}

	writeSecSize(ctxt, sizeOffset)
}

// writeTableSec3 emits a single anyfunc (funcref) table sized to
// cover every function index in the module. Element 0 corresponds
// to import 0, element numImports corresponds to the first native
// function, etc. The element section then populates the native-
// function entries via an active segment (see writeElementSec3);
// import entries stay ref.null funcref unless a future stage
// declares them explicitly. Interface method dispatch reads the
// codeptr (an i64 holding the function's symbol address in
// wasm3's `funcidx << 16` encoding), shifts it down to the
// funcidx, and `call_indirect`s through this table.
func writeTableSec3(ctxt *ld.Link, numImports, numFns int) {
	sizeOffset := writeSecHeader(ctxt, sectionTable)
	writeUleb128(ctxt.Out, 1) // number of tables
	ctxt.Out.WriteByte(0x70)  // elemtype: anyfunc (funcref)
	ctxt.Out.WriteByte(0x00)  // no max
	writeUleb128(ctxt.Out, uint64(funcValueOffset+numFns))
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
			// sleb128) — wasm type indices in struct.get / array.* /
			// call_indirect positions are unsigned in the binary
			// format. For heap-type positions (ref.cast / ref.test /
			// ref.null), use R_WASMHEAPTYPE instead.
			pkgIx := int(r.Add())
			if pkgIx < 0 || pkgIx >= len(remap) {
				ldr.Errorf(fn, "R_WASMTYPE per-package index %d out of range for remap len %d", pkgIx, len(remap))
				continue
			}
			writeUleb128(wfn, uint64(remap[pkgIx]))
		case objabi.R_WASMHEAPTYPE:
			// Heap-type immediate (ref.cast / ref.test / ref.null):
			// signed LEB128 (s33). The spec shares the encoding with
			// abstract heap types (anyref = -18 = 0x6E, etc.), so
			// typeidx must always go through sleb. For typeidx >= 64,
			// uleb and sleb diverge: uleb's single byte 0x40 would be
			// decoded by a sleb-reading validator as -64 (void block
			// type), the "invalid heap type" instantiation error.
			pkgIx := int(r.Add())
			if pkgIx < 0 || pkgIx >= len(remap) {
				ldr.Errorf(fn, "R_WASMHEAPTYPE per-package index %d out of range for remap len %d", pkgIx, len(remap))
				continue
			}
			writeSleb128(wfn, int64(remap[pkgIx]))
		case objabi.R_WASMGLOBAL:
			// Boxed package-level variable: r.Sym names the variable.
			// Allocate (or reuse) one wasm anyref ref-global for it and
			// patch the global.get/global.set operand to its index.
			writeUleb128(wfn, m.getOrAllocBoxedGlobal(rs))
		case objabi.R_WASMDESCRIPTOR:
			// Type-descriptor identity ref: r.Sym names the descriptor.
			// Allocate (or reuse) one struct.new_default $go.object
			// identity global and patch the global.get operand to it.
			// (For itab descriptor symbols carrying the wasm3
			// per-itab vtab side-table aux, the allocator branches
			// to a typed (ref $go.vtab.<I>) global — see
			// getOrAllocDescriptorGlobal.)
			writeUleb128(wfn, m.getOrAllocDescriptorGlobal(ldr, rs))
		default:
			ldr.Errorf(fn, "bad reloc type %d for wasm3", r.Type())
		}
	}
	wfn.Write(P[off:])
}

// Wasm3TagIndexPark is the tag index of the single M4 "goroutine park"
// tag declared by writeTagSec3. Every suspend that hands control back to
// the goroutine scheduler uses this tag; every cont-running resume site
// installs a handler for it.
const Wasm3TagIndexPark = 0

// writeTagSec3 writes the WebAssembly 3.0 tag section for GOARCH=wasm3.
// One tag is declared today (Wasm3TagIndexPark), typed `(tag (param))`
// — i.e. carrying no payload. M4 Phase 3 uses it for gopark/goready;
// M5 (panic) will add an exception tag carrying the panic value alongside.
//
// Wasm tag declaration:
//
//	tagsec := vec(tag)
//	tag    := byte typeidx   ; byte=0 (exception attribute)
//
// The typeidx points at a (func) type — for stack-switching tags, the
// "function" describes the tag's payload shape. We reuse
// TypeGoGoroutineEntry, which is the prelude (func) — no params,
// matching our zero-payload tag.
func writeTagSec3(ctxt *ld.Link) {
	sizeOffset := writeSecHeader(ctxt, sectionTag)
	writeUleb128(ctxt.Out, 1) // tag count
	// tag 0 (Wasm3TagIndexPark): attribute=0 (exception), typeidx=TypeGoGoroutineEntry.
	ctxt.Out.WriteByte(0x00)
	writeUleb128(ctxt.Out, uint64(wasmgc.TypeGoGoroutineEntry))
	writeSecSize(ctxt, sizeOffset)
}

// wasm3BodiesUseCont reports whether any assembled function body
// contains the OpWasm3RunInCont* signature {cont.new $go.cont,
// resume $go.cont}. The wasm3 SSA backend always emits those two
// opcodes back-to-back with the same prelude-type immediate, so a
// 4-byte signature ({AContNew, idx, AResume, idx}) is enough to
// distinguish "this program spawns goroutines" from "this program
// only ever runs main.main". The result drives whether the prelude
// $go.cont type is emitted as (cont $go.goroutine.entry) — required
// by V8 only when actually used and gated on
// --experimental-wasm-wasmfx — or downgraded to a benign (func) so
// JSPI alone (--experimental-wasm-stack-switching) suffices.
func wasm3BodiesUseCont(fns []*wasm3Func) bool {
	idx := wasmgc.ContTypeIndexByte()
	sig := []byte{0xE0, idx, 0xE3, idx}
	for _, fn := range fns {
		if bytes.Contains(fn.Code, sig) {
			return true
		}
	}
	return false
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
	nGlobals := uint64(3 + len(m.globalEntries))
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
	// globals 3..N: dynamically allocated globals, in index order
	// (closure singletons and boxed package-level vars, interleaved).
	for _, e := range m.globalEntries {
		if e.boxed {
			// Boxed package-level variable: (global (mut anyref)).
			// Using anyref (not the precise (ref null $T)) keeps the
			// linker from needing the variable's Go type. The init
			// expression depends on the variable's nature:
			//
			//   - String literal (go:string."<bytes>"): emit a
			//     struct.new $go.string with array.new_fixed of the
			//     literal bytes. No runtime init needed.
			//   - Anything else: ref.null any; the package init
			//     function constructs the real value at startup.
			ctxt.Out.WriteByte(0x6E) // anyref
			ctxt.Out.WriteByte(0x01) // mutable
			// Recognise a string-header static temp: 16 bytes of data
			// (data ptr + length) with a single R_ADDR reloc at offset
			// 0 pointing to a go:string."..." symbol whose data is the
			// raw string bytes. Emit a struct.new $go.string init
			// expression that materialises the WasmGC string at module
			// load time. Anything else stays ref.null any; the package
			// init function (called from wasm3RunInit) fills it in.
			stringBytes, isString := wasm3DecodeStringTemp(ldr, ldr.Data(e.boxSym), ldr.Relocs(e.boxSym))
			if isString {
				// init: <i32.const byte>...;
				//       array.new_fixed $go.bytes <len>;
				//       i32.const 0; i32.const <len>;
				//       struct.new $go.string
				//
				// $go.string is (anyref backing, i32 offset, i32 length)
				// after the slice/string-header i32-ification (commit
				// for #164), so offset/length must be i32.const in the
				// const-expression initializer — a function body can
				// follow an i64.const with i32.wrap_i64 to bridge, but
				// const-exprs accept only constants and global.get, so
				// the value must already be the right type.
				for _, b := range stringBytes {
					ctxt.Out.WriteByte(0x41) // i32.const
					writeSleb128(ctxt.Out, int64(b))
				}
				ctxt.Out.WriteByte(0xFB) // GC prefix
				ctxt.Out.WriteByte(0x08) // array.new_fixed sub-opcode
				writeUleb128(ctxt.Out, uint64(wasmgc.TypeGoBytes))
				writeUleb128(ctxt.Out, uint64(len(stringBytes)))
				ctxt.Out.WriteByte(0x41) // i32.const (offset)
				writeSleb128(ctxt.Out, 0)
				ctxt.Out.WriteByte(0x41) // i32.const (length)
				writeSleb128(ctxt.Out, int64(len(stringBytes)))
				ctxt.Out.WriteByte(0xFB) // GC prefix
				ctxt.Out.WriteByte(0x00) // struct.new sub-opcode
				writeUleb128(ctxt.Out, uint64(wasmgc.TypeGoString))
			} else {
				ctxt.Out.WriteByte(0xD0) // ref.null
				ctxt.Out.WriteByte(0x6E) // heaptype = any
			}
			ctxt.Out.WriteByte(0x0b) // end
			continue
		}
		if e.descriptor {
			// Phase D step 3: an itab descriptor whose compiler-side
			// Wasm3ItabVtab aux is present gets a typed
			// (ref $go.vtab.<I>) global initialised with the per-(I,T)
			// method funcrefs, so the dispatch trampoline's
			// ref.cast (ref $go.vtab.<I>) on the iface's itab slot
			// succeeds at runtime.
			//
			// Init expression shape (struct.new $go.vtab.<I>):
			//   ;; field 0: type-descriptor identity ref
			//   struct.new_default $go.object
			//   ;; fields 1..N: per-method funcrefs, ordered by
			//   ;; the itab's Fun array (which matches I.AllMethods)
			//   ref.func $T.M0
			//   ref.func $T.M1
			//   ...
			//   ;; pack into the vtab struct
			//   struct.new $go.vtab.<I>
			//
			// The field-0 type identity is a fresh struct.new_default
			// $go.object; using a separate per-(I,T) ref per itab is
			// fine because two itabs over the same (I,T) pair go
			// through ITabLsym's CONTENT-ADDRESSABLE dedup at the
			// compiler side, so we only ever emit one global per
			// distinct (I,T). Type-identity comparisons happen
			// against the vtab pointer itself (the iface's itab
			// slot), not against the inner field-0 ref, so the
			// per-itab fresh field-0 ref is benign.
			if e.itabVtabTypeIdx >= 0 {
				// valtype = (ref $go.vtab.<I>)
				ctxt.Out.WriteByte(0x64) // ref (non-null) prefix
				writeSleb128(ctxt.Out, int64(e.itabVtabTypeIdx))
				ctxt.Out.WriteByte(0x00) // immutable
				// init: struct.new_default $go.object  (field 0)
				ctxt.Out.WriteByte(0xFB) // GC prefix
				ctxt.Out.WriteByte(0x01) // struct.new_default sub-opcode
				writeUleb128(ctxt.Out, uint64(wasmgc.TypeGoObject))
				// init: ref.func $T.Mi  (fields 1..N)
				for _, msym := range e.itabMethodSyms {
					// ref.func operand = module-global funcidx,
					// uleb128. Same encoding as R_WASMREFFUNC.
					funcidx := int64(len(hostImportMap)) + ldr.SymValue(msym)>>16 - funcValueOffset
					ctxt.Out.WriteByte(0xD2) // ref.func
					writeUleb128(ctxt.Out, uint64(funcidx))
				}
				// struct.new $go.vtab.<I>
				ctxt.Out.WriteByte(0xFB) // GC prefix
				ctxt.Out.WriteByte(0x00) // struct.new sub-opcode
				writeUleb128(ctxt.Out, uint64(e.itabVtabTypeIdx))
				ctxt.Out.WriteByte(0x0b) // end
				continue
			}
			// Legacy opaque descriptor ref: an immutable
			// (global (ref $go.object) (struct.new_default $go.object)).
			// The exact-heap-type prefix (0x62) was dropped to match
			// wasmfxtime; see typesec.go heaptype helper for rationale.
			// Encoding:  0x64 (ref prefix, non-null) <typeidx>
			ctxt.Out.WriteByte(0x64) // ref (non-null) prefix
			writeSleb128(ctxt.Out, int64(wasmgc.TypeGoObject))
			ctxt.Out.WriteByte(0x00) // immutable
			ctxt.Out.WriteByte(0xFB) // GC prefix
			ctxt.Out.WriteByte(0x01) // struct.new_default sub-opcode
			writeUleb128(ctxt.Out, uint64(wasmgc.TypeGoObject))
			ctxt.Out.WriteByte(0x0b) // end
			continue
		}
		key := e.key
		// valtype = (ref $closureCtx). Exact prefix dropped — see
		// typesec.go heaptype helper for rationale.
		// Encoding:  0x64 (ref) <typeidx>
		ctxt.Out.WriteByte(0x64) // ref (non-null) prefix
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

// writeNameSec3 writes the name custom section for wasm3. Subsections:
//   - 0x01 function names: from m.imports + m.funcs.
//   - 0x04 type names (wasm GC proposal extension): from m.table[i].Name —
//     these are the human-readable Go-derived labels the type collector
//     assigned ("go.string", "go.slice.<T>", "main.Box", ...).
//   - 0x07 global names: derived from m.globalEntries — closure singletons
//     get the producing function name with a ".closure" suffix; boxed
//     package-level vars use the symbol name; descriptor / itab-vtab
//     entries name themselves after the descriptor symbol.
//
// Field names (subsection 0x0A) are a future refinement — wasmgc.Field
// does not yet carry a Name. The fixed bump / CTXT / CTXT_REF globals
// get explicit names too.
func writeNameSec3(ctxt *ld.Link, ldr *loader.Loader, firstFnIndex int, m *wasm3Module) {
	sizeOffset := writeSecHeader(ctxt, sectionCustom)
	writeName(ctxt.Out, "name")

	// 0x01 function names — imports first (indices 0..len(m.imports)-1),
	// then defined functions (indices firstFnIndex..).
	sizeOffset2 := writeSecHeader(ctxt, 0x01)
	writeUleb128(ctxt.Out, uint64(len(m.imports)+len(m.funcs)))
	for i, fn := range m.imports {
		writeUleb128(ctxt.Out, uint64(i))
		writeName(ctxt.Out, fn.Module+"."+fn.Name)
	}
	for i, fn := range m.funcs {
		writeUleb128(ctxt.Out, uint64(firstFnIndex+i))
		writeName(ctxt.Out, fn.Name)
	}
	writeSecSize(ctxt, sizeOffset2)

	// 0x04 type names.
	sizeOffset2 = writeSecHeader(ctxt, 0x04)
	named := 0
	for _, t := range m.table {
		if t.Name != "" {
			named++
		}
	}
	writeUleb128(ctxt.Out, uint64(named))
	for i, t := range m.table {
		if t.Name == "" {
			continue
		}
		writeUleb128(ctxt.Out, uint64(i))
		writeName(ctxt.Out, t.Name)
	}
	writeSecSize(ctxt, sizeOffset2)

	// 0x07 global names. Three fixed leading globals + per-entry names.
	sizeOffset2 = writeSecHeader(ctxt, 0x07)
	writeUleb128(ctxt.Out, uint64(3+len(m.globalEntries)))
	writeUleb128(ctxt.Out, 0)
	writeName(ctxt.Out, "runtime.bumpPtr")
	writeUleb128(ctxt.Out, 1)
	writeName(ctxt.Out, "runtime.CTXT")
	writeUleb128(ctxt.Out, 2)
	writeName(ctxt.Out, "runtime.CTXT_REF")
	for i, e := range m.globalEntries {
		idx := uint64(3 + i)
		writeUleb128(ctxt.Out, idx)
		var name string
		switch {
		case e.boxed:
			name = "var." + ldr.SymName(e.boxSym)
		case e.descriptor:
			if e.itabVtabTypeIdx != 0 {
				name = "vtab." + ldr.SymName(e.descSym)
			} else {
				name = "desc." + ldr.SymName(e.descSym)
			}
		default:
			name = "closure." + ldr.SymName(e.key.sym)
		}
		writeName(ctxt.Out, name)
	}
	writeSecSize(ctxt, sizeOffset2)

	writeSecSize(ctxt, sizeOffset)
}

// wasm3DecodeStringTemp recognises a string-header static temp
// (compiler-generated for an inline string literal that escapes to a
// boxed global): 16 bytes of data shaped as (data ptr, length) with
// a single R_ADDR reloc at offset 0 pointing to a go:string."..."
// symbol whose data is the raw string bytes. If matched, returns the
// raw bytes; otherwise returns ok=false. On wasm3 the temp's storage
// is a WasmGC anyref global, not 16 bytes of linear memory, so the
// header bytes never become accessible at runtime — the caller emits
// a struct.new $go.string init expression instead.
func wasm3DecodeStringTemp(ldr *loader.Loader, data []byte, rels loader.Relocs) ([]byte, bool) {
	if len(data) != 16 || rels.Count() != 1 {
		return nil, false
	}
	r := rels.At(0)
	if r.Type() != objabi.R_ADDR || r.Off() != 0 {
		return nil, false
	}
	bytesSym := r.Sym()
	if !strings.HasPrefix(ldr.SymName(bytesSym), "go:string.") {
		return nil, false
	}
	// length lives in bytes 8..16 (little-endian uint64).
	var length uint64
	for i := 0; i < 8; i++ {
		length |= uint64(data[8+i]) << (8 * i)
	}
	stringBytes := ldr.Data(bytesSym)
	if uint64(len(stringBytes)) < length {
		return nil, false
	}
	return stringBytes[:length], true
}

// patchWasm3RunInit overwrites the runtime.wasm3RunInit function body
// with a sequence of direct `call $init` instructions, one per init
// function the standard inittask machinery scheduled. The runtime's
// _rt0_wasm3_js calls wasm3RunInit before main_main so package-level
// var initialisers and explicit init() functions actually run on
// js/wasm3.
//
// The standard runtime.doInit path walks initTask records and reads
// each task's function pointers as uintptrs that get cast to func()
// — incompatible with wasm3's WasmGC closure-ref representation of
// func values. Materialising the call schedule statically at link
// time as wasm `call` instructions sidesteps that mismatch.
func patchWasm3RunInit(ctxt *ld.Link, ldr *loader.Loader, m *wasm3Module, numImports int) {
	// Find the wasm3RunInit slot in m.funcs.
	slot := -1
	for i, fn := range m.funcs {
		if fn.Name == "runtime.wasm3RunInit" {
			slot = i
			break
		}
	}
	if slot < 0 {
		return
	}

	// Collect init function symbols in dependency order. Walk both
	// the runtime's own inittasks (go:runtime.inittasks) and the
	// user's (go:main.inittasks). Both are slices of *initTask whose
	// R_ADDR relocs point to per-package inittask records, in
	// schedule order. Each inittask record's R_ADDR relocs then
	// point to its init functions.
	var initFuncs []loader.Sym
	schedSyms := []loader.Sym{ctxt.MainInittasks()}
	if s := ldr.Lookup("runtime.runtime_inittasks", 0); s != 0 {
		// runtime.runtime_inittasks is a slice header (ptr,len,cap) whose
		// first 8 bytes hold an R_ADDR reloc pointing to the schedule
		// symbol go:runtime.inittasks. Resolve via its single relocation.
		rt := ldr.Relocs(s)
		for ri := 0; ri < rt.Count(); ri++ {
			r := rt.At(ri)
			if r.Type() == objabi.R_ADDR {
				schedSyms = append([]loader.Sym{r.Sym()}, schedSyms...)
				break
			}
		}
	}
	for _, schedSym := range schedSyms {
		if schedSym == 0 {
			continue
		}
		schedRelocs := ldr.Relocs(schedSym)
		for ri := 0; ri < schedRelocs.Count(); ri++ {
			r := schedRelocs.At(ri)
			if r.Type() != objabi.R_ADDR {
				continue
			}
			taskSym := r.Sym()
			taskRelocs := ldr.Relocs(taskSym)
			for tri := 0; tri < taskRelocs.Count(); tri++ {
				tr := taskRelocs.At(tri)
				if tr.Type() != objabi.R_ADDR {
					continue
				}
				fnSym := tr.Sym()
				// Skip elided/empty init stubs (val=0 = no body). Some
				// runtime.init.N entries are present in the inittask
				// list but never get a function body on wasm3 (the
				// associated subsystem is excluded entirely or stubbed
				// out — e.g., GC-related inits never reach a wasm
				// function). Calling them would land on a bogus index.
				if ldr.SymValue(fnSym) == 0 {
					continue
				}
				initFuncs = append(initFuncs, fnSym)
			}
		}
	}

	// Emit the body: local-decl-count=0, then `call <funcidx>` per
	// init function, then end.
	w := new(bytes.Buffer)
	writeUleb128(w, 0) // local declaration count
	for _, sym := range initFuncs {
		funcidx := uint64(int64(numImports) + ldr.SymValue(sym)>>16 - funcValueOffset)
		w.WriteByte(0x10) // call
		writeUleb128(w, funcidx)
	}
	w.WriteByte(0x0b) // end
	m.funcs[slot].Code = w.Bytes()
}
