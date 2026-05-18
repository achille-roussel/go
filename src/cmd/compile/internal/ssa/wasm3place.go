// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssa

import (
	"cmd/compile/internal/types"
	"cmd/internal/obj"
)

// wasm3place.go is the wasm3-specific value-placement pass for the M3
// regalloc-bypass restructure (doc/wasm3-m3-no-regalloc.md).
//
// The pass runs *alongside* regalloc (between flagalloc and regalloc
// in the SSA pipeline), populating f.Wasm3ValueLocals and
// f.Wasm3LocalTypes for the wasm3 backend's genssa path. Codegen
// reads a value's per-value local via wasm3ValueLocalIdx; values the
// pass skips (OpArg*, OpSelect*, OpPhi-of-memory, statement-mark
// pseudo-ops) fall back to regalloc's register-local assignment.
//
// Phi resolution: OpPhi *is* placed (gets its own per-value local).
// The wasm3 backend's ssaGenBlock emits a `local.get src; local.set
// Lphi` copy on each incoming control-flow edge before the branch,
// taking the role regalloc's destination-register-sharing scheme
// plays for the register-local path.
//
// Placement strategy: every value that survives wasm3HasOutput gets
// a fresh local in schedule order, typed via wasm3ValueType. The obj
// backend declares them in fn.Wasm3LocalTypes order immediately
// after the wasm function's parameter locals, so the absolute wasm-
// local index is len(WasmType.Params) + Wasm3ValueLocals[v.ID].

// Wasm value-type bytes from the binary format. Kept private to
// avoid a circular import with cmd/internal/obj/wasm — when the obj
// backend reads these it interprets the bytes directly.
const (
	wasm3ValI32   = 0x7F
	wasm3ValI64   = 0x7E
	wasm3ValF32   = 0x7D
	wasm3ValF64   = 0x7C
	wasm3ValAnyref = 0x6E // (ref null any), single-byte abstract heap type
)

// wasm3MarkOnStack mirrors regalloc's OnWasmStack analysis
// (regalloc.go around line 850) so wasm3PlaceValues can skip
// placement for values that will be consumed inline on the wasm
// stack. With regalloc skipped for GOARCH=wasm3, this is the
// only place v.OnWasmStack gets set.
func wasm3MarkOnStack(f *Func) {
	canLiveOnStack := f.newSparseSet(f.NumValues())
	defer f.retSparseSet(canLiveOnStack)
	for _, b := range f.Blocks {
		canLiveOnStack.clear()
		for _, c := range b.ControlValues() {
			if c.Uses == 1 && !opcodeTable[c.Op].generic {
				canLiveOnStack.add(c.ID)
			}
		}
		for i := len(b.Values) - 1; i >= 0; i-- {
			v := b.Values[i]
			if canLiveOnStack.contains(v.ID) {
				v.OnWasmStack = true
			} else {
				canLiveOnStack.clear()
			}
			// Generic consumers (OpConvert, OpPhi, OpKeepAlive, etc.)
			// don't call getValue on their args in the wasm3 backend —
			// most go through ssagen's `nothing to do` short-circuit.
			// With regalloc skipped there's no spill-driven
			// materialization to absorb their args, so an OnWasmStack
			// value flowing into a generic op would leak (Skipped++
			// with no matching decrement). Skip adding args when v is
			// generic — except for OpMakeResult, whose args are
			// consumed by BlockRet's getValue calls and therefore do
			// honor the OnWasmStack contract.
			if opcodeTable[v.Op].generic && v.Op != OpMakeResult {
				continue
			}
			for _, arg := range v.Args {
				if arg.Uses == 1 && arg.Block == v.Block && !arg.Type.IsMemory() && !opcodeTable[arg.Op].generic {
					canLiveOnStack.add(arg.ID)
				}
			}
		}
	}
}

