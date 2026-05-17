// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm3

import (
	"bytes"
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/wasmgc"
	"sync"
)

// wasmabi.go lowers Go function signatures to the typed-function
// calling convention GOARCH=wasm3 uses. Unlike GOARCH=wasm, a wasm3
// function is an ordinary typed wasm function: Go parameters and
// results map directly to wasm function parameters and results, with no
// Go-stack-in-linear-memory frame, no PC_B block-id parameter, and no
// unwind-flag result. See doc/wasm3-m2-design.md §2 and §2.1.
//
// The signature is carried to the linker as an obj.WasmFuncType — the
// same per-function aux mechanism //go:wasmimport and //go:wasmexport
// already use — so no new abi.ABIConfig is needed. The only extension
// is obj.WasmRef, a reference field whose referenced wasm type index is
// stored in obj.WasmField.Offset (the field is otherwise the memory-ABI
// frame offset, which is meaningless for the typed ABI).

// objField converts one wasm GC field to the obj.WasmField the linker
// reads. A reference field carries its referenced wasm type index in
// Offset; a primitive field carries a value type. The packed i8/i16
// storage types are valid only inside arrays and structs, never as a
// function parameter or result, so they panic here.
func objField(f wasmgc.Field) obj.WasmField {
	if f.Storage.IsRef() {
		return obj.WasmField{Type: obj.WasmRef, Offset: int64(f.Storage.RefType)}
	}
	switch f.Storage.Prim {
	case wasmgc.I32:
		return obj.WasmField{Type: obj.WasmI32}
	case wasmgc.I64:
		return obj.WasmField{Type: obj.WasmI64}
	case wasmgc.F32:
		return obj.WasmField{Type: obj.WasmF32}
	case wasmgc.F64:
		return obj.WasmField{Type: obj.WasmF64}
	}
	panic("wasm3: packed i8/i16 storage is not valid in a function signature")
}

// objFields converts a slice of wasm GC fields to obj.WasmFields.
func objFields(fields []wasmgc.Field) []obj.WasmField {
	if len(fields) == 0 {
		return nil
	}
	out := make([]obj.WasmField, len(fields))
	for i, f := range fields {
		out[i] = objField(f)
	}
	return out
}

// loweredSignature lowers a Go function type to the typed wasm function
// signature wasm3 emits. Each Go parameter and result is exploded into
// one or more wasm value slots following the object model (rule 6 of
// doc/wasm3-design.md §6): a scalar is one slot, a pointer is one
// reference slot, a string is three slots (backing, offset, length), a
// struct value flattens into its fields' slots. A receiver, if present,
// becomes the first parameter.
//
// The collector is updated in place with every GC type the signature
// references, so its type table must be emitted alongside the function.
func (c *typeCollector) loweredSignature(ft *types.Type) obj.WasmFuncType {
	if ft.Kind() != types.TFUNC {
		panic("wasm3: loweredSignature on non-function type " + ft.Kind().String())
	}
	var sig obj.WasmFuncType
	for _, p := range ft.RecvParams() {
		sig.Params = append(sig.Params, objFields(c.lowerFields(p.Type))...)
	}
	for _, r := range ft.Results() {
		sig.Results = append(sig.Results, objFields(c.lowerFields(r.Type))...)
	}
	return sig
}

// loweredStorages lowers a Go function type's parameters and results to
// the wasm reference/primitive storage slots of a wasmgc func type. It
// is loweredSignature expressed in the shared wasmgc model rather than
// obj.WasmField, used by collectSignature to build the KindFunc table
// entry the linker needs.
func (c *typeCollector) loweredStorages(ft *types.Type) (params, results []wasmgc.Storage) {
	for _, p := range ft.RecvParams() {
		for _, f := range c.lowerFields(p.Type) {
			params = append(params, f.Storage)
		}
	}
	for _, r := range ft.Results() {
		for _, f := range c.lowerFields(r.Type) {
			results = append(results, f.Storage)
		}
	}
	return params, results
}

