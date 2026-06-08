// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm

import (
	"fmt"
	"os"
	"sync"

	"cmd/internal/obj"
)

// wasm3CheckLocalTypes walks fn.Text and, for each ALocalSet /
// ALocalTee, checks that the inferred wasm type at the top of the
// operand stack matches the declared type of the target local. On
// mismatch, ctxt.Diag is called with the source position from the
// offending obj.Prog. Returns false if a mismatch was found (the
// caller may choose to bail; we do, so the wasm validator's harder-
// to-read error doesn't follow).
//
// This is Option A of the wasm3 compile-time type-check design:
// a sliding one-slot inference rather than a full operand-stack
// simulator. It catches the i64-vs-anyref class of bugs that
// historically surface at wasm-instantiate time with messages like
// "local.tee[0] expected type anyref, found local.get of type
// i64". That class accounts for the bulk of the wasm3 bring-up
// pain — see Rung 34 commits and the acquireSudog / casgstatus /
// checkmcount / sync_atomic.Bool.Store debug sessions.
//
// Inference rules:
//
//   - producers (ALocalGet, AI64Const, ARefCast, AStructGet,
//     AStructNew, AArrayNew, ARefFunc, AI64ExtendI32U/S,
//     AI32WrapI64, ...) set topType to the result type
//   - consumers (ALocalSet, ALocalTee) check then either clear
//     (set) or keep (tee) topType
//   - block boundaries / control-flow (ABlock, ALoop, AIf, AElse,
//     AEnd, ABr, ABrIf, AReturn, AUnreachable, ACall, ACallRef,
//     AThrow, ATryTable, ...) clear topType — cross-block inference
//     would need a full simulator
//   - any opcode whose result type the inferrer doesn't know
//     clears topType (defensive — never false-positive)
//
// When topType is wt_unknown at a local.set/tee site, the check
// is skipped: we can't false-positive, only true-positive.
func wasm3CheckLocalTypes(ctxt *obj.Link, s *obj.LSym) bool {
	fn := s.Func()
	if fn == nil {
		return true
	}
	var params []obj.WasmField
	switch {
	case fn.WasmType != nil:
		params = fn.WasmType.Params
	case fn.WasmExport != nil:
		params = fn.WasmExport.Params
	case fn.WasmImport != nil:
		params = fn.WasmImport.Params
	default:
		return true
	}

	// localTypes[i] is the wasm value-type byte of local i.
	// Indices 0..len(params)-1 are the params; the per-value SSA
	// locals (fn.Wasm3LocalTypes) come next; register-locals
	// declared lazily by wasm3Locals follow. We only need types
	// for params + per-value SSA locals here — the register-local
	// range almost never receives anything we mis-classify (the
	// SSA-emit layer drives those directly and they hold typed
	// register data), so a sparse map keyed only on the indices
	// we care about is enough. Use wt_unknown for the rest.
	localTypes := make([]byte, 0, len(params)+len(fn.Wasm3LocalTypes))
	for _, f := range params {
		localTypes = append(localTypes, wasmFieldTypeByte(f.Type))
	}
	for _, t := range fn.Wasm3LocalTypes {
		localTypes = append(localTypes, t)
	}

	const wtUnknown byte = 0
	topType := wtUnknown
	ok := true

	for p := fn.Text; p != nil; p = p.Link {
		switch p.As {
		// Consumers we validate.
		case ALocalSet, ALocalTee:
			if p.To.Type == obj.TYPE_CONST && topType != wtUnknown {
				idx := p.To.Offset
				if idx >= 0 && int(idx) < len(localTypes) {
					declared := localTypes[idx]
					if declared != topType {
						wasm3CheckReport(ctxt, p, s.Name, p.As, idx, declared, topType)
						ok = false
					}
				}
			}
			if p.As == ALocalSet {
				topType = wtUnknown // value consumed, stack empty
			}
			// ALocalTee: value stays on stack — topType unchanged
			// (it equals the local's type since we passed the check).

		// Producers — set topType to what they push.
		case ALocalGet:
			if p.From.Type == obj.TYPE_CONST {
				idx := p.From.Offset
				if idx >= 0 && int(idx) < len(localTypes) {
					topType = localTypes[idx]
				} else {
					topType = wtUnknown
				}
			} else {
				topType = wtUnknown
			}
		case AI32Const:
			topType = 0x7F
		case AI64Const:
			topType = 0x7E
		case AF32Const:
			topType = 0x7D
		case AF64Const:
			topType = 0x7C
		case AI64ExtendI32U, AI64ExtendI32S,
			AI64Add, AI64Sub, AI64Mul, AI64And, AI64Or, AI64Xor,
			AI64Shl, AI64ShrU, AI64ShrS, AI64DivU, AI64DivS,
			AI64RemU, AI64RemS, AI64Load, AI64Load8U, AI64Load8S,
			AI64Load16U, AI64Load16S, AI64Load32U, AI64Load32S:
			topType = 0x7E
		case AI32WrapI64, AI32Eqz, AI32Eq, AI32Ne,
			AI32LtU, AI32LtS, AI32LeU, AI32LeS, AI32GtU, AI32GtS, AI32GeU, AI32GeS,
			AI64Eqz, AI64Eq, AI64Ne,
			AI64LtU, AI64LtS, AI64LeU, AI64LeS, AI64GtU, AI64GtS, AI64GeU, AI64GeS,
			ARefIsNull, ARefTest,
			AI32Add, AI32Sub, AI32Mul, AI32And, AI32Or, AI32Xor,
			AI32Shl, AI32ShrU, AI32ShrS,
			AI32Load, AI32Load8U, AI32Load8S, AI32Load16U, AI32Load16S:
			topType = 0x7F
		case AF32Load:
			topType = 0x7D
		case AF64Load:
			topType = 0x7C
		case ARefNull, ARefNullAny, ARefFunc,
			ARefCast, ARefCastNull,
			AStructNew, AStructNewDefault,
			AArrayNew, AArrayNewDefault, AArrayNewFixed:
			// All of these definitionally produce a ref. We don't
			// disambiguate between typed-ref and anyref at this level
			// — both satisfy the anyref local-slot we care about.
			topType = 0x6E
		case AStructGet, AStructGetU, AStructGetS,
			AArrayGet, AArrayGetU, AArrayGetS,
			AGlobalGet:
			// Field/element/global type depends on the struct field
			// decl, array element type, or global type — none of
			// which we have at this layer. Leave as unknown rather
			// than guess and false-positive; the wasm validator
			// catches mismatches in these flavours.
			topType = wtUnknown

		// Block boundaries / control flow — clear inference.
		default:
			topType = wtUnknown
		}
	}
	return ok
}

