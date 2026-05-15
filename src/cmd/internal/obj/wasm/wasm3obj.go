// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm

// wasm3obj.go is the obj backend for GOARCH=wasm3. Where wasmobj.go's
// preprocess/assemble are built end to end around the Go-stack-in-linear
// -memory model and the PC_F/PC_B virtual-PC resume scheme, wasm3
// functions are ordinary typed wasm functions: real params and results,
// no PC_B block-id parameter, no SP frame, no prologue branch table, no
// morestack check, and direct calls. See doc/wasm3-m2-design.md §2 and
// doc/wasm3-m2-cutover-notes.md §1.
//
// Status: work in progress on the wasm3-m2-cutover branch. This is
// Stage A of the cutover — make an empty main *link* into a
// structurally valid WebAssembly 3.0 module before making it *run*.
//
// The real wasm3 codegen — lowering the SSA-generated obj.Prog stream
// to the typed ABI, emitting the GC-opcode immediates (the operand
// encoding wasmobj.go's assemble has no case for), resolving direct
// call indices — is filled in rung by rung in Stage C. Until then
// preprocess3/assemble3 emit a minimal valid body for every function:
// the module's whole structure (the GC type section, imports,
// functions, globals, exports, code, data) is exercised and validated,
// and only the function *semantics* are still stubbed.

import (
	"bytes"
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"encoding/binary"
	"math"
)

// preprocess3 prepares a function for the wasm3 typed ABI.
//
// Stage A: it does not transform the body — assemble3 emits a stub — but
// it still has to leave the function in a shape the rest of the
// toolchain expects:
//
//   - record Args/Locals from the TEXT prog;
//   - assign a monotonic Pc to every prog and set s.Size, so the
//     pc/line table builder (cmd/internal/obj.linkpcln) sees a
//     consistent, non-negative pc range;
//   - materialize the wasmimport/wasmexport aux symbols, so functions
//     carrying those pragmas (the WASI syscalls) serialize without a
//     nil aux.
//
// Stage C replaces the body pass with the real typed-ABI lowering.
func preprocess3(ctxt *obj.Link, s *obj.LSym, newprog obj.ProgAlloc) {
	framesize := s.Func().Text.To.Offset
	if framesize < 0 {
		panic("bad framesize")
	}
	s.Func().Args = s.Func().Text.To.Val.(int32)
	s.Func().Locals = int32(framesize)

	pc := int64(0)
	for p := s.Func().Text; p != nil; p = p.Link {
		p.Pc = pc
		pc++
	}
	s.Size = pc

	if wi := s.Func().WasmImport; wi != nil {
		wi.CreateAuxSym()
	}
	if we := s.Func().WasmExport; we != nil {
		we.CreateAuxSym()
	}
	if wt := s.Func().WasmType; wt != nil {
		wt.CreateAuxSym()
	}
}

// assemble3 encodes a function body for GOARCH=wasm3.
//
// Stage C: assemble3 walks the obj.Prog stream the wasm3 SSA backend
// produced and encodes as much of it as the current rung of the
// bring-up ladder supports. The first rung handles only the trivial
// body — the no-op progs the backend threads through every function
// (TEXT/FUNCDATA/PCDATA/RESUMEPOINT) and the terminating RET of a
// ()->() function.
//
// A function that contains any instruction this rung does not yet
// understand falls back to the degenerate ()->() stub body (zero
// locals, bare `end`), exactly as Stage A/B emitted for every
// function. That keeps an empty main — whose own body is trivial and
// is now genuinely walked — buildable and runnable while the ladder is
// climbed: each later rung (arithmetic, then struct/pointer) teaches
// assemble3 more instruction encodings, including the 0xFB GC-opcode
// immediates, and more functions graduate from the stub to a real
// body. asm3.go declares every function as ()->() until the compiler
// attaches typed signatures, so both the stub and a trivial walked
// body validate against the declared type.
//
// The entry symbol is the one exception (Stage B): it is given a real
// body that calls main.main, so an empty Go program runs end to end.
func assemble3(ctxt *obj.Link, s *obj.LSym, newprog obj.ProgAlloc) {
	if s.Name == entrySym {
		assembleWasm3Entry(ctxt, s)
		return
	}

	if body, ok := encodeWasm3Body(ctxt, s); ok {
		s.P = body
		return
	}

	// Not yet encodable at this rung: emit the degenerate stub. The
	// `unreachable` is what makes the stub valid against *any* declared
	// signature — once the compiler attaches a real typed signature
	// (e.g. (i64,i64)->i64), a bare `end` would fail validation because
	// the wasm stack would not hold the declared results, but an
	// unreachable body type-checks against every signature. A function
	// that traps here is one whose real body lands in a later rung.
	s.P = []byte{
		0x00, // local declaration count: 0
		0x00, // unreachable
		0x0b, // end
	}
}