// attachWasmType attaches fn's typed-ABI signature to its LSym as an
// obj.WasmType aux symbol, so the linker (cmd/link/internal/wasm's
// asm3.go) declares the function with its real wasm signature instead
// of the degenerate ()->(). It is wired in as ssagen.Arch.PrepareFunc
// and runs once per function, after genssa and before the obj backend.
//
// Stage C.2: only signatures whose every parameter and result is an
// integer-class Go scalar are attached. Each slot lowers to the wasm
// integer type of its width — i32 for the sub-word kinds (int32, byte,
// bool, …), i64 for the 64-bit kinds (int, int64, uint, uintptr). This
// is the width-faithful lowering: the wasm signature mirrors the Go
// types. The SSA backend works in i64 GP "registers", so the obj
// backend widens a narrow parameter (i64.extend_i32_u) on entry and
// narrows a narrow result (i32.wrap_i64) at return; the conversions
// live at the ABI boundary, not in the function body.
//
// Floating-point parameters are not attached yet: wasm has distinct
// f32 and f64 register classes, which the single flat wasm3 float
// parameter-register list does not yet distinguish. Pointers, strings,
// slices and other composites need the per-package wasmgc.Table the
// compiler does not emit yet. A function with any such parameter or
// result is left without an aux and falls back to ()->() with an
// unreachable stub body. See doc/wasm3-m2-cutover-notes.md §5.
func attachWasmType(fn *ir.Func) {
	ft := fn.Type()
	if ft == nil || ft.Kind() != types.TFUNC {
		return
	}
	// //go:wasmimport stubs have no Go body — the linker fabricates the
	// import call site from the WasmImport aux. Attach nothing.
	//
	// //go:wasmexport: the *wrapper* (a separate LSym created by
	// GenWasmExportWrapper) carries the export signature in its
	// WasmExport aux and is built directly by assembleWasm3ExportWrapper
	// — attachWasmType is never called for it. The *wrapped* function
	// (the user's Go func) still needs its own typed signature though,
	// since the wrapper's body calls into it: it is an ordinary wasm3
	// function and must be lowered to a typed wasm function in its own
	// right. So fn.WasmExport != nil (set on the wrapped fn by the
	// pragma parser) is *not* a skip condition here — only WasmImport is.
	if fn.WasmImport != nil {
		return
	}

	// Two-tier signature lowering. Try the legacy wasm3Fields path
	// first: it produces the simpler (i64 ptr, i64 len)-style lowering
	// the wasmexport wrapper today knows how to bridge to host i32
	// pointers, and covers every primitive plus string. Only when
	// wasm3Fields rejects a parameter — typically a struct, slice,
	// interface, or other composite — fall through to the typeCollector
	// path which emits a per-function wasmgc.Table and reference-typed
	// wasm fields per the §6 object model. Per-register narrowness at
	// the call site (RegisterTypesAndOffsets) keeps the wider field-
	// level wasm signature in sync with what the caller pushes; the
	// per-package table travels via WasmType.Table to the linker which
	// merges it into the module-wide table.
	//
	// A wasmexport function with a composite parameter still won't have
	// a working host call path — paramsToWasmFields and the wrapper
	// don't know how to marshal between host and GC representations of
	// composites — but every internal call between wasm3 functions
	// works.
	if sig, ok := tryPrimitiveAttach(ft); ok {
		fn.LSym.Func().WasmType = &obj.WasmType{WasmFuncType: sig}
		return
	}
	if wt, c, ok := tryCollectorAttach(ft); ok {
		fn.LSym.Func().WasmType = wt
		// Hand the live collector to wasm3LiveCollector so codegen
		// (ssaGenValue → wasm3RegisterStruct) can extend the table
		// with body-emitted types and reserialise wt.Table.
		wasm3StashCollector(fn.LSym.Func(), c)
		return
	}
	// Fallback: a flat primitive lowering that walks each Go param /
	// result recursively, flattening structs / arrays into the i64/i32/
	// f32/f64 register slots the SSA layer actually pushes. This gives
	// every function a signature that lines up with its call sites even
	// when neither tryPrimitiveAttach nor tryCollectorAttach can produce
	// a precise ref-typed lowering — the function body might still fall
	// back to the obj backend's stub (unreachable), but at least its
	// signature matches the caller's wasm stack and the module validates.
	//
	// Without this fallback, attachWasmType would leave the function with
	// no aux and the linker would default to ()->(). Callers compiled
	// against the original Go signature would push args / pop returns,
	// causing wasm validation to reject the module.
	if sig, ok := tryFlatPrimitiveAttach(ft); ok {
		fn.LSym.Func().WasmType = &obj.WasmType{WasmFuncType: sig}
		return
	}
}

