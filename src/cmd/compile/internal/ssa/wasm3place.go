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
			if canLiveOnStack.contains(v.ID) && !wasm3SkipOnStackMark(v) {
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
			// Fat-pointer ops (Piece 3 of
			// doc/wasm3-fat-pointers-design.md) are kept out of the
			// OnWasmStack optimisation entirely — see
			// wasm3SkipOnStackMark for why. They use per-value-local
			// writeback unconditionally, which means their args also
			// shouldn't be promoted (the codegen reads each arg via a
			// plain local.get rather than relying on the stack-passing
			// contract).
			if wasm3SkipOnStackMark(v) {
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

// wasm3SkipOnStackMark reports whether v should be excluded from the
// OnWasmStack optimisation entirely — its result is always written to
// a per-value local, never left transient on the wasm stack for the
// next op to consume. Used for the fat-pointer ops added in Piece 3
// of doc/wasm3-fat-pointers-design.md.
//
// Why opt out: OpWasm3LoadInterior and OpWasm3StoreInterior read two
// fields from the same iptr arg (container + offset), which forces
// them to either call getValue64 twice on the arg or to stash it in
// a temp local. With OnWasmStack=true on the arg, the first
// getValue64 emits the producer inline; a second call would emit it
// AGAIN. The temp-local workaround works for the codegen but the
// extra local.tee + local.get isn't free, and more importantly the
// OnWasmStack accounting (OnWasmStackSkipped) gets confused by the
// nested inline-emission paths the new ops introduce, surfacing as
// "wasm: bad stack" failures at block end.
//
// Per the project-level guidance (correctness first, optimisation
// later), the simplest fix is to skip the OnWasmStack analysis for
// these ops: their args and results always go through per-value
// locals. Slower, but trivially correct.
func wasm3SkipOnStackMark(v *Value) bool {
	switch v.Op {
	case OpWasm3InteriorPtr, OpWasm3LoadInterior, OpWasm3StoreInterior:
		return true
	}
	return false
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

	// Compute the set of SSA Values whose per-value local must be
	// anyref. Direct producers (e.g. OpWasm3MakeSlice, OpArgIntReg
	// for a wasmgc-slice-arg's .array, OpSelectN of a slice-
	// returning call) are seeded first; the propagation step then
	// extends the set through OpCopy / OpPhi so a slice's .array
	// retains its anyref classification across the SSA value flow
	// that the rewrite pipeline tends to introduce (e.g. the
	// `b = b[n:]` loop variable threading through a Phi in
	// runtime.concatstrings). Without this propagation, the
	// `(*byte)`-typed Phi would default to i64 and the call's
	// anyref result would fail to local.set into it.
	anyref := wasm3ComputeAnyrefValues(f)

	var types []byte
	nextLocal := uint32(0)
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if !wasm3HasOutput(v) {
				continue
			}
			locals[v.ID] = nextLocal
			t := wasm3ValueType(v)
			if anyref[v] {
				t = wasm3ValAnyref
			}
			types = append(types, t)
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

// wasm3ComputeAnyrefValues runs a fixed-point pass over f to find
// every SSA Value whose per-value wasm local must be anyref. The
// classification has two layers:
//
//  1. Direct producers — values whose Op already signals anyref
//     (OpWasm3MakeSlice, OpWasm3StackArray, OpArgIntReg of a
//     wasmgc-typed param, OpSelectN extracting a slice's .array
//     from a slice-returning call, etc.). These are exactly the
//     cases wasm3ValueType handles in its Op-based switch.
//  2. Propagation — OpCopy / OpPhi / OpSelectN values whose
//     incoming Args include an already-anyref Value also need
//     an anyref local. Without propagation, a Phi like
//
//       b1 := …slice-from-call….ptr         // anyref
//       loop {
//         b1 = b1[n:].ptr                    // OpPhi <*byte>
//       }
//
//     gets its OpPhi local typed i64 (the default for `*byte`),
//     and the anyref-typed Phi predecessor fails to local.set.
//     A fixed-point pass propagates the anyref classification
//     until stable. The Op set is intentionally narrow — only
//     value-flow ops that pass through their input verbatim —
//     so transformations like loads/stores/arith that change
//     the value's wasm-stack representation aren't promoted.
func wasm3ComputeAnyrefValues(f *Func) map[*Value]bool {
	set := make(map[*Value]bool)
	// Seed: direct producers per wasm3ValueType's Op-based logic.
	for _, b := range f.Blocks {
		for _, v := range b.Values {
			if wasm3IsDirectAnyref(v) {
				set[v] = true
			}
		}
	}
	// Propagate through Copy (single Arg). For Phi, only propagate
	// when EVERY incoming Value is anyref — a Phi with mixed
	// anyref + i64 inputs (e.g. append's slice .array Phi, where
	// the initial nil-slice is i64 0 and the growslice return is
	// anyref) cannot pick one wasm-typed local without forcing
	// an invalid local.set on the other side; the conservative
	// fallback keeps the Phi at i64 and any anyref input must
	// be bridged elsewhere. The Phi-all-anyref case is what
	// runtime.concatstrings hits (b = b[n:] loops back through
	// a Phi whose only incoming Value is the anyref from
	// rawstringtmp's result).
	for changed := true; changed; {
		changed = false
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if set[v] {
					continue
				}
				switch v.Op {
				case OpCopy:
					if len(v.Args) >= 1 && set[v.Args[0]] {
						set[v] = true
						changed = true
					}
				case OpPhi:
					if len(v.Args) == 0 {
						continue
					}
					allAnyref := true
					for _, a := range v.Args {
						if !set[a] {
							allAnyref = false
							break
						}
					}
					if allAnyref {
						set[v] = true
						changed = true
					}
				}
			}
		}
	}
	return set
}

// wasm3IsDirectAnyref reports whether v's Op alone (independent of
// Args) classifies the value as anyref. Mirrors the Op-based
// branches of wasm3ValueType; the value-type-based logic
// (looking at v.Type) is deliberately excluded because *byte etc.
// can be either anyref or i64 depending on context — propagation
// is the way to resolve that.
func wasm3IsDirectAnyref(v *Value) bool {
	switch v.Op {
	case OpWasm3StructNew, OpWasm3StructNewDefault,
		OpWasm3ArrayNew, OpWasm3ArrayNewDefault,
		OpWasm3RefNull, OpWasm3RefCast,
		OpWasm3StackArray, OpWasm3StackStruct, OpWasm3MakeSlice, OpWasm3SubSlice, OpWasm3SliceData, OpWasm3StringData, OpWasm3IfaceItab, OpWasm3IfaceData, OpWasm3IfaceMake,
		OpWasm3FuncValue, OpWasm3MakeClosureRef,
		OpWasm3MakeClosureRefInline,
		OpWasm3LoweredGetClosureRef, OpWasm3LoweredCastClosureRef,
		OpWasm3GlobalGet:
		return true
	case OpWasm3GetClosureField:
		return v.AuxInt&wasm3GetClosureFieldAnyrefBit != 0
	case OpArgIntReg:
		return wasm3OpArgIsRefParam(v)
	case OpSelectN:
		return wasm3SelectNIsAnyrefResult(v)
	}
	return false
}

// wasm3PointerIsRef reports whether v's Go type is a pointer that the
// pointer-representation cutover represents as a WasmGC reference rather
// than a linear i64 address. Scoped to *struct for the first cutover
// step (doc/wasm3-pointer-cutover); other pointee kinds follow.
func wasm3PointerIsRef(v *Value) bool {
	t := v.Type
	if t == nil || !t.IsPtr() {
		return false
	}
	e := t.Elem()
	return e != nil && (e.IsStruct() || e.IsArray())
}

// wasm3IsScalarPtr reports whether t is a pointer to an integer/bool Go
// scalar that maps to the i64 or i32 interior-pointer class. Such a
// pointer is an interior pointer represented as a $go.ptr.<class>
// accessor-pair fat pointer (doc/wasm3-pointer-cutover): a scalar always
// lives inside a container (struct field / array element / boxed cell),
// so it cannot be a direct ref like a *struct. Used by both
// wasm3ValueType (the value's local is anyref) and the lowering rules
// (Wasm3.rules / late-lower). Float (f32/f64) and ref (anyref) pointee
// classes follow once their $go.ptr.<class> wrappers + codegen exist.
func wasm3IsScalarPtr(t *types.Type) bool {
	if t == nil || !t.IsPtr() {
		return false
	}
	e := t.Elem()
	if e == nil {
		return false
	}
	switch e.Kind() {
	case types.TINT, types.TINT64, types.TUINT, types.TUINT64, types.TUINTPTR, // i64 class
		types.TINT32, types.TUINT32, types.TINT16, types.TUINT16,
		types.TINT8, types.TUINT8, types.TBOOL: // i32 class (via i64 ABI)
		return true
	case types.TUNSAFEPTR: // ref/anyref class
		return true
	}
	return false
}

// wasm3IsFieldInteriorPtr reports whether t is a pointer to a struct-field
// leaf class that MakeFieldPtr / the WasmGCFieldGetter/Setter accessor pair
// can represent as a $go.ptr.<class> fat pointer: the i64 class (integer/
// bool, via the converting accessors), unsafe.Pointer, and *T pointer
// fields (ref class). Each of these occupies a single field slot whose
// byte offset matches what wasm3FieldAtOffset resolves. Float fields are
// excluded (the i64 converting accessor would truncate, and there is no
// float $go.ptr class); map/chan/func are excluded (not convertible
// through the i64 accessor, not in the ref class); and slice/string/
// interface fields are excluded for now — they are boxed but their Go ABI
// field offsets span multiple linear words (24/16 bytes), which does not
// match the single boxed ref slot, so an escaping &s.sliceField needs
// extra offset reconciliation (a separate piece). Used by the late-lower
// OffPtr -> MakeFieldPtr rule so an escaping &struct.field of a handled
// class becomes a fat pointer rather than staying an unlowered OffPtr.
func wasm3IsFieldInteriorPtr(t *types.Type) bool {
	if t == nil || !t.IsPtr() {
		return false
	}
	e := t.Elem()
	if e == nil {
		return false
	}
	switch e.Kind() {
	case types.TINT, types.TINT64, types.TUINT, types.TUINT64, types.TUINTPTR,
		types.TINT32, types.TUINT32, types.TINT16, types.TUINT16,
		types.TINT8, types.TUINT8, types.TBOOL: // i64 class
		return true
	case types.TUNSAFEPTR, types.TPTR: // ref class (single-slot pointer fields)
		return true
	}
	return false
}

// wasm3IsBoxedType reports whether a value of type t is represented as a
// single WasmGC reference (so a package-level var of type t cannot live
// in linear-memory static data and must be a wasm ref-global; see
// doc/wasm3-pointer-cutover). Covers the directly-ref-shaped types;
// composite value types (struct/array) that merely *contain* a ref are
// not handled here (they need finer-grained global handling).
func wasm3IsBoxedType(t *types.Type) bool {
	if t == nil {
		return false
	}
	switch t.Kind() {
	case types.TSLICE, types.TSTRING, types.TINTER,
		types.TMAP, types.TCHAN, types.TFUNC:
		return true
	case types.TPTR:
		e := t.Elem()
		return e != nil && e.IsStruct()
	}
	return false
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
		OpWasm3StackArray, OpWasm3StackStruct, OpWasm3MakeSlice, OpWasm3SubSlice, OpWasm3SliceData, OpWasm3StringData, OpWasm3IfaceItab, OpWasm3IfaceData, OpWasm3IfaceMake,
		OpWasm3FuncValue, OpWasm3MakeClosureRef,
		OpWasm3MakeClosureRefInline,
		OpWasm3LoweredGetClosureRef, OpWasm3LoweredCastClosureRef,
		OpWasm3InteriorPtr, OpWasm3GlobalGet:
		return wasm3ValAnyref
	case OpWasm3GetClosureField:
		// Whether a GetClosureField produces an anyref or an i64
		// local depends on the closureCtx field type at AuxInt, not
		// on v.Type — a slice's .array field reads back as anyref
		// even though its IR type is `*Elem`. The body-side prologue
		// signals this by setting an AuxInt high-bit (handled at
		// the dedicated check below) so wasm3ValueType doesn't have
		// to recover the field type from the side channel.
		if v.AuxInt&wasm3GetClosureFieldAnyrefBit != 0 {
			return wasm3ValAnyref
		}
		// Otherwise scalar (int/uintptr) captures stay i64 by
		// falling through to the generic type-based default below.
	case OpArgIntReg:
		if wasm3OpArgIsRefParam(v) {
			return wasm3ValAnyref
		}
	case OpSelectN:
		// A function call returning a slice (or another anyref-
		// shaped composite) leaves the slice's .array on the wasm
		// stack as anyref, but the SSA OpSelectN that extracts
		// it has Type *Elem (a pointer) — wasm3ValueType's
		// generic default would assign an i64 local, which then
		// mismatches the call's anyref result and fails wasm
		// validation at the local.set after the call. Detect the
		// slice-component case by walking the parent call's
		// return tuple: if the SelectN's index lands on a slice
		// .array field, classify as anyref.
		if wasm3SelectNIsAnyrefResult(v) {
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
	// Stage G: a SSA value whose Go type is func or *func represents a
	// wasmgc closure ref (see ssagen PFUNC handling and walkClosure
	// wrap). The per-value local must be anyref so OpWasm3LoweredClosureCall
	// can ref.cast it to the typed closureCtx struct. The wasm signature
	// for a TFUNC param/result is also anyref (flatPrimitiveFields), so
	// the call-result -> local copy is well-typed.
	//
	// Exception (Stage F): a TFUNC value loaded from a linear-memory
	// struct field — typically a func-typed field of `*_type` or
	// `*itab` — is the wasm result of `i64.load`, an i64. The local
	// stays i64 and the call path uses call_indirect (which takes
	// an i32 funcidx, not an anyref). Without this exception the
	// runtime helpers that read those fields (runtime.ifaceeq,
	// runtime.gwrite via getg().writebuf path, etc.) trip
	// "i64.load result into anyref local" validation errors.
	if isWasm3LoadOp(v.Op) {
		// Loaded TFUNC and pointer-to-TFUNC values stay i64.
		// Other types fall through to the generic categorisation.
	} else if t.Kind() == types.TFUNC || (t.IsPtr() && t.Elem() != nil && t.Elem().Kind() == types.TFUNC) {
		return wasm3ValAnyref
	} else if t.IsUnsafePtr() {
		// Pointer-representation cutover: unsafe.Pointer is a WasmGC ref
		// (go.object), so its per-value local is anyref — consistent with
		// the anyref ABI (wasm3IntField) and with (*T)(p) ref.casts.
		return wasm3ValAnyref
	} else if wasm3IsScalarPtr(t) {
		// A pointer to an integer/bool scalar is an interior pointer,
		// represented as a $go.ptr.<class> accessor-pair fat pointer
		// (anyref).
		return wasm3ValAnyref
	} else if wasm3PointerIsRef(v) {
		// Pointer-representation cutover (doc/wasm3-pointer-cutover):
		// a Go pointer to a heap struct is a WasmGC (ref $go.struct.T),
		// not a linear i64 address, so its per-value local is anyref
		// (downcast to the typed ref at struct.get/struct.set use
		// sites). Scoped to *struct first to limit blast radius; other
		// pointee kinds follow as the cutover lands.
		return wasm3ValAnyref
	} else if t.IsArray() {
		// A Go array lowers to a single (ref (array T)) (wasmtype.go
		// TARRAY), so an array-typed value (e.g. FieldGet of an array
		// struct field, an array param, or a StackArray) is the array
		// ref — its per-value local is anyref. Element access goes
		// through array.get/array.set on the ref (interior pointers).
		return wasm3ValAnyref
	}
	return wasm3ValI64
}

// wasm3GetClosureFieldAnyrefBit is a high bit set on
// OpWasm3GetClosureField's AuxInt when the captured field is
// anyref-typed in the closureCtx (e.g. the .array of a slice
// capture). The body-side prologue sets it for slice/string/array
// composite captures so wasm3ValueType allocates an anyref local
// for the result. The codegen masks it off when computing the
// struct.get field index.
const wasm3GetClosureFieldAnyrefBit int64 = 1 << 32

// Wasm3GetClosureFieldOffset extracts the actual field-offset
// portion of a OpWasm3GetClosureField AuxInt (clearing the anyref-
// marker bit). Used by the wasm3 obj codegen.
func Wasm3GetClosureFieldOffset(auxInt int64) int64 {
	return auxInt &^ wasm3GetClosureFieldAnyrefBit
}

// Wasm3GetClosureFieldAnyrefBit is the exported flag bit so callers
// (the wasm3 ssagen prologue) can mark composite-capture reads.
const Wasm3GetClosureFieldAnyrefBit int64 = wasm3GetClosureFieldAnyrefBit

// isWasm3LoadOp reports whether v.Op is one of the wasm3 load
// opcodes that produce an i64-shaped wasm result.
func isWasm3LoadOp(op Op) bool {
	switch op {
	case OpWasm3I64Load, OpWasm3I64Load8U, OpWasm3I64Load8S,
		OpWasm3I64Load16U, OpWasm3I64Load16S,
		OpWasm3I64Load32U, OpWasm3I64Load32S:
		return true
	}
	return false
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
	// Closure-captured slice case: the body-side prologue emits
	// OpWasm3GetClosureField with the anyref-marker bit set for a
	// slice .array field. v.Type is *T (elem pointer), so v.Type.Elem()
	// gives the elem type the rewrite rules need.
	if v.Op == OpWasm3GetClosureField && v.AuxInt&wasm3GetClosureFieldAnyrefBit != 0 {
		if v.Type != nil && v.Type.IsPtr() && v.Type.Elem() != nil {
			return v.Type.Elem()
		}
		return nil
	}
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
			return wasm3FindSliceElemAt(p.Type, wasmFieldIdx-cursor)
		}
		cursor += nFields
	}
	return nil
}

