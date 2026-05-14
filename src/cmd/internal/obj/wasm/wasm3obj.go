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

import "cmd/internal/obj"

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
// Stage A: every function is emitted as the minimal valid body for the
// degenerate ()->() signature — zero local declarations followed by
// `end` (0x0b). asm3.go declares every function as ()->() until the
// compiler attaches typed signatures, so this body validates against
// its declared type. Stage C replaces this with real codegen: walking
// the obj.Prog stream, declaring typed locals, and encoding the
// instruction operands (including the 0xFB GC-opcode immediates).
func assemble3(ctxt *obj.Link, s *obj.LSym, newprog obj.ProgAlloc) {
	s.P = []byte{
		0x00, // local declaration count: 0
		0x0b, // end
	}
}