// tryFlatPrimitiveAttach lowers a function signature by walking each
// parameter / result recursively, flattening composites (structs,
// arrays) into their leaf primitive types. Each leaf becomes one wasm
// field (i64 / i32 / f64 / f32 per width and integer/float category);
// pointer-shaped leaves lower to i64 (matching what the SSA layer
// pushes for register-resident pointer values, not the ref-typed wasm
// field a precise lowering would emit). Returns false if any leaf is
// itself a composite the flat lowering can't handle — currently slices
// and interfaces (their wasm-field shape's 4-/2- field count doesn't
// match Go's regabi 3-/2- register shape, so the call site would still
// mismatch).
func tryFlatPrimitiveAttach(ft *types.Type) (obj.WasmFuncType, bool) {
	var sig obj.WasmFuncType
	for _, p := range ft.RecvParams() {
		fs, ok := flatPrimitiveFields(p.Type)
		if !ok {
			return obj.WasmFuncType{}, false
		}
		sig.Params = append(sig.Params, fs...)
	}
	for _, r := range ft.Results() {
		fs, ok := flatPrimitiveFields(r.Type)
		if !ok {
			return obj.WasmFuncType{}, false
		}
		sig.Results = append(sig.Results, fs...)
	}
	return sig, true
}

func flatPrimitiveFields(t *types.Type) ([]obj.WasmField, bool) {
	if f, ok := wasm3IntField(t); ok {
		return []obj.WasmField{f}, true
	}
	switch t.Kind() {
	case types.TSTRING:
		// (data *byte, len int) — both pass as i64 registers.
		return []obj.WasmField{
			{Type: obj.WasmI64},
			{Type: obj.WasmI64},
		}, true
	case types.TSLICE:
		// (data *Elem, len int, cap int) — three i64 registers.
		// Stage E proper boxes the backing in a wasmgc ref instead
		// of a linear-memory pointer; until then the regabi shape
		// is the SSA-pushed shape and this flat lowering matches.
		return []obj.WasmField{
			{Type: obj.WasmI64},
			{Type: obj.WasmI64},
			{Type: obj.WasmI64},
		}, true
	case types.TINTER:
		// (type *_type, data unsafe.Pointer) — two i64 registers.
		return []obj.WasmField{
			{Type: obj.WasmI64},
			{Type: obj.WasmI64},
		}, true
	case types.TCOMPLEX64:
		return []obj.WasmField{
			{Type: obj.WasmF32},
			{Type: obj.WasmF32},
		}, true
	case types.TCOMPLEX128:
		return []obj.WasmField{
			{Type: obj.WasmF64},
			{Type: obj.WasmF64},
		}, true
	case types.TSTRUCT:
		var out []obj.WasmField
		for _, f := range t.Fields() {
			fs, ok := flatPrimitiveFields(f.Type)
			if !ok {
				return nil, false
			}
			out = append(out, fs...)
		}
		return out, true
	case types.TARRAY:
		// Empty arrays produce no fields and the per-call-site
		// register count is also 0 — matches by being empty on both
		// sides. A non-empty array exceeds the per-register flatten
		// budget for the typical regabi, so we leave it to the
		// collector path (which boxes it as a single ref field).
		if t.NumElem() == 0 {
			return nil, true
		}
		return nil, false
	case types.TMAP, types.TCHAN, types.TFUNC:
		// Pointer-shaped runtime types: one i64 register at the SSA
		// layer until the M3/M4 lowering refines them.
		return []obj.WasmField{{Type: obj.WasmI64}}, true
	}
	return nil, false
}