// wasm3FindSliceElemAt walks t's wasm-flattened field layout looking
// for a slice whose data-pointer field sits at the given local
// offset within t's flattened span. Returns the slice element type
// when found; returns nil otherwise (the local offset names a non-
// slice-data field, or a non-slice subfield).
//
// A slice flattens to 3 fields (data, len, cap); the data field is
// at offset 0 of the slice's flattened span. Struct subfields walk
// recursively. Top-level slice args (the most common case) match
// the `t.IsSlice() && off == 0` short-circuit.
func wasm3FindSliceElemAt(t *types.Type, off int) *types.Type {
	if t.IsSlice() {
		if off == 0 {
			return t.Elem()
		}
		return nil
	}
	if !t.IsStruct() {
		return nil
	}
	cursor := 0
	for _, f := range t.Fields() {
		nFields := wasm3NumFlatFields(f.Type)
		if cursor <= off && off < cursor+nFields {
			return wasm3FindSliceElemAt(f.Type, off-cursor)
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

// wasm3SelectNIsAnyrefResult reports whether v is an OpSelectN
// extracting a value that the wasm3 call-result ABI returns as
// anyref — specifically a slice's .array (data) component when the
// parent call returns a slice or a composite containing a slice.
// Used by wasm3ValueType so the per-value local matches the wasm
// stack value the call leaves behind.
func wasm3SelectNIsAnyrefResult(v *Value) bool {
	if v.Op != OpSelectN {
		return false
	}
	if len(v.Args) < 1 {
		return false
	}
	call := v.Args[0]
	auxCall, ok := call.Aux.(*AuxCall)
	if !ok || auxCall == nil {
		return false
	}
	// Consult the callee's actual wasm signature (set by
	// attachWasmType in cmd/compile/internal/wasm3). Many runtime
	// helpers like runtime.growslice flatten composite returns
	// through tryFlatPrimitiveAttach, ending up with all-i64
	// returns even when the abstract Go type is a slice — in that
	// case the call site pops i64 from the wasm stack and
	// SelectN's local must also be i64. Only when the callee's
	// wasm result field at this position is WasmAnyref do we
	// classify the SelectN as anyref.
	want := v.AuxInt
	if auxCall.Fn != nil {
		if fi := auxCall.Fn.Func(); fi != nil && fi.WasmType != nil {
			wt := fi.WasmType
			// AuxInt indexes the wasm result vector directly
			// (post-decomposition). The wasm sig was emitted in
			// the same flat order the SSA uses for its tuple
			// components, so position-based indexing matches.
			if want < 0 || want >= int64(len(wt.Results)) {
				return false
			}
			rf := wt.Results[want]
			return rf.Type == obj.WasmAnyref || rf.Type == obj.WasmRef
		}
	}
	// Cross-package callee: the FuncInfo (and therefore the cached
	// WasmType) lives in the callee's compilation unit and isn't
	// visible here. Reconstruct the anyref-ness from the abstract Go
	// return signature exposed by the AuxCall — flatPrimitiveFields
	// is deterministic on the Go type, so a slice's `.array` is
	// always anyref. Walk the abstract results to locate the
	// (resultIdx, subOff) that the flat AuxInt picks out, then ask
	// wasm3FieldIsAnyref about that field.
	cursor := int64(0)
	for i := int64(0); i < auxCall.NResults(); i++ {
		rt := auxCall.TypeOfResult(i)
		nFields := int64(wasm3NumFlatFields(rt))
		if cursor <= want && want < cursor+nFields {
			return wasm3FieldIsAnyref(rt, int(want-cursor))
		}
		cursor += nFields
	}
	return false
}

// wasm3FieldIsAnyref reports whether the offset-th flat wasm field
// of t is an anyref (the data pointer of a slice nested inside t,
// recursively for nested structs).
func wasm3FieldIsAnyref(t *types.Type, off int) bool {
	if t.IsSlice() {
		// Slice flattens to (data, len, cap); only the data field
		// (offset 0) is anyref.
		return off == 0
	}
	if !t.IsStruct() {
		return false
	}
	cursor := 0
	for _, f := range t.Fields() {
		nFields := wasm3NumFlatFields(f.Type)
		if cursor <= off && off < cursor+nFields {
			return wasm3FieldIsAnyref(f.Type, off-cursor)
		}
		cursor += nFields
	}
	return false
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