// encodeWasm3Body walks s's obj.Prog stream and encodes it as a typed
// wasm function body, returning ok=false (and no body, no relocations
// added) the moment it meets an instruction the current rung does not
// support, so assemble3 can fall back to the stub.
//
// Stage C.2b — the arithmetic rung — handles a leaf function: register
// access (Get/Set/Tee of a parameter register), integer and floating
// constants, direct calls, the operand-less arithmetic/comparison/
// conversion opcodes, and RET. A function that touches the linear-
// memory frame (Get SP, loads, stores), uses a register that is not a
// parameter register, branches, or makes an indirect call still falls
// back to the unreachable stub; those are later rungs.
func encodeWasm3Body(ctxt *obj.Link, s *obj.LSym) (body []byte, ok bool) {
	localOf, decls, ok := wasm3Locals(s)
	if !ok {
		return nil, false
	}

	w := new(bytes.Buffer)
	writeUleb128(w, uint64(len(decls)))
	for _, d := range decls {
		writeUleb128(w, d.count)
		w.WriteByte(d.typ)
	}

	var relocs []obj.Reloc
	sawRet := false
	for p := s.Func().Text; p != nil; p = p.Link {
		switch p.As {
		case obj.ATEXT, obj.AFUNCDATA, obj.APCDATA, obj.ANOP, ANop, ARESUMEPOINT:
			// No body contribution: ATEXT carries the signature,
			// FUNCDATA/PCDATA carry metadata, and RESUMEPOINT is the
			// Go-stack ABI's block marker, which the wasm3 typed ABI
			// has no use for.

		case obj.ARET:
			writeOpcode(w, AReturn)
			sawRet = true

		case obj.AUNDEF:
			writeOpcode(w, AUnreachable)

		case AGet:
			idx, isLocal := localOf[p.From.Reg]
			if !isLocal {
				return nil, false
			}
			writeOpcode(w, ALocalGet)
			writeUleb128(w, idx)

		case ASet, ATee:
			idx, isLocal := localOf[p.To.Reg]
			if !isLocal {
				return nil, false
			}
			if p.As == ASet {
				writeOpcode(w, ALocalSet)
			} else {
				writeOpcode(w, ALocalTee)
			}
			writeUleb128(w, idx)

		case AI32Const, AI64Const:
			if p.From.Type != obj.TYPE_CONST || p.From.Name != obj.NAME_NONE {
				// A symbol address (NAME_EXTERN) needs an R_ADDR
				// relocation; that is a later rung.
				return nil, false
			}
			writeOpcode(w, p.As)
			writeSleb128(w, p.From.Offset)

		case AF32Const:
			writeOpcode(w, p.As)
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], math.Float32bits(float32(p.From.Val.(float64))))
			w.Write(b[:])

		case AF64Const:
			writeOpcode(w, p.As)
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], math.Float64bits(p.From.Val.(float64)))
			w.Write(b[:])

		case obj.ACALL:
			if p.To.Type != obj.TYPE_MEM || (p.To.Name != obj.NAME_EXTERN && p.To.Name != obj.NAME_STATIC) {
				// An indirect call needs call_indirect plus the type
				// table; that is a later rung.
				return nil, false
			}
			writeOpcode(w, ACall)
			relocs = append(relocs, obj.Reloc{
				Type: objabi.R_CALL,
				Off:  int32(w.Len()),
				Siz:  1, // variable-sized; the linker writes the function index
				Sym:  p.To.Sym,
			})

		case ANot:
			writeOpcode(w, AI32Eqz)

		default:
			// The operand-less wasm stack instructions — the
			// arithmetic, comparison and conversion opcodes the SSA
			// backend emits with no From/To — encode as a single
			// opcode byte. Anything carrying an operand (control-flow
			// structure, loads, stores, memory ops) needs immediate
			// encoding this rung does not do.
			if p.From.Type != obj.TYPE_NONE || p.To.Type != obj.TYPE_NONE {
				return nil, false
			}
			if p.As < AUnreachable || p.As >= ALast {
				return nil, false
			}
			switch p.As {
			case ABlock, ALoop, AIf, AElse, AEnd, ABr, ABrIf, ABrTable,
				ACall, ACallIndirect, AReturnCallRef:
				return nil, false
			}
			writeOpcode(w, p.As)
		}
	}

	if !sawRet {
		// A real function body always ends in a return (BlockRet, or a
		// tail call lowered to RET). A prog stream with none — e.g. the
		// not-yet-generated //go:wasmexport wrapper, which is just
		// TEXT/FUNCDATA — is an incomplete function; fall back to the
		// stub rather than emit a body that drops off the end without
		// producing the declared results.
		return nil, false
	}

	w.WriteByte(0x0b) // end

	for _, r := range relocs {
		s.AddRel(ctxt, r)
	}
	return w.Bytes(), true
}