// tryPrimitiveAttach is the legacy lowering: every Go param/result is
// run through wasm3Fields, which handles primitives and strings (split
// to two i64 slots). Returns ok=false on any type wasm3Fields rejects,
// so the caller can fall through to the typeCollector path.
func tryPrimitiveAttach(ft *types.Type) (obj.WasmFuncType, bool) {
	var sig obj.WasmFuncType
	for _, p := range ft.RecvParams() {
		fs, ok := wasm3Fields(p.Type)
		if !ok {
			return obj.WasmFuncType{}, false
		}
		sig.Params = append(sig.Params, fs...)
	}
	for _, r := range ft.Results() {
		fs, ok := wasm3Fields(r.Type)
		if !ok {
			return obj.WasmFuncType{}, false
		}
		sig.Results = append(sig.Results, fs...)
	}
	return sig, true
}

// tryCollectorAttach lowers a Go function type via a per-function
// typeCollector and returns the resulting WasmType (with the collector's
// table serialized into wt.Table). Returns ok=false if the lowering
// recovers a panic — typeCollector.lowerFields panics on Go types it
// can't yet represent, and we want to fall back to the primitive-only
// path rather than abort compilation.
//
// The typeCollector is also rejected when a parameter or result type
// would lower to a wasm field shape that doesn't match Go's register
// ABI: the SSA call site pushes one register per Go-ABI register, and
// we only know how to bridge composites whose collector lowering has
// the same number of fields as the regabi has registers. Today that
// rules out slices (collector: ref+3 i32; regabi: ptr+2 ints — 4 vs
// 3) and interfaces (collector: 2 refs; regabi: 2 ints — same count
// but ref/i64 type mismatch). Structs by value generally match because
// both lowerings flatten field-by-field.
// wasm3LiveCollector maps the FuncInfo of every wasm3 function under
// compilation to the typeCollector that built its signature table.
// Codegen (ssaGenValue) uses it to register additional types
// referenced by struct.new / struct.get / etc. that aren't part of
// the function's signature, and reserialise the resulting table back
// to fi.WasmType.Table so the linker sees the union.
//
// Stash is keyed on the FuncInfo pointer (the *obj.FuncInfo is
// reachable from ssagen.State via FuncInfo()); entries live for the
// lifetime of the compilation. Cleanup-on-finish isn't critical
// because the compiler is short-lived, but a future maybe-leak-fix
// could clear each entry in a PostGenssa hook.
var wasm3LiveCollector sync.Map // map[*obj.FuncInfo]*typeCollector

// wasm3RegisterStruct registers the Go struct type t with fi's
// per-function wasmgc.Table, returning its module-internal type
// index. Idempotent — repeated calls for the same t return the same
// index. Used by wasm3 SSA codegen to plumb R_WASMTYPE relocations
// on body-emitted GC ops (struct.new, struct.get, ref.cast, ...) the
// way the signature lowering already plumbs them for the WasmType
// signature's ref fields.
//
// If fi has no live collector (typically a function whose signature
// lowering used tryPrimitiveAttach rather than tryCollectorAttach),
// this allocates a fresh one and attaches its bytes to a freshly-
// created WasmType — so any wasm3 function can register types
// without needing the typeCollector path to have run first.
func wasm3RegisterStruct(fi *obj.FuncInfo, t *types.Type) uint32 {
	if fi == nil {
		base.Fatalf("wasm3RegisterStruct: fi is nil")
	}
	cAny, ok := wasm3LiveCollector.Load(fi)
	var c *typeCollector
	if ok {
		c = cAny.(*typeCollector)
	} else {
		c = newTypeCollector()
		wasm3LiveCollector.Store(fi, c)
		if fi.WasmType == nil {
			fi.WasmType = &obj.WasmType{}
		}
	}
	idx := c.collectBox(t)
	var b bytes.Buffer
	c.table.Write(&b)
	fi.WasmType.Table = b.Bytes()
	return uint32(idx)
}