// wasm3PlaceValues populates f.Wasm3ValueLocals and f.Wasm3LocalTypes
// for GOARCH=wasm3 functions. No-op for other arches.
func wasm3PlaceValues(f *Func) {
	if f.Config.arch != "wasm3" {
		return
	}

	wasm3MarkOnStack(f)

	// Single walk over blocks/values in layout/schedule order:
	// assign a fresh local to every value that survives wasm3HasOutput.
	// OpArgIntReg / OpArgFloatReg are intentionally skipped (see
	// wasm3HasOutput): a parameter already lives in its wasm
	// parameter local 0..nparams-1, and the genssa fallback path
	// for unplaced values (getReg → local.get <param>) handles
	// the read just fine — no per-value-local copy needed for it.

	// maxID + 1 is the upper bound on value IDs in this function;
	// we use a sparse slice rather than a map so the lookup in
	// genssa is a direct index without a map probe.
	locals := make([]uint32, f.NumValues())
	// noLocal sentinel — a value with no local mapping (parameters,
	// Phi, statement-mark ops). Use 0xFFFFFFFF; real local indices
	// fit well within 32 bits for any realistic function.
	const noLocal = ^uint32(0)
	for i := range locals {
		locals[i] = noLocal
	}

	var types []byte
	nextLocal := uint32(0)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if !wasm3HasOutput(v) {
				continue
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
	// OpArgIntReg / OpArgFloatReg ARE placed. A wasm function
	// parameter lives in wasm local 0..nparams-1 by the function
	// signature, but the SSA backend uses i64 internally even for
	// narrow params (int32, etc.) — so OpArg gets its own per-
	// value local of the SSA type, and the wasm3 backend emits an
	// entry-prologue copy (`local.get <param>; widen?; local.set
	// <Larg>`) at each OpArg's codegen site. Routing OpArg through
	// a per-value local makes its consumers (and especially
	// emitPhiCopies sources) independent of regalloc's
	// register-local assignment, paving the way for Phase 4.
	case OpSelect0, OpSelect1:
		// Select0/Select1 are used by Mul64uhilo and friends
		// (Wasm3.rules rewrites them away into Hmul64u/I64Mul),
		// so they shouldn't survive to genssa. If one does, fall
		// back to the register-local path: ssagen's outer loop
		// skips Arch.SSAGenValue for Select{0,1,N}, so we can't
		// emit anything backend-side anyway.
		return false
		// OpSelectN IS placed. ssagen skips Arch.SSAGenValue for
		// it, but the wasm3 backend's call-result placement loop
		// scans for the SelectN values that consume each result
		// of the call and writes the wasm-stack value directly
		// into the selector's per-value local. The fallback when
		// no SelectN exists (or no per-value local was assigned)
		// is the regalloc-driven setReg path.
	}
	// OpPhi is intentionally allowed through: a Phi gets its own
	// per-value local, and the wasm3 backend emits explicit
	// per-edge `local.get src; local.set Lphi` copies in
	// ssaGenBlock before each control transfer, taking the place
	// of regalloc's destination-register-sharing scheme.

	// Skip placement for values wasm3MarkOnStack flagged
	// OnWasmStack — they're consumed inline at their use site,
	// so the per-value local would be declared but never written.
	if v.OnWasmStack {
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
// Ref-producing ops (OpWasm3StructNew / *Default, OpWasm3ArrayNew /
// *Default, OpWasm3RefNull, OpWasm3RefCast) get an `(ref null any)`
// local. Their wasm semantics produce a `(ref $T)` value that must
// land in a ref-typed local; the abstract `(ref null any)` byte is
// the single-byte encoding that needs no R_WASMTYPE relocation and
// accepts every typed-ref subtype by wasmgc subtyping. Consumers
// downcast with `ref.cast (ref $T)` at their use site (the
// `(ref $T)` precise type comes from v.Aux).
//
// OpArgIntReg whose wasm parameter type is WasmAnyref or WasmRef
// (per the function's WasmType signature) also gets an anyref
// local — the local.get of the param must land in a type
// compatible with the wasm signature. Stage E phase 2 lowers
// slice-ptr params to WasmAnyref via flatPrimitiveFields.
//
// Pointer-shaped values lower to i64 in the M2 backend; the M3
// switch to ref types (Stage B onward) will revise this — first
// for these explicit GC-op producers, then for ordinary Go *T
// values once Stage C's rules lower OpAddr/OpLoad/OpStore on
// struct fields to struct.get/struct.set.
func wasm3ValueType(v *Value) byte {
	switch v.Op {
	case OpWasm3StructNew, OpWasm3StructNewDefault,
		OpWasm3ArrayNew, OpWasm3ArrayNewDefault,
		OpWasm3RefNull, OpWasm3RefCast,
		OpWasm3StackArray, OpWasm3MakeSlice, OpWasm3SubSlice,
		OpWasm3FuncValue:
		return wasm3ValAnyref
	case OpArgIntReg:
		if wasm3OpArgIsRefParam(v) {
			return wasm3ValAnyref
		}
	}
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

// Wasm3IsAnyrefValue reports whether v's per-value wasm local is
// declared as anyref (per wasm3ValueType). Used by the wasm3 obj
// backend to special-case comparisons on ref-typed values — an
// I64Ne between two anyref locals must lower to `ref.eq; i32.eqz`
// (and I64Eqz on an anyref to `ref.is_null`), since `i64.ne` is
// invalid against anyref operands.
func Wasm3IsAnyrefValue(v *Value) bool {
	return wasm3ValueType(v) == wasm3ValAnyref
}

// Wasm3SliceArgElemType reports the slice element *types.Type for v
// when v is an OpArgIntReg corresponding to a TSLICE parameter's
// data-pointer field. Returns nil if v is not a slice-ptr OpArg
// (the caller's lowering rule should then not fire).
//
// Used by Wasm3.rules lowering for the (I64Add OpArgIntReg ...)
// shape that arises when a slice parameter's `s[i]` access reaches
// the SSA codegen. The returned slice's Elem() drives both the
// ArrayGet/Set opcode-width choice and the wasmgc-backing-type
// registration done by wasm3RegisterArrayAux.
func Wasm3SliceArgElemType(v *Value) *types.Type {
	if v.Op != OpArgIntReg {
		return nil
	}
	ifn := v.Block.Func.Frontend().Func()
	if ifn == nil || ifn.LSym == nil {
		return nil
	}
	wt := ifn.LSym.Func().WasmType
	if wt == nil {
		return nil
	}
	wantIntIdx := v.AuxInt
	var intCount int64
	wasmFieldIdx := -1
	for i, f := range wt.Params {
		if f.Type == obj.WasmF32 || f.Type == obj.WasmF64 {
			continue
		}
		if intCount == wantIntIdx {
			wasmFieldIdx = i
			break
		}
		intCount++
	}
	if wasmFieldIdx < 0 {
		return nil
	}
	if wt.Params[wasmFieldIdx].Type != obj.WasmAnyref {
		return nil
	}
	cursor := 0
	for _, p := range ifn.Type().RecvParams() {
		nFields := wasm3NumFlatFields(p.Type)
		if cursor <= wasmFieldIdx && wasmFieldIdx < cursor+nFields {
			if p.Type.IsSlice() && wasmFieldIdx == cursor {
				return p.Type.Elem()
			}
			return nil
		}
		cursor += nFields
	}
	return nil
}

// wasm3NumFlatFields returns the number of wasm fields a Go type t
// produces when flattened by the wasm3 compiler's flatPrimitiveFields
// path (cmd/compile/internal/wasm3/wasmabi.go). Slices flatten to 3
// fields, strings to 2, interfaces to 2, complex to 2; the rest are
// 1 field. Mirrors flatPrimitiveFields' explicit cases.
func wasm3NumFlatFields(t *types.Type) int {
	switch t.Kind() {
	case types.TSLICE:
		return 3
	case types.TSTRING:
		return 2
	case types.TINTER:
		return 2
	case types.TCOMPLEX64, types.TCOMPLEX128:
		return 2
	case types.TSTRUCT:
		n := 0
		for _, f := range t.Fields() {
			n += wasm3NumFlatFields(f.Type)
		}
		return n
	}
	return 1
}

// wasm3OpArgIsRefParam reports whether v is an OpArgIntReg whose
// corresponding wasm function-signature parameter is WasmAnyref or
// WasmRef. Used by wasm3ValueType to assign the right per-value
// local type; the entry-prologue local.get must land in a local
// compatible with the wasm parameter's type.
//
// v.AuxInt is the per-class integer-param index — it counts only
// OpArgIntReg-shaped params before v in declaration order. The
// wasm signature interleaves int and float fields; this scan walks
// WasmType.Params and counts non-float fields to find the absolute
// wasm field index.
func wasm3OpArgIsRefParam(v *Value) bool {
	if v.Op != OpArgIntReg {
		return false
	}
	ifn := v.Block.Func.Frontend().Func()
	if ifn == nil || ifn.LSym == nil {
		return false
	}
	wt := ifn.LSym.Func().WasmType
	if wt == nil {
		return false
	}
	wantIdx := v.AuxInt
	var intCount int64
	for _, f := range wt.Params {
		switch f.Type {
		case obj.WasmF32, obj.WasmF64:
			continue
		}
		if intCount == wantIdx {
			return f.Type == obj.WasmAnyref || f.Type == obj.WasmRef
		}
		intCount++
	}
	return false
}