// wasm3LocalDecl is one entry of a wasm function body's local
// declaration vector: count locals of the given wasm value type.
type wasm3LocalDecl struct {
	count uint64
	typ   byte
}

// wasm3Locals builds the register-to-wasm-local-index map for s and
// the body's local declaration vector.
//
// A wasm function's parameters are locals 0..nparams-1 in signature
// order; the register ABI assigns the integer parameters to R0, R1, …
// and the floating parameters to F0, F1, …, so that mapping is
// reconstructed from the parameter storage types in whichever
// signature aux s carries — an obj.WasmType for an ordinary wasm3
// function, or the WasmExport/WasmImport aux for a function bearing
// those pragmas. ok is false if s carries none (a function the
// compiler left as ()->(), or a hand-written stub): with no known
// parameter layout it is left to the stub.
//
// Every other local-class register (R0-R15, F0-F31) the body
// references becomes a declared local, numbered after the parameters
// in first-use order, with one declaration entry each. Registers that
// are not local-class — SP, g, CTXT — are not mapped; a body using one
// makes encodeWasm3Body fall back, since the linear-memory frame is a
// later rung.
func wasm3Locals(s *obj.LSym) (localOf map[int16]uint64, decls []wasm3LocalDecl, ok bool) {
	fn := s.Func()
	var params []obj.WasmField
	switch {
	case fn.WasmType != nil:
		params = fn.WasmType.Params
	case fn.WasmExport != nil:
		params = fn.WasmExport.Params
	case fn.WasmImport != nil:
		params = fn.WasmImport.Params
	default:
		return nil, nil, false
	}

	localOf = make(map[int16]uint64)
	next := uint64(0)
	var intParam, floatParam int16
	for _, f := range params {
		var reg int16
		switch f.Type {
		case obj.WasmF32, obj.WasmF64:
			reg = REG_F0 + floatParam
			floatParam++
		default: // WasmI32, WasmI64, WasmPtr, WasmBool, WasmRef
			reg = REG_R0 + intParam
			intParam++
		}
		localOf[reg] = next
		next++
	}

	declare := func(reg int16) {
		if reg < REG_R0 || reg > REG_F31 {
			return // not a local-class register (SP, g, CTXT, …)
		}
		if _, done := localOf[reg]; done {
			return
		}
		localOf[reg] = next
		next++
		decls = append(decls, wasm3LocalDecl{count: 1, typ: byte(regType(reg))})
	}
	for p := fn.Text; p != nil; p = p.Link {
		switch p.As {
		case AGet:
			declare(p.From.Reg)
		case ASet, ATee:
			declare(p.To.Reg)
		}
	}
	return localOf, decls, true
}

// entrySym is the wasip1 entry symbol; cmd/link exports it as "_start".
// The name follows the _rt0_<GOARCH>_<GOOS> convention.
const entrySym = "_rt0_wasm3_wasip1"

// assembleWasm3Entry gives the entry symbol a degenerate bootstrap body
// for Stage B of the cutover: call main.main, then return. WASI treats
// a normal return from _start as a clean (exit 0) termination, and an
// empty main.main needs no runtime initialization, so the whole
// schedinit/scheduler/allocator bootstrap is skipped for now. It is
// reintroduced incrementally as the runtime fork lands (Stage B proper).
//
// The body is:
//
//	local declaration count: 0
//	call <main.main>          ; operand filled in by the R_CALL reloc
//	end
//
// The call operand is variable-length and written by the linker, so the
// obj backend emits only the 0x10 opcode and records an R_CALL reloc at
// the byte that follows it — the same scheme assemble uses for calls.
func assembleWasm3Entry(ctxt *obj.Link, s *obj.LSym) {
	s.P = []byte{
		0x00, // local declaration count: 0
		0x10, // call
		0x0b, // end
	}
	s.AddRel(ctxt, obj.Reloc{
		Type: objabi.R_CALL,
		Off:  2, // immediately after the 0x10 call opcode
		Siz:  1, // variable-sized; the linker writes the function index
		Sym:  ctxt.LookupABI("main.main", obj.ABIInternal),
	})
}