// wasm3StashCollector hands off c to the wasm3LiveCollector map so
// codegen can register additional types into it. Called from
// tryCollectorAttach once the signature collector is built — the
// signature already populated the table with all ref fields, so
// codegen starts from there rather than from a fresh prelude-only
// collector.
func wasm3StashCollector(fi *obj.FuncInfo, c *typeCollector) {
	wasm3LiveCollector.Store(fi, c)
}

// wasm3RegisterArrayBacking is the array.* counterpart of
// wasm3RegisterStruct: it registers the wasmgc array type that
// backs a Go slice or array of `elem`, returning its module-
// internal type index. Like wasm3RegisterStruct it lazily
// initialises the function's typeCollector on first use.
func wasm3RegisterArrayBacking(fi *obj.FuncInfo, elem *types.Type) uint32 {
	if fi == nil {
		base.Fatalf("wasm3RegisterArrayBacking: fi is nil")
	}
	cAny, ok := wasm3LiveCollector.Load(fi)
	var c *typeCollector
	if ok {
		c = cAny.(*typeCollector)
	} else {
		c = newTypeCollector()
		wasm3LiveCollector.Store(fi, c)
		if fi.WasmType == nil {
			fi.WasmType = &obj.WasmType{}
		}
	}
	idx := c.collectBacking(elem)
	var b bytes.Buffer
	c.table.Write(&b)
	fi.WasmType.Table = b.Bytes()
	return uint32(idx)
}

func tryCollectorAttach(ft *types.Type) (wt *obj.WasmType, c *typeCollector, ok bool) {
	for _, p := range ft.RecvParams() {
		if !collectorMatchesRegabi(p.Type) {
			return nil, nil, false
		}
	}
	for _, r := range ft.Results() {
		if !collectorMatchesRegabi(r.Type) {
			return nil, nil, false
		}
	}
	defer func() {
		if r := recover(); r != nil {
			wt = nil
			c = nil
			ok = false
		}
	}()
	c = newTypeCollector()
	sig := c.loweredSignature(ft)
	wt = &obj.WasmType{WasmFuncType: sig}
	// Emit the per-function table whenever any field references a wasm
	// type — even a prelude type like go.bytes. The linker uses the
	// table as the remap source for every WasmRef Offset, so an empty
	// table on a signature that has refs would leave the linker without
	// a way to translate the per-function index. Primitive-only
	// signatures (no WasmRef anywhere) skip the table to keep their aux
	// payload small.
	if signatureHasRef(sig) {
		var b bytes.Buffer
		c.table.Write(&b)
		wt.Table = b.Bytes()
	}
	ok = true
	return
}

// collectorMatchesRegabi reports whether t's typeCollector lowering
// has the same shape as Go's register ABI for t. Conservative: only
// scalar primitives and structs (recursively) are accepted today.
// Slices, strings, interfaces, maps, channels, and func values all
// have a wasm-field shape that diverges from the regabi's per-
// register breakdown — bridging those requires SSA-level marshalling
// (a later rung).
func collectorMatchesRegabi(t *types.Type) bool {
	switch t.Kind() {
	case types.TBOOL,
		types.TINT8, types.TINT16, types.TINT32, types.TINT, types.TINT64,
		types.TUINT8, types.TUINT16, types.TUINT32, types.TUINT, types.TUINT64,
		types.TUINTPTR, types.TFLOAT32, types.TFLOAT64:
		return true
	case types.TPTR, types.TUNSAFEPTR:
		// At the top level tryPrimitiveAttach has already won and
		// lowered a pointer to i64; collectorMatchesRegabi is only
		// called for the composite-recursion case. Accept here so
		// the struct-walking case below can decide field-by-field
		// (it explicitly rejects pointer fields — see TSTRUCT).
		return true
	case types.TSTRUCT:
		for _, f := range t.Fields() {
			// Reject any struct containing a pointer field: the
			// typeCollector lowers a pointer to a ref-typed wasm
			// field, but the SSA call site pushes an i64 register.
			// The wasm validator rejects the mismatched signature.
			// tryFlatPrimitiveAttach catches the rejected case with
			// an i64-shaped fallback signature that matches what the
			// caller pushes — the function body may still fall back
			// to the obj backend's stub, but the module validates.
			if f.Type.Kind() == types.TPTR || f.Type.Kind() == types.TUNSAFEPTR {
				return false
			}
			if !collectorMatchesRegabi(f.Type) {
				return false
			}
		}
		return true
	case types.TARRAY:
		return collectorMatchesRegabi(t.Elem())
	}
	return false
}