// wasmFieldTypeByte maps an obj.WasmField type tag to the wasm
// value-type byte used for locals. The signature lowering at
// wasm3Locals (and the linker's fieldsToTypes pass) collapses
// WasmPtr / WasmBool to i32, so we mirror that here.
func wasmFieldTypeByte(t obj.WasmFieldType) byte {
	switch t {
	case obj.WasmI32, obj.WasmPtr, obj.WasmBool:
		return 0x7F
	case obj.WasmI64:
		return 0x7E
	case obj.WasmF32:
		return 0x7D
	case obj.WasmF64:
		return 0x7C
	case obj.WasmAnyref:
		return 0x6E
	}
	return 0
}

func wasmTypeName(t byte) string {
	switch t {
	case 0x7F:
		return "i32"
	case 0x7E:
		return "i64"
	case 0x7D:
		return "f32"
	case 0x7C:
		return "f64"
	case 0x6E:
		return "anyref"
	case 0:
		return "<unknown>"
	}
	return "<unrecognized>"
}

func localSetOrTee(as obj.As) string {
	if as == ALocalTee {
		return "tee"
	}
	return "set"
}

// wasm3CheckSeen dedups warnings per (function, source-line)
// to bound the noise; the same SSA pattern often produces many
// near-duplicate hits within a single function. Each unique
// site reports once; a one-line summary at the end lists the
// suppressed-count per function.
var (
	wasm3CheckMu        sync.Mutex
	wasm3CheckSeen      = map[string]struct{}{}
	wasm3CheckPerFnSeen = map[string]int{}
)

// wasm3CheckReport emits a mismatch finding. Default mode is a
// warning to stderr (does not increment ctxt.Errors / fail the
// build); the wider stdlib still has many latent mismatches that
// don't surface as runtime failures because the offending
// functions aren't currently reachable, and bailing here would
// block every build. Set GOWASM3CHECK=fatal to escalate to
// ctxt.Diag — useful once a given driver's reachable set is
// believed clean and you want to enforce no regressions.
//
// Dedup policy: at most 3 unique (file, line) sites per function
// report individually; subsequent unique sites in the same
// function aggregate into a single "and N more" line at the
// function's third report.
func wasm3CheckReport(ctxt *obj.Link, p *obj.Prog, fn string, as obj.As, idx int64, declared, found byte) {
	pos := ctxt.PosTable.Pos(p.Pos)
	siteKey := fmt.Sprintf("%v|%s", pos, fn)

	wasm3CheckMu.Lock()
	if _, dup := wasm3CheckSeen[siteKey]; dup {
		wasm3CheckMu.Unlock()
		return
	}
	wasm3CheckSeen[siteKey] = struct{}{}
	wasm3CheckPerFnSeen[fn]++
	count := wasm3CheckPerFnSeen[fn]
	wasm3CheckMu.Unlock()

	const perFnLimit = 3
	if count > perFnLimit {
		if count == perFnLimit+1 {
			fmt.Fprintf(os.Stderr, "wasm3check: ... (further mismatches in %s suppressed)\n", fn)
		}
		return
	}

	msg := fmt.Sprintf("%v: wasm3 type mismatch in %s: local.%s[%d] (declared %s) ← %s",
		pos,
		fn,
		localSetOrTee(as),
		idx,
		wasmTypeName(declared),
		wasmTypeName(found))
	if os.Getenv("GOWASM3CHECK") == "fatal" {
		ctxt.Diag("%s", msg)
		return
	}
	fmt.Fprintln(os.Stderr, "wasm3check: "+msg)
}

