// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

// wasm3place.go is the wasm3-specific value-placement pass for the M3
// regalloc-bypass restructure (doc/wasm3-m3-no-regalloc.md, Phase 1).
//
// Phase 1 scope: the pass runs as a scaffold alongside the normal
// regalloc pass (it does *not* skip regalloc). It computes the
// value-ID → wasm-local-index map and the per-local wasm value-type
// vector, storing both on f.Wasm3ValueLocals / f.Wasm3LocalTypes.
// Codegen does not yet consume these — that's Phase 2. The scaffold
// exists so the harder later phases have something to verify
// against and so a buggy place-values pass surfaces before genssa
// depends on it.
//
// The placement strategy is the simplest correct one: every value
// gets its own wasm local in schedule order, typed by its Go type
// width. Function parameters (OpArgIntReg, OpArgFloatReg) consume
// the first N local slots (matching wasm's "params are locals 0..N-1"
// convention); the rest of the values get fresh locals afterward.

// Wasm value-type bytes from the binary format. Kept private to
// avoid a circular import with cmd/internal/obj/wasm — when the obj
// backend reads these it interprets the bytes directly.
const (
	wasm3ValI32 = 0x7F
	wasm3ValI64 = 0x7E
	wasm3ValF32 = 0x7D
	wasm3ValF64 = 0x7C
)

// wasm3PlaceValues populates f.Wasm3ValueLocals and f.Wasm3LocalTypes
// for GOARCH=wasm3 functions. No-op for other arches.
func wasm3PlaceValues(f *Func) {
	if f.Config.arch != "wasm3" {
		return
	}

	// Two passes. Pass 1 reserves the first M locals for the
	// function's parameter values (those produced by OpArgIntReg /
	// OpArgFloatReg) in parameter order, so the resulting local
	// indices match wasm's "params are locals 0..M-1" convention.
	// Pass 2 walks blocks in layout order, values in schedule order,
	// and assigns a fresh local to every other value-producing op.

	// maxID + 1 is the upper bound on value IDs in this function;
	// we use a sparse slice rather than a map so the lookup in
	// genssa is a direct index without a map probe.
	locals := make([]uint32, f.NumValues())
	// noLocal sentinel — a value with no local mapping (e.g. Phi,
	// statement-mark ops). Use 0xFFFFFFFF; real local indices fit
	// well within 32 bits for any realistic function.
	const noLocal = ^uint32(0)
	for i := range locals {
		locals[i] = noLocal
	}

	var types []byte
	nextLocal := uint32(0)

	// Pass 1: parameters first, in (block-entry) order. The entry
	// block (f.Entry) holds the OpArg* values; the compiler emits
	// them in parameter order.
	for _, v := range f.Entry.Values {
		switch v.Op {
		case OpArgIntReg, OpArgFloatReg:
			locals[v.ID] = nextLocal
			types = append(types, wasm3ValueType(v))
			nextLocal++
		}
	}

	// Pass 2: every other value-producing op, block by block.
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if locals[v.ID] != noLocal {
				continue // already placed in pass 1
			}
			if !wasm3HasOutput(v) {
				continue // memory phi, statement marks, etc.
			}
			locals[v.ID] = nextLocal
			types = append(types, wasm3ValueType(v))
			nextLocal++
		}
	}

	f.Wasm3ValueLocals = locals
	f.Wasm3LocalTypes = types

	// Publish to the underlying LSym's FuncInfo so the wasm3 obj
	// backend (which runs after genssa in the same process) can read
	// the placement without going through SSA-private data structures.
	if ifn := f.Frontend().Func(); ifn != nil && ifn.LSym != nil {
		fi := ifn.LSym.Func()
		fi.Wasm3ValueLocals = locals
		fi.Wasm3LocalTypes = types
	}
}

// wasm3HasOutput reports whether v produces a value that needs a
// wasm local. Excludes mem-typed phis, void-result ops, and
// statement-marking pseudo-ops that have no runtime representation.
func wasm3HasOutput(v *Value) bool {
	if v.Type == nil {
		return false
	}
	if v.Type.IsMemory() || v.Type.IsVoid() || v.Type.IsFlags() || v.Type.IsTuple() {
		return false
	}
	// Some no-op markers carry typed results in the SSA model but
	// emit no bytes — they don't need a local.
	switch v.Op {
	case OpInlMark, OpInvalid, OpUnknown, OpVarDef, OpVarLive, OpKeepAlive:
		return false
	case OpPhi:
		// Phi resolution (merging incoming locals into one) is a
		// separate concern handled by a later phase. Phase 1 leaves
		// them unplaced; the codegen path still uses v.Reg() so
		// nothing breaks.
		return false
	}
	return true
}

// wasm3ValueType returns the wasm value-type byte for v's Go type.
//
// All integer-class values lower to i64. The SSA backend works in
// i64 GP registers; sub-word values are widened at boundaries
// (parameter entry, comparison results, etc.) and stored as i64
// internally. This matches the existing register-local convention
// the obj backend's `regType` produces.
//
// Pointer-shaped values lower to i64 in the M2 backend; the M3
// switch to ref types (Stage B onward) will revise this.
func wasm3ValueType(v *Value) byte {
	t := v.Type
	if t.IsFloat() {
		switch t.Size() {
		case 4:
			return wasm3ValF32
		case 8:
			return wasm3ValF64
		}
		return wasm3ValF64
	}
	return wasm3ValI64
}
