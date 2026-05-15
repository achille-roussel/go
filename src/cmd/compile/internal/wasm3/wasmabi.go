// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm3

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/wasmgc"
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
// integer-class Go scalar are attached, and each such slot lowers to a
// single wasm i64 — the width of the "register" the SSA backend
// operates on. Narrower Go integers (int32, byte, bool, …) still occupy
// a full i64 across the call boundary, with their narrowing semantics
// kept inside the function body; this matches how Go's own register
// ABI treats sub-word integers on 64-bit targets, and it keeps the
// boundary type (i64) consistent with what the obj backend emits for a
// GP register.
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
	// //go:wasmimport and //go:wasmexport functions already carry a
	// typed signature to the linker through their own aux symbol (which
	// is also emitted as a goobj.AuxWasmType); attaching a second one
	// would be a duplicate.
	if fn.WasmImport != nil || fn.WasmExport != nil {
		return
	}
	var sig obj.WasmFuncType
	for _, p := range ft.RecvParams() {
		if !isWasm3IntKind(p.Type.Kind()) {
			return
		}
		sig.Params = append(sig.Params, obj.WasmField{Type: obj.WasmI64})
	}
	for _, r := range ft.Results() {
		if !isWasm3IntKind(r.Type.Kind()) {
			return
		}
		sig.Results = append(sig.Results, obj.WasmField{Type: obj.WasmI64})
	}
	fn.LSym.Func().WasmType = &obj.WasmType{WasmFuncType: sig}
}

// isWasm3IntKind reports whether a Go type kind is an integer-class
// scalar that the Stage C.2 ABI lowers to a single wasm i64 slot.
func isWasm3IntKind(k types.Kind) bool {
	switch k {
	case types.TBOOL,
		types.TINT, types.TINT8, types.TINT16, types.TINT32, types.TINT64,
		types.TUINT, types.TUINT8, types.TUINT16, types.TUINT32, types.TUINT64,
		types.TUINTPTR:
		return true
	}
	return false
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