func signatureHasRef(sig obj.WasmFuncType) bool {
	for _, f := range sig.Params {
		if f.Type == obj.WasmRef {
			return true
		}
	}
	for _, f := range sig.Results {
		if f.Type == obj.WasmRef {
			return true
		}
	}
	return false
}

// wasm3Fields lowers a Go parameter or result type to one or more wasm
// fields. Most types lower to a single field via wasm3IntField; the
// composite types Go's calling convention spreads across multiple
// register slots — currently strings (ptr, len) — are split here so
// they line up with the wrapper's paramsToWasmFields, which does the
// same splitting on its side.
func wasm3Fields(t *types.Type) ([]obj.WasmField, bool) {
	if t.Kind() == types.TSTRING {
		// (ptr, len) — both i64 to match the wasm3 internal register
		// width. The wasmexport wrapper splits a string into 2
		// WasmPtr (i32) fields and widens each across the boundary,
		// the same dance pointer params do.
		return []obj.WasmField{
			{Type: obj.WasmI64},
			{Type: obj.WasmI64},
		}, true
	}
	f, ok := wasm3IntField(t)
	if !ok {
		return nil, false
	}
	return []obj.WasmField{f}, true
}

// wasm3IntField lowers a primitive Go scalar to its width-faithful
// wasm field: i64 for the 64-bit integer kinds and pointer-shaped
// scalars (Go pointers occupy an i64 register at the SSA layer; the
// eventual cutover to WasmGC ref types is deferred to the struct
// rung), i32 for narrower integers and bools, and f32/f64 for the
// matching float kinds. ok is false for composite types (string,
// slice, struct, …) — those need the deferred per-package wasmgc.Table
// emission for their reference-typed signatures.
func wasm3IntField(t *types.Type) (obj.WasmField, bool) {
	switch t.Kind() {
	case types.TBOOL,
		types.TINT8, types.TINT16, types.TINT32,
		types.TUINT8, types.TUINT16, types.TUINT32:
		return obj.WasmField{Type: obj.WasmI32}, true
	case types.TINT, types.TINT64, types.TUINT, types.TUINT64, types.TUINTPTR,
		types.TPTR, types.TUNSAFEPTR:
		return obj.WasmField{Type: obj.WasmI64}, true
	case types.TFLOAT32:
		return obj.WasmField{Type: obj.WasmF32}, true
	case types.TFLOAT64:
		return obj.WasmField{Type: obj.WasmF64}, true
	}
	return obj.WasmField{}, false
}

// collectSignature reserves and returns the table index of the wasm
// function type for a Go function type, registering every GC type the
// signature references along the way. Every wasm3 function is emitted as
// a native typed wasm function referencing one such entry; the linker
// merges the per-package tables and remaps the indices.
func (c *typeCollector) collectSignature(ft *types.Type) int {
	if ft.Kind() != types.TFUNC {
		panic("wasm3: collectSignature on non-function type " + ft.Kind().String())
	}
	if idx, ok := c.funcs[ft]; ok {
		return idx
	}
	params, results := c.loweredStorages(ft)
	idx := len(c.table)
	c.table = append(c.table, wasmgc.Type{
		Name:    "go.func." + typeName(ft),
		Kind:    wasmgc.KindFunc,
		Super:   -1,
		Params:  params,
		Results: results,
	})
	c.funcs[ft] = idx
	return idx
}
