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

	if body, ok := encodeWasm3Body(s); ok {
		s.P = body
		return
	}

	// Not yet encodable at this rung: emit the degenerate stub.
	s.P = []byte{
		0x00, // local declaration count: 0
		0x0b, // end
	}
}

// encodeWasm3Body walks s's obj.Prog stream and encodes it as a typed
// wasm function body, returning ok=false (and no body) the moment it
// meets an instruction the current rung does not support, so assemble3
// can fall back to the stub. See assemble3's comment for the staging
// rationale.
func encodeWasm3Body(s *obj.LSym) (body []byte, ok bool) {
	w := new(bytes.Buffer)
	w.WriteByte(0x00) // local declaration count: 0

	for p := s.Func().Text; p != nil; p = p.Link {
		switch p.As {
		case obj.ATEXT, obj.AFUNCDATA, obj.APCDATA, obj.ANOP, ANop, ARESUMEPOINT:
			// No body contribution: ATEXT carries the signature,
			// FUNCDATA/PCDATA carry metadata, and RESUMEPOINT is the
			// Go-stack ABI's block marker, which the wasm3 typed ABI
			// has no use for.
			continue

		case obj.ARET:
			// Trivial ()->() function: a bare return. Typed results
			// land in a later rung once signatures carry them.
			writeOpcode(w, AReturn)
			continue

		default:
			return nil, false
		}
	}

	w.WriteByte(0x0b) // end
	return w.Bytes(), true
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
