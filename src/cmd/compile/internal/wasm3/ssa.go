// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package wasm3 is the compiler backend for GOARCH=wasm3.
//
// For the M2 pure-refactor checkpoint it is a copy of the wasm backend
// (cmd/compile/internal/wasm); the cutover to the WebAssembly 3.0 object
// model — typed-function calling convention, GC types, struct.new
// allocation — lands later in milestone M2. See doc/wasm3-m2-design.md.
package wasm3

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/logopt"
	"cmd/compile/internal/objw"
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/ssagen"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/obj/wasm"
	"cmd/internal/src"
	"cmd/internal/wasmgc"
)

/*

   Wasm implementation
   -------------------

   Wasm is a strange Go port because the machine isn't
   a register-based machine, threads are different, code paths
   are different, etc. We outline those differences here.

   See the design doc for some additional info on this topic.
   https://docs.google.com/document/d/131vjr4DH6JFnb-blm_uRdaC0_Nv3OUwjEY5qVCxCup4/edit#heading=h.mjo1bish3xni

   PCs:

   Wasm doesn't have PCs in the normal sense that you can jump
   to or call to. Instead, we simulate these PCs using our own construct.

   A PC in the Wasm implementation is the combination of a function
   ID and a block ID within that function. The function ID is an index
   into a function table which transfers control to the start of the
   function in question, and the block ID is a sequential integer
   indicating where in the function we are.

   Every function starts with a branch table which transfers control
   to the place in the function indicated by the block ID. The block
   ID is provided to the function as the sole Wasm argument.

   Block IDs do not encode every possible PC. They only encode places
   in the function where it might be suspended. Typically these places
   are call sites.

   Sometimes we encode the function ID and block ID separately. When
   recorded together as a single integer, we use the value F<<16+B.

   Threads:

   Wasm doesn't (yet) have threads. We have to simulate threads by
   keeping goroutine stacks in linear memory and unwinding
   the Wasm stack each time we want to switch goroutines.

   To support unwinding a stack, each function call returns on the Wasm
   stack a boolean that tells the function whether it should return
   immediately or not. When returning immediately, a return address
   is left on the top of the Go stack indicating where the goroutine
   should be resumed.

   Stack pointer:

   There is a single global stack pointer which records the stack pointer
   used by the currently active goroutine. This is just an address in
   linear memory where the Go runtime is maintaining the stack for that
   goroutine.

   Functions cache the global stack pointer in a local variable for
   faster access, but any changes must be spilled to the global variable
   before any call and restored from the global variable after any call.

   Calling convention:

   All Go arguments and return values are passed on the Go stack, not
   the wasm stack. In addition, return addresses are pushed on the
   Go stack at every call point. Return addresses are not used during
   normal execution, they are used only when resuming goroutines.
   (So they are not really a "return address", they are a "resume address".)

   All Go functions have the Wasm type (i32)->i32. The argument
   is the block ID and the return value is the exit immediately flag.

   Callsite:
    - write arguments to the Go stack (starting at SP+0)
    - push return address to Go stack (8 bytes)
    - write local SP to global SP
    - push 0 (type i32) to Wasm stack
    - issue Call
    - restore local SP from global SP
    - pop int32 from top of Wasm stack. If nonzero, exit function immediately.
    - use results from Go stack (starting at SP+sizeof(args))
       - note that the callee will have popped the return address

   Prologue:
    - initialize local SP from global SP
    - jump to the location indicated by the block ID argument
      (which appears in local variable 0)
    - at block 0
      - check for Go stack overflow, call morestack if needed
      - subtract frame size from SP
      - note that arguments now start at SP+framesize+8

   Normal epilogue:
    - pop frame from Go stack
    - pop return address from Go stack
    - push 0 (type i32) on the Wasm stack
    - return
   Exit immediately epilogue:
    - push 1 (type i32) on the Wasm stack
    - return
    - note that the return address and stack frame are left on the Go stack

   The main loop that executes goroutines is wasm_pc_f_loop, in
   runtime/rt0_js_wasm.s. It grabs the saved return address from
   the top of the Go stack (actually SP-8?), splits it up into F
   and B parts, then calls F with its Wasm argument set to B.

   Note that when resuming a goroutine, only the most recent function
   invocation of that goroutine appears on the Wasm stack. When that
   Wasm function returns normally, the next most recent frame will
   then be started up by wasm_pc_f_loop.

   Global 0 is SP (stack pointer)
   Global 1 is CTXT (closure pointer)
   Global 2 is GP (goroutine pointer)
*/

func Init(arch *ssagen.ArchInfo) {
	arch.LinkArch = &wasm.Linkwasm3
	arch.REGSP = wasm.REG_SP
	arch.MAXWIDTH = 1 << 50

	arch.ZeroRange = zeroRange
	arch.Ginsnop = ginsnop

	arch.SSAMarkMoves = ssaMarkMoves
	arch.SSAGenValue = ssaGenValue
	arch.SSAGenBlock = ssaGenBlock
	arch.SpillArgReg = spillArgReg
	arch.LoadRegResult = loadRegResult

	arch.PrepareFunc = attachWasmType
}

// spillArgReg and loadRegResult bridge register-resident arguments and
// results to the Go stack frame on the register architectures. They
// exist to keep pointer arguments visible to the Go garbage collector
// (the part-live-args spill) and to reload results on the open-coded
// defer recover path.
//
// M2 cutover, Stage C.2: neither applies to wasm3. Its objects live on
// the host GC heap, not in a linear-memory frame the Go collector
// scans, so there is nothing to spill for liveness (the design's §8
// blocker fixes suppress FUNCDATA_LocalsPointerMaps outright), and
// defer/recover is a later milestone. Both are no-ops, but must be
// non-nil because ssagen invokes them unconditionally once a function
// has register parameters.
func spillArgReg(pp *objw.Progs, p *obj.Prog, f *ssa.Func, t *types.Type, reg int16, n *ir.Name, off int64) *obj.Prog {
	return p
}

func loadRegResult(s *ssagen.State, f *ssa.Func, t *types.Type, reg int16, n *ir.Name, off int64) *obj.Prog {
	return nil
}

func zeroRange(pp *objw.Progs, p *obj.Prog, off, cnt int64, state *uint32) *obj.Prog {
	if cnt == 0 {
		return p
	}
	if cnt%8 != 0 {
		base.Fatalf("zerorange count not a multiple of widthptr %d", cnt)
	}

	for i := int64(0); i < cnt; i += 8 {
		p = pp.Append(p, wasm.AGet, obj.TYPE_REG, wasm.REG_SP, 0, 0, 0, 0)
		p = pp.Append(p, wasm.AI64Const, obj.TYPE_CONST, 0, 0, 0, 0, 0)
		p = pp.Append(p, wasm.AI64Store, 0, 0, 0, obj.TYPE_CONST, 0, off+i)
	}

	return p
}

func ginsnop(pp *objw.Progs) *obj.Prog {
	return pp.Prog(wasm.ANop)
}

func ssaMarkMoves(s *ssagen.State, b *ssa.Block) {
}

func ssaGenBlock(s *ssagen.State, b, next *ssa.Block) {
	// Trigger the relooper analysis on the first block of each
	// function; the cached plan is the foundation later commits will
	// drive scope-aware emission from. Shadow run for now — the plan
	// is computed and stashed but does not affect codegen.
	if b == b.Func.Entry {
		planForFunc(b.Func)
	}
	switch b.Kind {
	case ssa.BlockPlain, ssa.BlockDefer:
		emitPhiCopies(s, b, b.Succs[0].Block())
		if next != b.Succs[0].Block() {
			s.Br(obj.AJMP, b.Succs[0].Block())
		}

	case ssa.BlockIf:
		switch next {
		case b.Succs[0].Block():
			// if false, jump to b.Succs[1]
			getValue32(s, b.Controls[0])
			s.Prog(wasm.AI32Eqz)
			s.Prog(wasm.AIf)
			emitPhiCopies(s, b, b.Succs[1].Block())
			s.Br(obj.AJMP, b.Succs[1].Block())
			s.Prog(wasm.AEnd)
			emitPhiCopies(s, b, b.Succs[0].Block())
		case b.Succs[1].Block():
			// if true, jump to b.Succs[0]
			getValue32(s, b.Controls[0])
			s.Prog(wasm.AIf)
			emitPhiCopies(s, b, b.Succs[0].Block())
			s.Br(obj.AJMP, b.Succs[0].Block())
			s.Prog(wasm.AEnd)
			emitPhiCopies(s, b, b.Succs[1].Block())
		default:
			// if true, jump to b.Succs[0], else jump to b.Succs[1]
			getValue32(s, b.Controls[0])
			s.Prog(wasm.AIf)
			emitPhiCopies(s, b, b.Succs[0].Block())
			s.Br(obj.AJMP, b.Succs[0].Block())
			s.Prog(wasm.AEnd)
			emitPhiCopies(s, b, b.Succs[1].Block())
			s.Br(obj.AJMP, b.Succs[1].Block())
		}

	case ssa.BlockRet:
		// M2 cutover, Stage C.2: with the typed ABI a wasm `return`
		// takes the function's results from the wasm stack. When the
		// function has results, b.Controls[0] is the OpMakeResult whose
		// args are the result values followed by the memory value;
		// push each result value onto the wasm stack in order before
		// returning. This also balances OnWasmStackSkipped: a result
		// value may be marked OnWasmStack (single-use, feeds only the
		// codegen-nothing OpMakeResult), so getValue is what actually
		// consumes it.
		//
		// Width-faithful ABI: a sub-word integer result is narrowed to
		// i32 at the boundary. The width comes from the result's
		// per-register Go type, mirroring the call-site argument
		// narrowing: a struct-value result expands to one register per
		// scalar field with its own width.
		if len(b.Controls) != 0 {
			mr := b.Controls[0]
			argIdx := 0
			for _, p := range b.Func.OwnAux.ABIInfo().OutParams() {
				regTypes, _ := p.RegisterTypesAndOffsets()
				for ri := range p.Registers {
					a := mr.Args[argIdx]
					argIdx++
					narrow := wasm3NarrowABI(regTypes[ri])
					if narrow {
						getValue32(s, a)
					} else {
						getValue64(s, a)
					}
				}
			}
		}
		s.Prog(obj.ARET)

	case ssa.BlockExit, ssa.BlockRetJmp:
		// A block with no successor is one whose last call does not
		// return — runtime.panicdivide, runtime.gopanic and the like.
		// The wasm validator does not know that, so without an explicit
		// terminator the bytes that follow (a fall-through into the
		// next basic block, or the function's outer `end`) would have
		// to typecheck against the function's declared result type.
		// `unreachable` makes the stack polymorphic so anything
		// validates, and traps if it is ever reached.
		s.Prog(obj.AUNDEF)

	default:
		panic("unexpected block")
	}

	// Entry point for the next block. Used by the JMP in goToBlock.
	s.Prog(wasm.ARESUMEPOINT)

	if s.OnWasmStackSkipped != 0 {
		nm := "?"
		if fi := s.FuncInfo(); fi != nil && fi.Text != nil && fi.Text.From.Sym != nil {
			nm = fi.Text.From.Sym.Name
		}
		panic("wasm: bad stack in " + nm)
	}
}

func ssaGenValue(s *ssagen.State, v *ssa.Value) {
	switch v.Op {
	case ssa.OpWasm3LoweredStaticCall, ssa.OpWasm3LoweredClosureCall, ssa.OpWasm3LoweredInterCall, ssa.OpWasm3LoweredTailCall, ssa.OpWasm3LoweredTailCallInter:
		s.PrepareCall(v)
		call, _ := v.Aux.(*ssa.AuxCall)
		if call != nil && call.Fn == ir.Syms.Deferreturn {
			// The runtime needs to inject jumps to
			// deferreturn calls using the address in
			// _func.deferreturn. Hence, the call to
			// deferreturn must itself be a resumption
			// point so it gets a target PC.
			s.Prog(wasm.ARESUMEPOINT)
		}
		// Stage G: for wasm3 closure calls (closure arg is func or
		// *func), set CTXT from the closureCtx struct's captures
		// pointer (field 1). For bare functions, this stores zero
		// — harmless. For closures with captures, this gives the
		// callee body access to its captures via the existing
		// CTXT-relative load path. The setReg goes through the
		// wasm3GlobalIndex helper in the obj-encoder, which now
		// recognises REG_CTXT and emits `global.set 1`.
		//
		// The legacy path (closure arg is *struct{F, captures...})
		// also sets CTXT from the closure pointer directly. Those
		// calls take the wasm1 trampoline below (which still bails)
		// but at least the setReg now succeeds rather than failing
		// the encoder.
		if v.Op == ssa.OpWasm3LoweredClosureCall {
			if wasm3FuncTypeOf(v.Args[1].Type) != nil && ssa.Wasm3IsAnyrefValue(v.Args[1]) {
				closureArg := v.Args[1]
				funcType := wasm3FuncTypeOf(closureArg.Type)
				closureCtxIdx := wasm3RegisterClosureCtx(s.FuncInfo(), funcType)
				// Captures-in-struct prep (doc/wasm3-m3-captures-in-
				// struct.md piece 5): also set CTXT_REF = closure-ref,
				// so closure bodies migrated to the new prologue can
				// recover their concrete closureCtx via global.get +
				// ref.cast. Legacy bodies that still read CTXT (the
				// captures-ptr) keep working — the per-signature base
				// has captures-ptr at field 1, and per-closure
				// subtypes preserve that slot.
				getValue64(s, closureArg)
				pSetRef := s.Prog(wasm.AGlobalSet)
				pSetRef.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm.Wasm3GlobalIndexCtxRef)}
				getValue64(s, closureArg)
				cast := s.Prog(wasm.ARefCast)
				cast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(closureCtxIdx)}
				get := s.Prog(wasm.AStructGet)
				get.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(closureCtxIdx)}
				get.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
				setReg(s, wasm.REG_CTXT)
			} else if wasm3FuncTypeOf(v.Args[1].Type) != nil {
				// closureArg is TFUNC-typed but i64 (a raw codeptr
				// loaded from a linear-memory global var like
				// `var f func(...)`, not a wasmgc closureCtx ref).
				// There are no captures — skip the CTXT/CTXT_REF
				// setup; the i64 codeptr will be routed through
				// call_indirect at the call-emission site below.
			} else {
				getValue64(s, v.Args[1])
				setReg(s, wasm.REG_CTXT)
			}
		}

		// M2 cutover, Stage C.2: bridge Go's register ABI to wasm's
		// stack-machine call. expand_calls appended each register-
		// resident argument to the call value as an explicit arg, so
		// v.Args is [fixed inputs..., register args..., mem]. Go passes
		// those arguments "in registers"; a wasm `call` instead takes
		// them from the operand stack, so each one is pushed here. The
		// fixed leading inputs (a code pointer, and for a closure call
		// the closure word) are not pushed — an indirect call takes its
		// code pointer last, below.
		firstRegArg := 0
		switch v.Op {
		case ssa.OpWasm3LoweredInterCall, ssa.OpWasm3LoweredTailCallInter:
			firstRegArg = 1 // arg0 = code pointer
		case ssa.OpWasm3LoweredClosureCall:
			firstRegArg = 2 // arg0 = code pointer, arg1 = closure
		}
		// Width-faithful ABI: a sub-word integer argument is narrowed to
		// i32 at the boundary, everything else crosses at its register
		// width. The width is taken per-register from the parameter's
		// per-register Go type — a struct-value param expands to one
		// register per field, and each field's own width drives the
		// narrowing. (For a scalar Go param this collapses to "the
		// parameter's declared type" since RegisterTypes returns a
		// single-element slice. The narrowness is taken from the type,
		// not the SSA value: a no-op conversion such as int32(x) leaves
		// the value typed int even though the slot it fills is i32.)
		//
		// A //go:wasmimport call crosses the host boundary: the import
		// signature carries WasmPtr/WasmBool fields (i32 from the
		// linker's view), so the call site narrows pointer-shaped
		// register values to i32 too. paramsToWasmFields and the
		// wasmimport stub follow the same convention.
		argIdx := firstRegArg
		var calleeWasmImport *obj.WasmImport
		if call != nil && call.Fn != nil {
			if fi := call.Fn.Func(); fi != nil {
				calleeWasmImport = fi.WasmImport
			}
		}
		wasmFieldIdx := 0
		for _, p := range call.ABIInfo().InParams() {
			regTypes, _ := p.RegisterTypesAndOffsets()
			for ri := range p.Registers {
				a := v.Args[argIdx]
				argIdx++
				narrow := wasm3NarrowABI(regTypes[ri])
				if calleeWasmImport != nil && wasmFieldIdx < len(calleeWasmImport.Params) {
					narrow = isNarrowWasmField(calleeWasmImport.Params[wasmFieldIdx])
				}
				wasmFieldIdx++
				if narrow {
					getValue32(s, a)
				} else {
					getValue64(s, a)
				}
			}
		}

		if call != nil && call.Fn != nil {
			sym := call.Fn
			p := s.Prog(obj.ACALL)
			p.To = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: sym}
			p.Pos = v.Pos
			if v.Op == ssa.OpWasm3LoweredTailCall {
				p.As = obj.ARET
			}
		} else if v.Op == ssa.OpWasm3LoweredClosureCall && wasm3FuncTypeOf(v.Args[1].Type) != nil && ssa.Wasm3IsAnyrefValue(v.Args[1]) {
			// Stage G closure call. Drop the dummy codeptr arg,
			// then extract the funcref from the closureCtx and
			// call_ref. v.Args[0] is the dummy SSA-level codeptr
			// (constInt(0) from ssagen's wasm3 path); consume +
			// drop to balance OnWasmStack.
			//
			// The CTXT-set (struct.get field 1 → global.set 1) for
			// closures-with-captures already happened above the
			// register-args loop; see wasm3SetClosureCtxFromArg
			// (called from the pre-args branch).
			getValue64(s, v.Args[0])
			s.Prog(wasm.ADrop)
			closureArg := v.Args[1]
			funcType := wasm3FuncTypeOf(closureArg.Type)
			closureCtxIdx := wasm3RegisterClosureCtx(s.FuncInfo(), funcType)
			funcIdx := wasm3RegisterFuncSig(s.FuncInfo(), funcType)
			getValue64(s, closureArg)
			cast := s.Prog(wasm.ARefCast)
			cast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(closureCtxIdx)}
			get := s.Prog(wasm.AStructGet)
			get.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(closureCtxIdx)}
			get.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
			callRef := s.Prog(wasm.ACallRef)
			callRef.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(funcIdx)}
			callRef.Pos = v.Pos
		} else if v.Op == ssa.OpWasm3LoweredClosureCall && wasm3FuncTypeOf(v.Args[1].Type) != nil {
			// Closure call on a TFUNC value that is i64-shaped (a
			// raw codeptr loaded from a linear-memory global like
			// `var f func(...)`, not a wasmgc closureCtx). Route
			// through the same call_indirect path the interface
			// dispatch uses: drop the dummy codeptr arg, push the
			// loaded codeptr, decode `funcidx << 16` back to the
			// table index with `>> 16; i32.wrap`, then
			// call_indirect on the synthetic func type.
			getValue64(s, v.Args[0])
			s.Prog(wasm.ADrop)
			getValue64(s, v.Args[1])
			p1 := s.Prog(wasm.AI64Const)
			p1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: 16}
			s.Prog(wasm.AI64ShrU)
			s.Prog(wasm.AI32WrapI64)
			pCall := s.Prog(wasm.ACallIndirect)
			ft := wasm3SyntheticFuncType(call)
			pCall.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterFuncSig(s.FuncInfo(), ft))}
			pCall.Pos = v.Pos
		} else {
			// Stage F (doc/wasm3-m3-stage-f-interfaces.md): an
			// indirect call where the codeptr is on the wasm
			// stack — typically the funcref loaded from an itab
			// at an interface method dispatch. Emit
			// `<codeptr i64>; i64.const 16; i64.shr_u; i32.wrap;
			// call_indirect <typeidx> <table=0>`. The codeptr's
			// `funcid << 16` encoding (see assignAddress in the
			// wasm linker) means `>> 16` recovers the table
			// index directly: writeElementSec3 populates the
			// table starting at offset funcValueOffset, the same
			// constant that's baked into the symbol-value
			// encoding.
			getValue64(s, v.Args[0])
			p1 := s.Prog(wasm.AI64Const)
			p1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: 16}
			s.Prog(wasm.AI64ShrU)
			s.Prog(wasm.AI32WrapI64)
			pCall := s.Prog(wasm.ACallIndirect)
			ft := wasm3SyntheticFuncType(call)
			pCall.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterFuncSig(s.FuncInfo(), ft))}
			pCall.Pos = v.Pos
			if v.Op == ssa.OpWasm3LoweredTailCallInter {
				// Tail call: ACallIndirect followed by RET works
				// for now (no return_call_indirect on every engine
				// we target yet). Treated as a normal call by the
				// register-result-pop loop below.
				// TODO: emit return_call_indirect once supported.
			}
		}

		// Move the register-resident results off the wasm stack into
		// their result registers. wasm leaves results on the operand
		// stack with the last result on top, so they are popped in
		// reverse. As with arguments, narrowness is per-register: a
		// composite-typed result expands to one register per scalar
		// field, each driven by its own width. A sub-word integer
		// result arrives as an i32 and is widened to the i64 register
		// width before being stored, the mirror of the parameter-
		// widening prologue. A tail call does not return here.
		if call != nil && v.Op != ssa.OpWasm3LoweredTailCall && v.Op != ssa.OpWasm3LoweredTailCallInter {
			type resultReg struct {
				reg    int16
				narrow bool
			}
			var regs []resultReg
			wasmResultIdx := 0
			for _, p := range call.ABIInfo().OutParams() {
				regTypes, _ := p.RegisterTypesAndOffsets()
				for ri, r := range p.Registers {
					narrow := wasm3NarrowABI(regTypes[ri])
					if calleeWasmImport != nil && wasmResultIdx < len(calleeWasmImport.Results) {
						narrow = isNarrowWasmField(calleeWasmImport.Results[wasmResultIdx])
					}
					wasmResultIdx++
					regs = append(regs, resultReg{ssa.ObjRegForAbiReg(r, v.Block.Func.Config), narrow})
				}
			}
			// Find the OpSelectN value (if any) that downstream
			// code uses to read result i, so we can write the
			// result straight into its per-value local under the
			// M3 regalloc-bypass scheme. The selectors are
			// scheduled in the same block as the call
			// (tightenTupleSelectors enforces this). A nil here
			// means no selector reads this result, or the
			// selector has no per-value local — fall back to the
			// register-local path so regalloc-driven Phi
			// resolution still sees the value.
			selectN := make([]*ssa.Value, len(regs))
			for _, u := range v.Block.Values {
				if u.Op != ssa.OpSelectN {
					continue
				}
				if len(u.Args) < 1 || u.Args[0] != v {
					continue
				}
				if u.AuxInt < 0 || int(u.AuxInt) >= len(regs) {
					continue
				}
				selectN[u.AuxInt] = u
			}
			for i := len(regs) - 1; i >= 0; i-- {
				if regs[i].narrow {
					s.Prog(wasm.AI64ExtendI32U)
				}
				placed := false
				if sel := selectN[i]; sel != nil {
					if idx, ok := wasm3ValueLocalIdx(s, sel); ok {
						localSetIdx(s, idx)
						placed = true
					}
				} else {
					// Void-context call: no SSA reader for result i.
					// Falling through to setReg would stash the value
					// into a register-local; the wasm3 obj-encoder's
					// reg → local mapping can place that local at the
					// same slot as a caller param of an incompatible
					// wasm type (e.g. on a `func gwrite(b []byte)`
					// whose only param is anyref-typed local 0, an
					// unused i32 call result lands at `local.set 0`
					// — i64 into anyref, failing validation). Drop
					// the value instead so no local store happens.
					s.Prog(wasm.ADrop)
					placed = true
				}
				if !placed {
					setReg(s, regs[i].reg)
				}
			}
		}

	case ssa.OpWasm3LoweredMove:
		getValue32(s, v.Args[0])
		getValue32(s, v.Args[1])
		i32Const(s, int32(v.AuxInt))
		s.Prog(wasm.AMemoryCopy)

	case ssa.OpWasm3LoweredZero:
		getValue32(s, v.Args[0])
		i32Const(s, 0)
		i32Const(s, int32(v.AuxInt))
		s.Prog(wasm.AMemoryFill)

	case ssa.OpWasm3LoweredNilCheck:
		// Stage E phase 4 (wasm3): when arg0 is anyref (the common
		// case for wasmgc-backed slice / array backings), `i64.eqz`
		// is invalid wasm. Emit `ref.is_null` instead — same
		// boolean semantics (true when the ref is null).
		getValue64(s, v.Args[0])
		if ssa.Wasm3IsAnyrefValue(v.Args[0]) {
			s.Prog(wasm.ARefIsNull)
		} else {
			s.Prog(wasm.AI64Eqz)
		}
		s.Prog(wasm.AIf)
		p := s.Prog(wasm.ACALLNORESUME)
		p.To = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: ir.Syms.SigPanic}
		s.Prog(wasm.AEnd)
		if logopt.Enabled() {
			logopt.LogOpt(v.Pos, "nilcheck", "genssa", v.Block.Func.Name)
		}
		if base.Debug.Nil != 0 && v.Pos.Line() > 1 { // v.Pos.Line()==1 in generated wrappers
			base.WarnfAt(v.Pos, "generated nil check")
		}

	case ssa.OpWasm3LoweredWB:
		p := s.Prog(wasm.ACall)
		// AuxInt encodes how many buffer entries we need.
		p.To = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: ir.Syms.GCWriteBarrier[v.AuxInt-1]}
		setReg(s, v.Reg0()) // move result from wasm stack to register local

	case ssa.OpWasm3I64Store8, ssa.OpWasm3I64Store16, ssa.OpWasm3I64Store32, ssa.OpWasm3I64Store, ssa.OpWasm3F32Store, ssa.OpWasm3F64Store:
		getValue32(s, v.Args[0])
		getValue64(s, v.Args[1])
		p := s.Prog(v.Op.Asm())
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: v.AuxInt}

	case ssa.OpWasm3StructSet:
		// struct.set $type AuxInt. The wasm type index for v.Aux and the
		// field-index immediate are resolved by the obj backend; see
		// doc/wasm3-m2-design.md §5. Not yet emitted by any rule.
		getValue64(s, v.Args[0])
		getValue64(s, v.Args[1])
		p := s.Prog(wasm.AStructSet)
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: v.AuxInt}

	case ssa.OpWasm3FieldSet:
		// struct.set of a Go struct field addressed by byte offset. The
		// base (arg0) is a *struct, which the pointer-representation
		// cutover represents as an anyref local; ref.cast it to the
		// concrete (ref $go.struct.T) before struct.set (struct.set
		// rejects the anyref supertype).
		st := v.Aux.(*types.Type)
		structIdx := wasm3RegisterStruct(s.FuncInfo(), st)
		// Restrict to iface components: container/ring's autogen eq stores
		// the iface field's itab/data halves into a temp via per-field
		// stores at off+0/+8 (FieldSet path), which previously panicked.
		// String/slice components also reach this offset but pass a value
		// with no per-value local in some SSA shapes (multi-return tuple
		// extraction in internal/gover.Parse) — Reg() crashes in
		// getValue64. Until that shape is understood, fire only for ifaces.
		if fwidx, boxIdx, comp, ok := wasm3BoxedComponentAtOffset(s, st, v.AuxInt, 0); ok {
			getValue64(s, v.Args[0])
			pCast := s.Prog(wasm.ARefCast)
			pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
			pf := s.Prog(wasm.AStructGet)
			pf.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
			pf.To = obj.Addr{Type: obj.TYPE_CONST, Offset: fwidx}
			pbc := s.Prog(wasm.ARefCast)
			pbc.From = obj.Addr{Type: obj.TYPE_CONST, Offset: boxIdx}
			getValue64(s, v.Args[1])
			pset := s.Prog(wasm.AStructSet)
			pset.From = obj.Addr{Type: obj.TYPE_CONST, Offset: boxIdx}
			pset.To = obj.Addr{Type: obj.TYPE_CONST, Offset: comp}
			break
		}
		if afw, abIdx, ei, ebIdx, efi, ok := wasm3ArrayComponentAtOffset(s, st, v.AuxInt, 0); ok {
			// The offset lands inside an inlined array-of-structs field
			// (e.g. cache.Entries[0].Itab = v): struct.get the boxed array
			// ref, array.get the static element (a boxed ref), then
			// struct.set its field — mutating the shared element object.
			getValue64(s, v.Args[0])
			pCast := s.Prog(wasm.ARefCast)
			pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
			pf := s.Prog(wasm.AStructGet)
			pf.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
			pf.To = obj.Addr{Type: obj.TYPE_CONST, Offset: afw}
			pca := s.Prog(wasm.ARefCast)
			pca.From = obj.Addr{Type: obj.TYPE_CONST, Offset: abIdx}
			i32Const(s, int32(ei))
			if efi >= 0 {
				// Struct element: array.get the element ref, then struct.set
				// its field (mutating the shared element object).
				pg := s.Prog(wasm.AArrayGet)
				pg.From = obj.Addr{Type: obj.TYPE_CONST, Offset: abIdx}
				pce := s.Prog(wasm.ARefCast)
				pce.From = obj.Addr{Type: obj.TYPE_CONST, Offset: ebIdx}
				getValue64(s, v.Args[1])
				pe := s.Prog(wasm.AStructSet)
				pe.From = obj.Addr{Type: obj.TYPE_CONST, Offset: ebIdx}
				pe.To = obj.Addr{Type: obj.TYPE_CONST, Offset: efi}
			} else {
				// Whole boxed element (string/slice/interface): array.set the
				// element ref directly. Stack: arrayref, index, value.
				getValue64(s, v.Args[1])
				pset := s.Prog(wasm.AArraySet)
				pset.From = obj.Addr{Type: obj.TYPE_CONST, Offset: abIdx}
			}
			break
		}
		fieldIdx := wasm3FieldIndexAtOffset(s.FuncInfo(), st, v.AuxInt)
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
		getValue64(s, v.Args[1])
		p := s.Prog(wasm.AStructSet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(fieldIdx)}

	case ssa.OpWasm3BoxStore:
		// Write a *string/*slice/*interface cell: ref.cast the cell ref to
		// (ref $go.box.T) and struct.set field 0 = the boxed value. v.Aux is
		// the boxed pointee Go type, registered as the cell via collectBox.
		bt := v.Aux.(*types.Type)
		cellIdx := wasm3RegisterStruct(s.FuncInfo(), bt)
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(cellIdx)}
		getValue64(s, v.Args[1])
		p := s.Prog(wasm.AStructSet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(cellIdx)}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}

	case ssa.OpWasm3PtrStore:
		// Write through $go.ptr.i64: call_ref the setter with (base,
		// offset, value). arg0=ptr, arg1=value, arg2=mem.
		wasm3EnsureCollector(s.FuncInfo())
		ptrIdx, _, setterIdx, _ := wasm3InteriorPtrClass(v.Args[1].Type)
		tmp := wasm3AllocAnyrefTempLocal(s)
		getValue64(s, v.Args[0])
		localSetIdx(s, tmp)
		wasm3PtrField(s, tmp, 0, ptrIdx) // base (anyref)
		wasm3PtrField(s, tmp, 1, ptrIdx) // offset (i32)
		getValue64(s, v.Args[1])         // value (i64 or anyref)
		wasm3PtrField(s, tmp, 3, ptrIdx) // set funcref
		pc := s.Prog(wasm.ACallRef)
		pc.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(setterIdx)}

	case ssa.OpWasm3GlobalSet:
		// Write a boxed package-level variable's wasm ref-global
		// (pointer-representation cutover). v.Aux is the variable's
		// *obj.LSym; arg0 is the value (a ref). Emit `global.set $var`
		// marked Wasm3GlobalRef so the obj backend attaches R_WASMGLOBAL.
		sym, ok := v.Aux.(*obj.LSym)
		if !ok {
			v.Fatalf("OpWasm3GlobalSet: v.Aux is not *obj.LSym: %T", v.Aux)
		}
		getValue64(s, v.Args[0])
		p := s.Prog(wasm.AGlobalSet)
		p.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: sym}
		p.Mark = wasm.Wasm3GlobalRef

	case ssa.OpWasm3ArrayCopy:
		// M3 Stage E phase 4: copy(dst, src) lowered to `array.copy`
		// on two wasmgc backings. arg0=dst (anyref), arg1=src
		// (anyref), arg2=n (i64 element count), arg3=mem. v.Aux is
		// the slice's *types.Type; the elem-keyed backing index
		// resolves via wasm3RegisterArrayAux. Both refs need
		// ref.cast (ref $T_elem) before array.copy.
		//
		// Wasm stack discipline (5 inputs to array.copy):
		//   dst_ref; dst_off=0; src_ref; src_off=0; count
		// Result is Mem (void) — must be handled in ssaGenValue
		// (not ssaGenValueOnStack) because the dispatcher's default
		// early-returns for Mem-typed values.
		arrIdxC := int64(wasm3RegisterArrayAux(s, v))
		getValue64(s, v.Args[0])
		pCastDst := s.Prog(wasm.ARefCast)
		pCastDst.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdxC}
		i32Const(s, 0)
		getValue64(s, v.Args[1])
		pCastSrc := s.Prog(wasm.ARefCast)
		pCastSrc.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdxC}
		i32Const(s, 0)
		getValue64(s, v.Args[2])
		s.Prog(wasm.AI32WrapI64)
		pCopyC := s.Prog(wasm.AArrayCopy)
		pCopyC.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdxC}
		pCopyC.To = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdxC}

	case ssa.OpWasm3ArraySet:
		// M3 Stage D: array.set $arr_T (ref idx val).
		// arg0 is the array ref (anyref local) — cast to typed
		// ref. arg1 is the i64 index — narrow to i32. arg2 is
		// the i64 element value, narrowed to i32 for packed (i8/
		// i16) or i32 element storage; left as i64 for i64 elem.
		// The type-index operand goes via R_WASMTYPE on the array
		// backing's typeidx.
		idx := int64(wasm3RegisterArrayAux(s, v))
		auxSliceTypeSet := v.Aux.(*types.Type)
		elemSize := auxSliceTypeSet.Elem().Size()
		// Flat-stride slice backings (string / interface / slice
		// elem) — same i64-per-slot adjustment as OpWasm3ArrayGet.
		if wasm3FlatStride(auxSliceTypeSet.Elem()) > 0 {
			elemSize = 8
		}
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: idx}
		getValue64(s, v.Args[1])
		s.Prog(wasm.AI32WrapI64)
		getValue64(s, v.Args[2])
		if elemSize < 8 {
			s.Prog(wasm.AI32WrapI64)
		}
		p := s.Prog(wasm.AArraySet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: idx}

	case ssa.OpWasm3StoreInterior:
		// Write through a fat pointer (doc/wasm3-fat-pointers-design.md).
		// arg0 is the fat-pointer ref; arg1 is the value; arg2 is mem.
		// v.Aux is the container's *types.Type. Dual of LoadInterior;
		// must live in ssaGenValue (not ssaGenValueOnStack) because it
		// is a memory op — the ssaGenValue default returns early for
		// IsMemory() values before the on-stack path runs.
		//   1. Recover the typed container (local.tee the iptr; ref.cast
		//      + struct.get 0 + ref.cast to (ref $container_type));
		//   2. Get the offset (local.get iptr; ref.cast + struct.get 1);
		//   3. Push the value (narrowed if packed);
		//   4. array.set $container_type.
		containerType := v.Aux.(*types.Type)
		wrapIdx := wasm3IptrTypeIdx(containerType.Elem())
		containerIdx := int64(wasm3RegisterArrayBacking(s.FuncInfo(), containerType.Elem()))
		elemSize := containerType.Elem().Size()
		if elemSize > 8 {
			v.Fatalf("OpWasm3StoreInterior: unsupported elem size %d (composite pointees are deferred to a later piece)", elemSize)
		}
		iptrTmp := wasm3AllocAnyrefTempLocal(s)
		getValue64(s, v.Args[0])
		pTee := s.Prog(wasm.ALocalTee)
		pTee.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(iptrTmp)}
		// 1: typed container
		pCast1 := s.Prog(wasm.ARefCast)
		pCast1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wrapIdx)}
		pGet0 := s.Prog(wasm.AStructGet)
		pGet0.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wrapIdx)}
		pGet0.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		pCastC := s.Prog(wasm.ARefCast)
		pCastC.From = obj.Addr{Type: obj.TYPE_CONST, Offset: containerIdx}
		// 2: offset
		pGetTmp := s.Prog(wasm.ALocalGet)
		pGetTmp.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(iptrTmp)}
		pCast2 := s.Prog(wasm.ARefCast)
		pCast2.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wrapIdx)}
		pGet1 := s.Prog(wasm.AStructGet)
		pGet1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wrapIdx)}
		pGet1.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
		// 3: value. Integer values narrow to i32 if packed; float values
		// are already f32/f64 and pass through unchanged; reference
		// (pointer) values are anyref per-value locals and must be
		// downcast to the array's element ref type (ref null $T) before
		// array.set — a NULLABLE cast so a nil pointer doesn't trap (the
		// backing element is nullable; validated in
		// doc/wasm3-ref-element-derisk.wat).
		getValue64(s, v.Args[1])
		if wasm3IsRefSliceElem(containerType.Elem()) {
			elemRef := int64(wasm3RegisterSliceElemRef(s.FuncInfo(), containerType.Elem()))
			pCastV := s.Prog(wasm.ARefCastNull)
			pCastV.From = obj.Addr{Type: obj.TYPE_CONST, Offset: elemRef}
		} else if !containerType.Elem().IsFloat() && elemSize < 8 {
			s.Prog(wasm.AI32WrapI64)
		}
		// 4: array.set
		pSet := s.Prog(wasm.AArraySet)
		pSet.From = obj.Addr{Type: obj.TYPE_CONST, Offset: containerIdx}

	case ssa.OpWasm3MapKeysSet, ssa.OpWasm3MapValuesSet, ssa.OpWasm3MapUsedSet, ssa.OpWasm3MapCapSet:
		// M3 per-type maps: struct.set on a $go.map.<K,V> field. The op
		// is memory-typed; it must live here in ssaGenValue (not
		// ssaGenValueOnStack) because the default branch returns early
		// for memory-typed values before delegating to ssaGenValueOnStack
		// — so without an explicit case here the codegen would never run.
		mapType, ok := v.Aux.(*types.Type)
		if !ok || !mapType.IsMap() {
			v.Fatalf("OpWasm3Map*Set: v.Aux is not a map type: %v", v.Aux)
		}
		mapIdx := int64(wasm3RegisterMapStruct(s.FuncInfo(), mapType))
		var field int64
		switch v.Op {
		case ssa.OpWasm3MapUsedSet:
			field = 0
		case ssa.OpWasm3MapCapSet:
			field = 1
		case ssa.OpWasm3MapKeysSet:
			field = 2
		case ssa.OpWasm3MapValuesSet:
			field = 3
		}
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		getValue64(s, v.Args[1])
		p := s.Prog(wasm.AStructSet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: field}

	case ssa.OpWasm3MapClear:
		// M3 per-type maps: reset a $go.map.<K,V> back to empty —
		// used=0, cap=0, keys=null, values=null. See note on
		// OpWasm3Map*Set above: memory-typed, must live in ssaGenValue.
		mapType, ok := v.Aux.(*types.Type)
		if !ok || !mapType.IsMap() {
			v.Fatalf("OpWasm3MapClear: v.Aux is not a map type: %v", v.Aux)
		}
		mapIdx := int64(wasm3RegisterMapStruct(s.FuncInfo(), mapType))
		// used = 0
		getValue64(s, v.Args[0])
		pCast1 := s.Prog(wasm.ARefCast)
		pCast1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		pZero1 := s.Prog(wasm.AI64Const)
		pZero1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		pUsed := s.Prog(wasm.AStructSet)
		pUsed.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		pUsed.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		// cap = 0
		getValue64(s, v.Args[0])
		pCast2 := s.Prog(wasm.ARefCast)
		pCast2.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		pZero2 := s.Prog(wasm.AI64Const)
		pZero2.From = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		pCap := s.Prog(wasm.AStructSet)
		pCap.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		pCap.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
		// keys = null
		getValue64(s, v.Args[0])
		pCast3 := s.Prog(wasm.ARefCast)
		pCast3.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		s.Prog(wasm.ARefNull)
		pKeys := s.Prog(wasm.AStructSet)
		pKeys.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		pKeys.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 2}
		// values = null
		getValue64(s, v.Args[0])
		pCast4 := s.Prog(wasm.ARefCast)
		pCast4.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		s.Prog(wasm.ARefNull)
		pVals := s.Prog(wasm.AStructSet)
		pVals.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		pVals.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 3}

	case ssa.OpWasm3ArrayCopyInto:
		// In-place value copy into a pre-allocated array ref (b := a where
		// b is a local StackArray, a fixed allocated ref that cannot be
		// reassigned): array.copy $arr $arr dst 0 src 0 len. Memory op.
		ct := v.Aux.(*types.Type)
		arrIdx := int64(wasm3RegisterArrayBacking(s.FuncInfo(), ct.Elem()))
		n := ct.NumElem()
		getValue64(s, v.Args[0]) // dst array ref
		pCastD := s.Prog(wasm.ARefCast)
		pCastD.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}
		i32Const(s, 0)           // dst index
		getValue64(s, v.Args[1]) // src array ref
		pCastS := s.Prog(wasm.ARefCast)
		pCastS.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}
		i32Const(s, 0)        // src index
		i32Const(s, int32(n)) // len
		pCopy := s.Prog(wasm.AArrayCopy)
		pCopy.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}
		pCopy.To = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}

	case ssa.OpArgIntReg, ssa.OpArgFloatReg:
		// M3 Phase 3b: copy the wasm function parameter into v's
		// per-value local. The SSA backend works in i64 GP
		// "registers" so a narrow integer param (i32 in the wasm
		// signature) is widened with i64.extend_i32_u on the way
		// in. Float params and i64 params are copied verbatim.
		v.Block.Func.RegArgs = nil
		ssagen.CheckArgReg(v)
		dst, dstOk := wasm3ValueLocalIdx(s, v)
		if !dstOk {
			break // OpArg not placed; original no-op path.
		}
		idx, narrow := wasm3OpArgParamInfo(v)
		p := s.Prog(wasm.ALocalGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(idx)}
		if narrow {
			s.Prog(wasm.AI64ExtendI32U)
		}
		localSetIdx(s, dst)

	case ssa.OpStoreReg:
		// M2 cutover, Stage C.2: a register spill targets a wasm local,
		// not a linear-memory frame slot — wasm3 has no Go stack frame.
		// The obj backend maps the auto/param slot AddrAuto records to a
		// dedicated spill local; the spill class (i64/f32/f64) is all it
		// needs, since the local holds the whole register regardless of
		// the value's sub-word width. No SP base address is pushed.
		getValue64(s, v.Args[0])
		p := s.Prog(spillStoreOp(v.Type))
		ssagen.AddrAuto(&p.To, v)

	case ssa.OpClobber, ssa.OpClobberReg:
		// TODO: implement for clobberdead experiment. Nop is ok for now.

	default:
		if v.Type.IsMemory() {
			return
		}
		if v.OnWasmStack {
			s.OnWasmStackSkipped++
			// If a Value is marked OnWasmStack, we don't generate the value and store it to a register now.
			// Instead, we delay the generation to when the value is used and then directly generate it on the WebAssembly stack.
			return
		}
		if idx, ok := wasm3ValueLocalIdx(s, v); !ok {
			switch v.Op {
			case ssa.OpWasm3I64Const, ssa.OpWasm3F32Const, ssa.OpWasm3F64Const:
				// Rematerialized const for register-share Phi
				// resolution. emitPhiCopies / readPhiSource will
				// recreate the const inline at each Phi edge that
				// reads it. Skip the original emit + setReg
				// entirely so the regalloc-assigned register-local
				// stays unreferenced (and undeclared by the obj
				// backend's wasm3Locals scan).
				return
			}
			ssaGenValueOnStack(s, v, true)
			if s.OnWasmStackSkipped != 0 {
				panic("wasm: bad stack")
			}
			setReg(s, v.Reg())
		} else {
			ssaGenValueOnStack(s, v, true)
			if s.OnWasmStackSkipped != 0 {
				panic("wasm: bad stack")
			}
			localSetIdx(s, idx)
		}
	}
}

func ssaGenValueOnStack(s *ssagen.State, v *ssa.Value, extend bool) {
	switch v.Op {
	case ssa.OpWasm3LoweredGetClosurePtr:
		getReg(s, wasm.REG_CTXT)

	case ssa.OpWasm3LoweredGetCallerPC:
		p := s.Prog(wasm.AI64Load)
		// Caller PC is stored 8 bytes below first parameter.
		p.From = obj.Addr{
			Type:   obj.TYPE_MEM,
			Name:   obj.NAME_PARAM,
			Offset: -8,
		}

	case ssa.OpWasm3LoweredGetCallerSP:
		p := s.Prog(wasm.AGet)
		// Caller SP is the address of the first parameter.
		p.From = obj.Addr{
			Type:   obj.TYPE_ADDR,
			Name:   obj.NAME_PARAM,
			Reg:    wasm.REG_SP,
			Offset: 0,
		}

	case ssa.OpWasm3LoweredAddr:
		if v.Aux == nil { // address of off(SP), no symbol
			getValue64(s, v.Args[0])
			i64Const(s, v.AuxInt)
			s.Prog(wasm.AI64Add)
			break
		}
		p := s.Prog(wasm.AGet)
		p.From.Type = obj.TYPE_ADDR
		switch v.Aux.(type) {
		case *obj.LSym:
			ssagen.AddAux(&p.From, v)
		case *ir.Name:
			// args[0] is always OpSP for a stack-relative ir.Name
			// address; with regalloc skipped, v.Args[0].Reg() has
			// no assignment, so name the SP register directly.
			p.From.Reg = wasm.REG_SP
			ssagen.AddAux(&p.From, v)
		default:
			panic("wasm: bad LoweredAddr")
		}

	case ssa.OpWasm3LoweredConvert:
		getValue64(s, v.Args[0])

	case ssa.OpWasm3Select:
		getValue64(s, v.Args[0])
		getValue64(s, v.Args[1])
		getValue32(s, v.Args[2])
		s.Prog(v.Op.Asm())

	case ssa.OpWasm3I64AddConst:
		getValue64(s, v.Args[0])
		i64Const(s, v.AuxInt)
		s.Prog(v.Op.Asm())

	case ssa.OpWasm3I64Const:
		i64Const(s, v.AuxInt)

	case ssa.OpWasm3F32Const:
		f32Const(s, v.AuxFloat())

	case ssa.OpWasm3F64Const:
		f64Const(s, v.AuxFloat())

	case ssa.OpWasm3I64Load8U, ssa.OpWasm3I64Load8S, ssa.OpWasm3I64Load16U, ssa.OpWasm3I64Load16S, ssa.OpWasm3I64Load32U, ssa.OpWasm3I64Load32S, ssa.OpWasm3I64Load, ssa.OpWasm3F32Load, ssa.OpWasm3F64Load:
		getValue32(s, v.Args[0])
		p := s.Prog(v.Op.Asm())
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: v.AuxInt}

	case ssa.OpWasm3I64Eqz:
		// Stage E phase 2.D follow-up: when arg0's per-value local
		// is anyref (the slice-ptr / ref-typed case), `i64.eqz` is
		// invalid. Emit `ref.is_null` instead — same boolean
		// semantics (true when the ref is null).
		getValue64(s, v.Args[0])
		if ssa.Wasm3IsAnyrefValue(v.Args[0]) {
			s.Prog(wasm.ARefIsNull)
		} else {
			s.Prog(v.Op.Asm())
		}
		if extend {
			s.Prog(wasm.AI64ExtendI32U)
		}

	case ssa.OpWasm3I64Eq, ssa.OpWasm3I64Ne:
		// Stage E phase 2.D follow-up: ref-typed equality. When both
		// operands' per-value locals are anyref, lower to `ref.eq`
		// — but ref.eq requires its operands to be eqref subtypes,
		// while anyref is the supertype of eqref. Insert
		// `ref.cast (ref null eq)` on each operand to downcast first;
		// the cast is a runtime no-op for array/struct refs (which
		// are all eqref subtypes by wasmgc subtyping). I64Ne flips
		// the result with `i32.eqz`.
		if ssa.Wasm3IsAnyrefValue(v.Args[0]) && ssa.Wasm3IsAnyrefValue(v.Args[1]) {
			getValue64(s, v.Args[0])
			s.Prog(wasm.ARefCastEqref)
			getValue64(s, v.Args[1])
			s.Prog(wasm.ARefCastEqref)
			s.Prog(wasm.ARefEq)
			if v.Op == ssa.OpWasm3I64Ne {
				s.Prog(wasm.AI32Eqz)
			}
		} else {
			getValue64(s, v.Args[0])
			getValue64(s, v.Args[1])
			s.Prog(v.Op.Asm())
		}
		if extend {
			s.Prog(wasm.AI64ExtendI32U)
		}

	case ssa.OpWasm3I64LtS, ssa.OpWasm3I64LtU, ssa.OpWasm3I64GtS, ssa.OpWasm3I64GtU, ssa.OpWasm3I64LeS, ssa.OpWasm3I64LeU, ssa.OpWasm3I64GeS, ssa.OpWasm3I64GeU,
		ssa.OpWasm3F32Eq, ssa.OpWasm3F32Ne, ssa.OpWasm3F32Lt, ssa.OpWasm3F32Gt, ssa.OpWasm3F32Le, ssa.OpWasm3F32Ge,
		ssa.OpWasm3F64Eq, ssa.OpWasm3F64Ne, ssa.OpWasm3F64Lt, ssa.OpWasm3F64Gt, ssa.OpWasm3F64Le, ssa.OpWasm3F64Ge:
		getValue64(s, v.Args[0])
		getValue64(s, v.Args[1])
		s.Prog(v.Op.Asm())
		if extend {
			s.Prog(wasm.AI64ExtendI32U)
		}

	case ssa.OpWasm3I64Add, ssa.OpWasm3I64Sub, ssa.OpWasm3I64Mul, ssa.OpWasm3I64DivU, ssa.OpWasm3I64RemS, ssa.OpWasm3I64RemU, ssa.OpWasm3I64And, ssa.OpWasm3I64Or, ssa.OpWasm3I64Xor, ssa.OpWasm3I64Shl, ssa.OpWasm3I64ShrS, ssa.OpWasm3I64ShrU, ssa.OpWasm3I64Rotl,
		ssa.OpWasm3F32Add, ssa.OpWasm3F32Sub, ssa.OpWasm3F32Mul, ssa.OpWasm3F32Div, ssa.OpWasm3F32Copysign,
		ssa.OpWasm3F64Add, ssa.OpWasm3F64Sub, ssa.OpWasm3F64Mul, ssa.OpWasm3F64Div, ssa.OpWasm3F64Copysign:
		getValue64(s, v.Args[0])
		getValue64(s, v.Args[1])
		s.Prog(v.Op.Asm())

	case ssa.OpWasm3I32Rotl:
		getValue32(s, v.Args[0])
		getValue32(s, v.Args[1])
		s.Prog(wasm.AI32Rotl)
		s.Prog(wasm.AI64ExtendI32U)

	case ssa.OpWasm3I64DivS:
		getValue64(s, v.Args[0])
		getValue64(s, v.Args[1])
		// On wasm3 every int64 division lowers to a direct i64.div_s.
		// The wasm target wraps i64 division in a runtime helper to
		// turn the MinInt64 / -1 wasm trap into a Go runtime panic, but
		// wasm3 is targeting the restricted-subset compatibility level,
		// runs without the Go runtime, and treats engine traps as the
		// terminal failure mode — same end result as a panic, no
		// runtime helper required.
		s.Prog(wasm.AI64DivS)

	case ssa.OpWasm3I64TruncSatF32S, ssa.OpWasm3I64TruncSatF64S:
		getValue64(s, v.Args[0])
		s.Prog(v.Op.Asm())

	case ssa.OpWasm3I64TruncSatF32U, ssa.OpWasm3I64TruncSatF64U:
		getValue64(s, v.Args[0])
		s.Prog(v.Op.Asm())

	case ssa.OpWasm3F32DemoteF64:
		getValue64(s, v.Args[0])
		s.Prog(v.Op.Asm())

	case ssa.OpWasm3F64PromoteF32:
		getValue64(s, v.Args[0])
		s.Prog(v.Op.Asm())

	case ssa.OpWasm3F32ConvertI64S, ssa.OpWasm3F32ConvertI64U,
		ssa.OpWasm3F64ConvertI64S, ssa.OpWasm3F64ConvertI64U,
		ssa.OpWasm3I64Extend8S, ssa.OpWasm3I64Extend16S, ssa.OpWasm3I64Extend32S,
		ssa.OpWasm3F32Neg, ssa.OpWasm3F32Sqrt, ssa.OpWasm3F32Trunc, ssa.OpWasm3F32Ceil, ssa.OpWasm3F32Floor, ssa.OpWasm3F32Nearest, ssa.OpWasm3F32Abs,
		ssa.OpWasm3F64Neg, ssa.OpWasm3F64Sqrt, ssa.OpWasm3F64Trunc, ssa.OpWasm3F64Ceil, ssa.OpWasm3F64Floor, ssa.OpWasm3F64Nearest, ssa.OpWasm3F64Abs,
		ssa.OpWasm3I64Ctz, ssa.OpWasm3I64Clz, ssa.OpWasm3I64Popcnt:
		getValue64(s, v.Args[0])
		s.Prog(v.Op.Asm())

	// WebAssembly 3.0 garbage-collection ops (design-doc §6 object model).
	// The wasm type index for v.Aux and, for the field accessors, the
	// field-index immediate in v.AuxInt are resolved by the obj backend
	// once type-section emission lands. These ops are not yet produced by
	// any rule in Wasm3.rules. See doc/wasm3-m2-design.md §3, §5.
	case ssa.OpWasm3StructNew:
		// A boxed slice ($go.slice.<T>) / string ($go.string) header has a
		// typed backing-ref field 0 ((ref $go.array.T) / (ref $go.bytes)),
		// but the backing value flows as anyref — ref.cast it before
		// struct.new.
		aggT, _ := v.Aux.(*types.Type)
		castIdx := int64(-1)
		switch {
		case aggT != nil && aggT.IsSlice():
			castIdx = int64(wasm3RegisterArrayBacking(s.FuncInfo(), aggT.Elem()))
		case aggT != nil && aggT.IsString():
			wasm3EnsureCollector(s.FuncInfo())
			castIdx = int64(wasmgc.TypeGoBytes)
		}
		// String CONSTANT backing: field 0 is the linear address of a
		// string-literal symbol (OpWasm3LoweredAddr). Per the wasmgc-only
		// rule, a string's backing must be a real (array $go.bytes) ref —
		// ref.cast'ing a linear i64 address to a ref is invalid. Build the
		// (array i8) inline from the literal bytes via array.new_fixed.
		// (Large literals are left to the array.new_data + passive-segment
		// path; see doc/wasm3-design.md globals/constants-in-wasmgc.)
		var inlineBytes []byte
		if aggT != nil && aggT.IsString() && len(v.Args) > 0 {
			a0 := v.Args[0]
			if a0.Op == ssa.OpWasm3LoweredAddr && a0.AuxInt == 0 {
				if sym, ok := a0.Aux.(*obj.LSym); ok && len(sym.P) > 0 {
					inlineBytes = sym.P
				}
			}
		}
		for i, a := range v.Args {
			if i == 0 && inlineBytes != nil {
				// Consume + drop the linear address operand (it only
				// materializes the symbol address constant; it does not
				// read linear memory) so the OnWasmStack accounting stays
				// balanced, then build the (array $go.bytes) from the
				// literal bytes.
				getValue64(s, a)
				s.Prog(wasm.ADrop)
				for _, b := range inlineBytes {
					i32Const(s, int32(b))
				}
				pf := s.Prog(wasm.AArrayNewFixed)
				pf.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasmgc.TypeGoBytes)}
				pf.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(len(inlineBytes))}
				continue
			}
			getValue64(s, a)
			if i == 0 && castIdx >= 0 {
				pc := s.Prog(wasm.ARefCast)
				pc.From = obj.Addr{Type: obj.TYPE_CONST, Offset: castIdx}
			}
		}
		p := s.Prog(wasm.AStructNew)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterStructAux(s, v))}

	case ssa.OpWasm3StructNewDefault:
		p := s.Prog(wasm.AStructNewDefault)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterStructAux(s, v))}

	case ssa.OpWasm3StructGet:
		// Read field v.AuxInt (a Go field index) of a boxed struct value.
		// The struct value is an anyref local; ref.cast to (ref $go.struct.T)
		// before struct.get, and map the Go field index to the WasmGC field
		// index via the field's byte offset (lowerFields may expand fields).
		st := v.Aux.(*types.Type)
		structIdx := wasm3RegisterStruct(s.FuncInfo(), st)
		off := st.Field(int(v.AuxInt)).Offset
		fieldIdx := wasm3FieldIndexAtOffset(s.FuncInfo(), st, off)
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(fieldIdx)}

	case ssa.OpWasm3SliceData, ssa.OpWasm3SliceLength, ssa.OpWasm3SliceCapacity:
		// Boxed slice header field reads (doc/wasm3-slice-boxing.md):
		// struct.get $go.slice.<T> at the field's fixed index. data=0
		// (backing array ref, left as anyref in the per-value local),
		// len=2, cap=3 (i64).
		field := int64(0)
		switch v.Op {
		case ssa.OpWasm3SliceLength:
			field = 2
		case ssa.OpWasm3SliceCapacity:
			field = 3
		}
		getValue64(s, v.Args[0])
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterSliceStruct(s.FuncInfo(), v.Aux.(*types.Type)))}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: field}

	case ssa.OpWasm3StringData, ssa.OpWasm3StringLength:
		// Boxed string component reads: struct.get $go.string at the fixed
		// field index (0=backing $go.bytes ref, 2=length i64). $go.string
		// is the prelude type TypeGoString; wasm3EnsureCollector primes the
		// per-function table so the R_WASMTYPE reloc remaps the index.
		wasm3EnsureCollector(s.FuncInfo())
		field := int64(0)
		if v.Op == ssa.OpWasm3StringLength {
			field = 2
		}
		getValue64(s, v.Args[0])
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasmgc.TypeGoString)}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: field}

	case ssa.OpWasm3IfaceItab, ssa.OpWasm3IfaceData:
		// Boxed interface component reads: struct.get $go.iface at the
		// fixed field index (0=itab ref, 1=data ref). $go.iface is the
		// prelude type TypeGoIface; both fields are anyref.
		wasm3EnsureCollector(s.FuncInfo())
		field := int64(0)
		if v.Op == ssa.OpWasm3IfaceData {
			field = 1
		}
		getValue64(s, v.Args[0])
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasmgc.TypeGoIface)}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: field}

	case ssa.OpWasm3IfaceMake:
		// Build an interface: struct.new $go.iface {itab, data}. Each word
		// must be an anyref. A word that is a type descriptor / static
		// symbol address (OpWasm3LoweredAddr — the itab, and the data for a
		// zero-size concrete type whose new() returns &zerobase) is read
		// from its opaque WasmGC identity ref-global via global.get +
		// R_WASMDESCRIPTOR rather than its i64 linear address (an i64 can't
		// be boxed into an anyref field). Other words are already refs.
		wasm3EnsureCollector(s.FuncInfo())
		emitWord := func(a *ssa.Value) {
			if a.Op == ssa.OpWasm3LoweredAddr {
				if sym, ok := a.Aux.(*obj.LSym); ok {
					// Consume + drop the linear address operand (only
					// materializes the symbol address; no linear read) to
					// keep the OnWasmStack accounting balanced, then read
					// the descriptor's identity ref-global.
					getValue64(s, a)
					s.Prog(wasm.ADrop)
					p := s.Prog(wasm.AGlobalGet)
					p.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: sym}
					p.Mark = wasm.Wasm3DescriptorRef
					return
				}
			}
			getValue64(s, a)
		}
		emitWord(v.Args[0])
		emitWord(v.Args[1])
		p := s.Prog(wasm.AStructNew)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasmgc.TypeGoIface)}

	case ssa.OpWasm3MakeFieldPtr:
		// Materialize $go.ptr.i64 for &container.field. v.Aux is the
		// container struct *types.Type; v.AuxInt is the field byte
		// offset. Emit: <container>; i32.const 0 (offset unused for a
		// struct field — the field index is static in the accessors);
		// ref.func $get_T_field; ref.func $set_T_field;
		// struct.new $go.ptr.i64. The accessors must already be generated
		// (walk-phase pre-gen); WasmGCFieldGetter/Setter are idempotent.
		st := v.Aux.(*types.Type)
		wasm3EnsureCollector(s.FuncInfo())
		leaf := wasm3FieldAtOffset(st, v.AuxInt)
		if leaf == nil {
			v.Fatalf("OpWasm3MakeFieldPtr: no field at byte offset %d in %v", v.AuxInt, st)
		}
		ptrIdx, _, _, _ := wasm3InteriorPtrClass(leaf.Type)
		getValue64(s, v.Args[0])
		i32Const(s, 0)
		pg := s.Prog(wasm.ARefFunc)
		pg.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: reflectdata.WasmGCFieldGetter(st, v.AuxInt).Linksym()}
		ps := s.Prog(wasm.ARefFunc)
		ps.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: reflectdata.WasmGCFieldSetter(st, v.AuxInt).Linksym()}
		pn := s.Prog(wasm.AStructNew)
		pn.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(ptrIdx)}

	case ssa.OpWasm3PtrLoad:
		// Read through $go.ptr.i64: call_ref the getter with (base,
		// offset). The ptr is read three times (base/offset/get fields),
		// so stash it in a scratch anyref local first.
		wasm3EnsureCollector(s.FuncInfo())
		ptrIdx, getterIdx, _, _ := wasm3InteriorPtrClass(v.Type)
		tmp := wasm3AllocAnyrefTempLocal(s)
		getValue64(s, v.Args[0])
		localSetIdx(s, tmp)
		wasm3PtrField(s, tmp, 0, ptrIdx) // base (anyref)
		wasm3PtrField(s, tmp, 1, ptrIdx) // offset (i32)
		wasm3PtrField(s, tmp, 2, ptrIdx) // get funcref
		pc := s.Prog(wasm.ACallRef)
		pc.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(getterIdx)}

	case ssa.OpWasm3GlobalGet:
		// Read a boxed package-level variable's wasm ref-global
		// (pointer-representation cutover). v.Aux is the variable's
		// *obj.LSym. Emit `global.get $var` marked Wasm3GlobalRef so the
		// obj backend attaches R_WASMGLOBAL; the result (a ref) lands in
		// v's anyref per-value local via the value-on-stack store path.
		sym, ok := v.Aux.(*obj.LSym)
		if !ok {
			v.Fatalf("OpWasm3GlobalGet: v.Aux is not *obj.LSym: %T", v.Aux)
		}
		p := s.Prog(wasm.AGlobalGet)
		p.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: sym}
		p.Mark = wasm.Wasm3GlobalRef

	case ssa.OpWasm3FieldGet:
		// struct.get of a Go struct field addressed by byte offset
		// (doc/wasm3-slice-boxing.md field-access ABI). v.Aux is the Go
		// struct *types.Type; v.AuxInt is the field's BYTE OFFSET, which
		// wasm3FieldIndexAtOffset resolves to a WasmGC field index. This is
		// a value-producing op, so it lives in the value-on-stack dispatch
		// (FieldSet, a memory op, stays in the main dispatch).
		st := v.Aux.(*types.Type)
		structIdx := wasm3RegisterStruct(s.FuncInfo(), st)
		getValue64(s, v.Args[0])
		// The base (*struct) is an anyref local under the pointer-
		// representation cutover; ref.cast to (ref $go.struct.T) before
		// struct.get, which rejects the anyref supertype.
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
		if fwidx, boxIdx, comp, ok := wasm3BoxedComponentAtOffset(s, st, v.AuxInt, 0); ok {
			// The offset lands inside a boxed slice/string/interface field
			// (e.g. a slice field's len/cap): struct.get the boxed header
			// ref, then struct.get its component (data/len/cap, ...).
			pf := s.Prog(wasm.AStructGet)
			pf.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
			pf.To = obj.Addr{Type: obj.TYPE_CONST, Offset: fwidx}
			pbc := s.Prog(wasm.ARefCast)
			pbc.From = obj.Addr{Type: obj.TYPE_CONST, Offset: boxIdx}
			pc := s.Prog(wasm.AStructGet)
			pc.From = obj.Addr{Type: obj.TYPE_CONST, Offset: boxIdx}
			pc.To = obj.Addr{Type: obj.TYPE_CONST, Offset: comp}
			break
		}
		if afw, abIdx, ei, ebIdx, efi, ok := wasm3ArrayComponentAtOffset(s, st, v.AuxInt, 0); ok {
			// The offset lands inside an inlined array-of-structs field
			// (e.g. cache.Entries[0].Itab): struct.get the boxed array ref,
			// array.get the static element, then struct.get its field.
			pf := s.Prog(wasm.AStructGet)
			pf.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
			pf.To = obj.Addr{Type: obj.TYPE_CONST, Offset: afw}
			pca := s.Prog(wasm.ARefCast)
			pca.From = obj.Addr{Type: obj.TYPE_CONST, Offset: abIdx}
			i32Const(s, int32(ei))
			pg := s.Prog(wasm.AArrayGet)
			pg.From = obj.Addr{Type: obj.TYPE_CONST, Offset: abIdx}
			if efi >= 0 {
				// Struct element: ref.cast + struct.get the element's field.
				pce := s.Prog(wasm.ARefCast)
				pce.From = obj.Addr{Type: obj.TYPE_CONST, Offset: ebIdx}
				pe := s.Prog(wasm.AStructGet)
				pe.From = obj.Addr{Type: obj.TYPE_CONST, Offset: ebIdx}
				pe.To = obj.Addr{Type: obj.TYPE_CONST, Offset: efi}
			}
			// efi < 0: the element is a whole boxed value (string/slice/
			// interface); the array.get result IS the value.
			break
		}
		if outerIdx, innerStructIdx, innerFieldIdx, ok := wasm3EmbeddedPtrFieldAtOffset(s, st, v.AuxInt); ok {
			// The offset lands inside an embedded *struct field — Go
			// promotes the pointed-to struct's field via the embedded
			// pointer (e.g. funcInfo's *_func embedding flattens
			// `f.nameOff` into a single OffPtr [4] on funcInfo). Emit
			// the chained access: struct.get the embedded pointer, then
			// ref.cast + struct.get the inner field.
			pOuter := s.Prog(wasm.AStructGet)
			pOuter.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
			pOuter.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(outerIdx)}
			pCastInner := s.Prog(wasm.ARefCast)
			pCastInner.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(innerStructIdx)}
			pInner := s.Prog(wasm.AStructGet)
			pInner.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(innerStructIdx)}
			pInner.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(innerFieldIdx)}
			break
		}
		fieldIdx := wasm3FieldIndexAtOffset(s.FuncInfo(), st, v.AuxInt)
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(fieldIdx)}

	case ssa.OpWasm3BoxNewDefault:
		// Allocate a zeroed go.box.T cell for an escaping &localBoxed / a
		// new(string|slice|interface). v.Aux is the boxed pointee Go type,
		// registered as the cell via collectBox (wasm3RegisterStruct).
		bt := v.Aux.(*types.Type)
		cellIdx := wasm3RegisterStruct(s.FuncInfo(), bt)
		p := s.Prog(wasm.AStructNewDefault)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(cellIdx)}

	case ssa.OpWasm3BoxLoad:
		// Read a *string/*slice/*interface cell: ref.cast the cell ref to
		// (ref $go.box.T) and struct.get field 0 (the boxed value). v.Aux is
		// the boxed pointee Go type, registered as the cell via collectBox.
		bt := v.Aux.(*types.Type)
		cellIdx := wasm3RegisterStruct(s.FuncInfo(), bt)
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(cellIdx)}
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(cellIdx)}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}

	case ssa.OpWasm3ArrayNew:
		getValue64(s, v.Args[0])
		getValue64(s, v.Args[1])
		p := s.Prog(wasm.AArrayNew)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterArrayAux(s, v))}

	case ssa.OpWasm3ArrayNewDefault:
		getValue64(s, v.Args[0])
		p := s.Prog(wasm.AArrayNewDefault)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterArrayAux(s, v))}

	case ssa.OpWasm3ArrayElemRef:
		// &arr[i] for a struct element (boxed in the (array (ref box.E))
		// backing): ref.cast the container to the typed backing, narrow idx
		// to i32, array.get the boxed element ref. The result IS the *E
		// interior pointer in the boxed model, so a following field access
		// folds to FieldGet on it.
		backingIdx := int64(wasm3RegisterArrayAux(s, v))
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: backingIdx}
		getValue64(s, v.Args[1])
		s.Prog(wasm.AI32WrapI64)
		p := s.Prog(wasm.AArrayGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: backingIdx}

	case ssa.OpWasm3ArrayGet:
		// M3 Stage D: array.get $arr_T (ref idx). Same shape as
		// ArraySet: ref.cast the anyref to typed ref, narrow idx
		// to i32, then array.get with the type-index immediate.
		//
		// Packed storage (i8, i16) uses array.get_u / array.get_s
		// per v.Type signedness — wasmgc rejects plain array.get
		// on packed types because the unpacked value width is
		// ambiguous. i32/i64 use plain array.get. The result is
		// always extended back to i64 for the per-value local
		// (i64 by wasm3ValueType), with the extension polarity
		// matching the get_u/get_s chosen (and v.Type for i32).
		idx := int64(wasm3RegisterArrayAux(s, v))
		auxSliceType := v.Aux.(*types.Type)
		elemSize := auxSliceType.Elem().Size()
		// Flat-stride slice backings (string / interface / slice
		// elem types) lay out as (array i64) regardless of the Go
		// elem size — each i64 slot is one sub-field of the elem.
		// The wasm op is plain array.get on i64; the i64-extension
		// step below also stays a no-op since the read width is
		// already i64.
		if wasm3FlatStride(auxSliceType.Elem()) > 0 {
			elemSize = 8
		}
		signed := v.Type.IsSigned()
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: idx}
		getValue64(s, v.Args[1])
		s.Prog(wasm.AI32WrapI64)
		var getOp obj.As
		switch elemSize {
		case 1, 2:
			if signed {
				getOp = wasm.AArrayGetS
			} else {
				getOp = wasm.AArrayGetU
			}
		case 4, 8:
			getOp = wasm.AArrayGet
		default:
			v.Fatalf("OpWasm3ArrayGet: unsupported elem size %d", elemSize)
		}
		p := s.Prog(getOp)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: idx}
		if elemSize < 8 {
			if signed {
				s.Prog(wasm.AI64ExtendI32S)
			} else {
				s.Prog(wasm.AI64ExtendI32U)
			}
		}

	case ssa.OpWasm3ArrayLen:
		getValue64(s, v.Args[0])
		s.Prog(wasm.AArrayLen) // no type-index operand

	case ssa.OpWasm3RefNull:
		p := s.Prog(wasm.ARefNull)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterStructAux(s, v))}

	// OpWasm3StackArray emits via ssaGenValueOnStack so the default
	// case's localSetIdx fall-through lands the (ref (array T))
	// result in v's per-value local.

	case ssa.OpWasm3RefIsNull:
		getValue64(s, v.Args[0])
		s.Prog(wasm.ARefIsNull)
		if extend {
			s.Prog(wasm.AI64ExtendI32U)
		}

	case ssa.OpWasm3RefCast:
		getValue64(s, v.Args[0])
		s.Prog(wasm.ARefCast)

	case ssa.OpWasm3RefTest:
		getValue64(s, v.Args[0])
		s.Prog(wasm.ARefTest)
		if extend {
			s.Prog(wasm.AI64ExtendI32U)
		}

	case ssa.OpLoadReg:
		// Mirror of OpStoreReg: reload a spilled register from its
		// dedicated wasm local. See OpStoreReg.
		p := s.Prog(spillLoadOp(v.Type))
		ssagen.AddrAuto(&p.From, v.Args[0])

	case ssa.OpCopy:
		getValue64(s, v.Args[0])

	case ssa.OpWasm3MapKeys, ssa.OpWasm3MapValues, ssa.OpWasm3MapUsed, ssa.OpWasm3MapCap:
		// M3 per-type maps: struct.get on a $go.map.<K,V> field.
		//   MapUsed=0, MapCap=1, MapKeys=2, MapValues=3.
		mapType, ok := v.Aux.(*types.Type)
		if !ok || !mapType.IsMap() {
			v.Fatalf("OpWasm3Map*: v.Aux is not a map type: %v", v.Aux)
		}
		mapIdx := int64(wasm3RegisterMapStruct(s.FuncInfo(), mapType))
		var field int64
		switch v.Op {
		case ssa.OpWasm3MapUsed:
			field = 0
		case ssa.OpWasm3MapCap:
			field = 1
		case ssa.OpWasm3MapKeys:
			field = 2
		case ssa.OpWasm3MapValues:
			field = 3
		}
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: field}

	case ssa.OpWasm3MakeMap:
		// M3 per-type maps: allocate a fresh $go.map.<K,V> WasmGC struct
		// via struct.new_default $go.map.<K,V>. v.Aux is the map's
		// *types.Type; wasm3RegisterMapStruct resolves it to the wasm
		// type index. arg0 is the size hint (currently ignored — the
		// linear-seek implementation grows on demand). The resulting
		// (ref $go.map.<K,V>) is zero-init: cap=0, used=0, keys=null,
		// values=null. The default case's localSetIdx fall-through
		// stores the ref in v's per-value local, typed anyref by
		// wasm3ValueType's OpWasm3MakeMap case.
		mapType, ok := v.Aux.(*types.Type)
		if !ok || !mapType.IsMap() {
			v.Fatalf("OpWasm3MakeMap: v.Aux is not a map type: %v", v.Aux)
		}
		p := s.Prog(wasm.AStructNewDefault)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterMapStruct(s.FuncInfo(), mapType))}

	case ssa.OpWasm3MakeSlice:
		// M3 Stage E phase 2: replacement for the bump-heap
		// runtime.makeslice. v.Aux is the slice's *types.Type
		// (e.g. `[]int32`); the wasmgc backing is keyed on the
		// element type via wasm3RegisterArrayAux. arg0=len,
		// arg1=cap (the backing is sized by cap). Emit:
		//     i32.wrap(cap); array.new_default $arr_T_elem
		// The default case's localSetIdx fall-through stores the
		// resulting ref in v's per-value local, typed anyref by
		// wasm3ValueType's OpWasm3MakeSlice case.
		//
		// All element types now use boxed-ref backing (one slot per
		// Go element): string/slice/interface backings are
		// (array (ref go.string/slice/iface)), struct backings are
		// (array (ref box.E)), scalars use packed/primitive storage.
		// One Go element = one backing slot, so cap is the array length
		// directly — no stride multiply.
		getValue64(s, v.Args[1])
		s.Prog(wasm.AI32WrapI64)
		p := s.Prog(wasm.AArrayNewDefault)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterArrayAux(s, v))}

	case ssa.OpWasm3FuncValue:
		// M3 Stage G: a bare function value is a closure-singleton
		// — the same `(ref $closureCtx)` for every evaluation of
		// the same PFUNC reference. Emit `global.get $singleton`;
		// the linker materialises one wasm global per (sym,
		// closureCtx-type) pair with init expression
		// `(struct.new $closureCtx (ref.func $sym) (i64.const 0))`
		// and resolves the R_WASMCLOSURESINGLETON reloc to that
		// global's index. Saves a struct.new allocation per
		// evaluation versus the inline construction.
		sym, ok := v.Aux.(*obj.LSym)
		if !ok {
			v.Fatalf("OpWasm3FuncValue: v.Aux is not *obj.LSym: %T", v.Aux)
		}
		ft := wasm3FuncTypeOf(v.Type)
		if ft == nil {
			v.Fatalf("OpWasm3FuncValue: v.Type is not a func or *func: %v", v.Type)
		}
		closureIdx := wasm3RegisterClosureCtx(s.FuncInfo(), ft)
		p := s.Prog(wasm.AGlobalGet)
		p.From = obj.Addr{
			Type:   obj.TYPE_MEM,
			Name:   obj.NAME_EXTERN,
			Sym:    sym,
			Offset: int64(closureIdx),
		}

	case ssa.OpWasm3MakeClosureRef:
		// M3 Stage G closures: wrap a linear-memory captures struct
		// in a wasmgc `(ref $go.closure.<sig>)`. Emit
		//   ref.func $sym             ;; field 0: (ref $funcType)
		//   <push captures (i64)>     ;; field 1: captures pointer
		//   struct.new $closureCtx
		// arg0 is the i64 captures pointer (the address of the
		// &struct{F, X0, ...} layout walkClosure constructs).
		sym, ok := v.Aux.(*obj.LSym)
		if !ok {
			v.Fatalf("OpWasm3MakeClosureRef: v.Aux is not *obj.LSym: %T", v.Aux)
		}
		ft := wasm3FuncTypeOf(v.Type)
		if ft == nil {
			v.Fatalf("OpWasm3MakeClosureRef: v.Type is not a func or *func: %v", v.Type)
		}
		closureIdx := wasm3RegisterClosureCtx(s.FuncInfo(), ft)
		pf := s.Prog(wasm.ARefFunc)
		pf.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: sym}
		getValue64(s, v.Args[0])
		pn := s.Prog(wasm.AStructNew)
		pn.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(closureIdx)}

	case ssa.OpWasm3MakeClosureRefInline:
		// doc/wasm3-m3-captures-in-struct.md: build a closureCtx
		// subtype `(sub $go.closure.<sig> (struct (ref $func) i64
		// cap0 cap1 ...))` with captures stored inline rather than
		// behind a linear-memory pointer. Emit:
		//   ref.func $sym                ;; field 0: funcref
		//   i64.const 0                  ;; field 1: legacy
		//                                    captures-ptr slot
		//                                    (kept so the per-
		//                                    signature base type
		//                                    stays compatible);
		//                                    new-style bodies
		//                                    ignore it.
		//   <push capture0>              ;; field 2: cap0
		//   <push capture1>              ;; field 3: cap1
		//   ...
		//   struct.new $closureCtx_<sym>
		sym, ok := v.Aux.(*obj.LSym)
		if !ok {
			v.Fatalf("OpWasm3MakeClosureRefInline: v.Aux is not *obj.LSym: %T", v.Aux)
		}
		ft := wasm3FuncTypeOf(v.Type)
		if ft == nil {
			v.Fatalf("OpWasm3MakeClosureRefInline: v.Type is not a func or *func: %v", v.Type)
		}
		// Use the Go-level capture types from the side channel so the
		// caller and body agree on the per-closure-ctx struct shape
		// for composite captures (slice/string/array). For scalar
		// captures the side-channel types and v.Args types coincide,
		// so the caller-side count and body-side count match
		// trivially. Walk publishes the side channel before either
		// side runs codegen; see walkClosure's wasm3-composite branch
		// and the body-prologue at ssagen.
		captureTypes := wasm3CaptureTypesFromSide(sym)
		if captureTypes == nil {
			// Legacy fall-through: scalar captures whose walk path
			// hasn't published yet end up here; reconstruct from
			// v.Args (each arg is one capture).
			captureTypes = make([]*types.Type, len(v.Args))
			for i, a := range v.Args {
				captureTypes[i] = a.Type
			}
		}
		perClosureIdx := wasm3RegisterPerClosureCtx(s.FuncInfo(), sym, ft, captureTypes)
		pf := s.Prog(wasm.ARefFunc)
		pf.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: sym}
		// Legacy captures-ptr slot: 0 (new-style bodies ignore it).
		p0 := s.Prog(wasm.AI64Const)
		p0.From = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		// Push captures in order.
		for _, a := range v.Args {
			getValue64(s, a)
		}
		pn := s.Prog(wasm.AStructNew)
		pn.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(perClosureIdx)}

	case ssa.OpWasm3LoweredGetClosureRef:
		// doc/wasm3-m3-captures-in-struct.md: read the CTXT_REF
		// anyref module global (index 2). The default case's
		// localSetIdx fall-through stores the resulting anyref into
		// v's per-value local (typed anyref by wasm3ValueType).
		p := s.Prog(wasm.AGlobalGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm.Wasm3GlobalIndexCtxRef)}

	case ssa.OpWasm3LoweredCastClosureRef:
		// doc/wasm3-m3-captures-in-struct.md: cast a plain anyref
		// (from LoweredGetClosureRef) down to a concrete (ref
		// $go.closure.<sym>) subtype, so subsequent GetClosureField
		// ops can struct.get its capture fields. v.Aux is the
		// closure body's *obj.LSym; the per-closure type index is
		// resolved through wasm3EnsurePerClosureCtxFromSide, which
		// lazy-registers the type in *this* function's typeCollector
		// from the captures-list ssagen stashed in
		// wasm.Wasm3ClosureBodyCaptures.
		sym, ok := v.Aux.(*obj.LSym)
		if !ok {
			v.Fatalf("OpWasm3LoweredCastClosureRef: v.Aux is not *obj.LSym: %T", v.Aux)
		}
		perClosureIdx := wasm3EnsurePerClosureCtxFromSide(s.FuncInfo(), sym, v.Type)
		getValue64(s, v.Args[0])
		pc := s.Prog(wasm.ARefCast)
		pc.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(perClosureIdx)}

	case ssa.OpWasm3GetClosureField:
		// doc/wasm3-m3-captures-in-struct.md: struct.get a capture
		// from a typed closure-ref produced by
		// LoweredCastClosureRef. v.Aux is the closure body's
		// *obj.LSym; v.AuxInt is the *capture index* (0-based);
		// fields 0 (funcref) and 1 (legacy captures-ptr) come
		// before, so the emitted field index is AuxInt + 2.
		//
		// We re-emit ref.cast before each struct.get: the per-value
		// local for the LoweredCastClosureRef result is declared
		// anyref (wasm3place.go can't reach the per-closure type
		// index that would let it declare a typed local), so
		// local.get yields anyref. struct.get rejects anyref input;
		// the cast is mandatory. Cost: one ref.cast per capture
		// access, a single type-tag compare on V8.
		sym, ok := v.Aux.(*obj.LSym)
		if !ok {
			v.Fatalf("OpWasm3GetClosureField: v.Aux is not *obj.LSym: %T", v.Aux)
		}
		perClosureIdx := wasm3EnsurePerClosureCtxFromSide(s.FuncInfo(), sym, nil)
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(perClosureIdx)}
		pg := s.Prog(wasm.AStructGet)
		pg.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(perClosureIdx)}
		// AuxInt is the wasm-field offset within the captures
		// region; the high bit (Wasm3GetClosureFieldAnyrefBit) is
		// the anyref-typing marker — mask it off for the field
		// index, then add 2 for the base fields (funcref + legacy
		// captures-ptr).
		fieldOff := ssa.Wasm3GetClosureFieldOffset(v.AuxInt)
		pg.To = obj.Addr{Type: obj.TYPE_CONST, Offset: fieldOff + 2}

	case ssa.OpWasm3Clone:
		// Deep-copy a boxed composite for Go value semantics. v.Aux is the
		// Go composite type; arg0 is the source ref. Array case (scalar/
		// ref elements): newArr = array.new_default $arr N; array.copy the
		// N elements; result is newArr. Validated in
		// doc/wasm3-array-valuecopy-derisk.wat.
		ct := v.Aux.(*types.Type)
		if !ct.IsArray() {
			v.Fatalf("OpWasm3Clone: only array types supported so far, got %v", ct)
		}
		elem := ct.Elem()
		if elem.IsStruct() || elem.IsArray() {
			v.Fatalf("OpWasm3Clone: composite element %v deferred (needs recursive clone)", elem)
		}
		arrIdx := int64(wasm3RegisterArrayBacking(s.FuncInfo(), elem))
		n := ct.NumElem()
		tmp := wasm3AllocAnyrefTempLocal(s)
		// newArr = array.new_default $arr N
		i32Const(s, int32(n))
		pNew := s.Prog(wasm.AArrayNewDefault)
		pNew.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}
		// tee into tmp, leaving newArr on the stack as array.copy's dst
		pTee := s.Prog(wasm.ALocalTee)
		pTee.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(tmp)}
		// array.copy dst=newArr(on stack) dstidx=0 src=arg0 srcidx=0 len=N
		i32Const(s, 0)
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}
		i32Const(s, 0)
		i32Const(s, int32(n))
		pCopy := s.Prog(wasm.AArrayCopy)
		pCopy.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}
		pCopy.To = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}
		// result = newArr (from tmp)
		pGet := s.Prog(wasm.ALocalGet)
		pGet.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(tmp)}

	case ssa.OpWasm3InteriorPtr:
		// doc/wasm3-fat-pointers-design.md Piece 2: materialise a fat
		// pointer for an interior slot of a wasmgc container.
		// v.Aux is the container's *types.Type (a slice or array);
		// arg0 is the container ref (anyref-typed at the SSA level);
		// arg1 is the i32 element offset. Wrapper-type index is
		// derived from the element's storage class.
		//
		// Emits: <container>; <offset i32>; struct.new $go.iptr.<class>
		containerType := v.Aux.(*types.Type)
		wrapIdx := wasm3IptrTypeIdx(containerType.Elem())
		// Even though wrapIdx is a fixed prelude index, the linker's
		// R_WASMTYPE remap reads from the function's WasmType.Table —
		// which must be initialised with the prelude entries before
		// any prelude index is referenced.
		wasm3EnsureCollector(s.FuncInfo())
		getValue64(s, v.Args[0])
		getValue64(s, v.Args[1])
		s.Prog(wasm.AI32WrapI64)
		pNew := s.Prog(wasm.AStructNew)
		pNew.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wrapIdx)}

	case ssa.OpWasm3LoadInterior:
		// Read through a fat pointer. arg0 is the fat-pointer ref;
		// v.Aux is the container's *types.Type (slice or array).
		// Emits the canonical sequence of:
		//   1. ref.cast iptr to (ref $go.iptr.<class>); struct.get 0
		//      -> container as anyref;
		//   2. ref.cast to (ref $container_wasmgc_type);
		//   3. ref.cast iptr again; struct.get 1 -> offset i32;
		//   4. array.get_u / array.get on $container_wasmgc_type.
		// The result is widened back to i64 for the per-value local
		// (i64 by wasm3ValueType), matching OpWasm3ArrayGet's
		// extension polarity.
		containerType := v.Aux.(*types.Type)
		wrapIdx := wasm3IptrTypeIdx(containerType.Elem())
		containerIdx := int64(wasm3RegisterArrayBacking(s.FuncInfo(), containerType.Elem()))
		elemSize := containerType.Elem().Size()
		signed := v.Type.IsSigned()
		// Stash the iptr in an anyref temp local — we need to read
		// both of its fields (container at 0, offset at 1) but
		// getValue64 is one-shot for OnWasmStack values (a second
		// call would re-execute the producer's codegen). Local.tee
		// stores the iptr while keeping it on the stack for the
		// first use; the second use comes from local.get.
		iptrTmp := wasm3AllocAnyrefTempLocal(s)
		getValue64(s, v.Args[0])
		pTee := s.Prog(wasm.ALocalTee)
		pTee.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(iptrTmp)}
		// 1: container ref — cast iptr from anyref to iptr-typed, then
		// struct.get field 0.
		pCast1 := s.Prog(wasm.ARefCast)
		pCast1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wrapIdx)}
		pGet0 := s.Prog(wasm.AStructGet)
		pGet0.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wrapIdx)}
		pGet0.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		// 2: typed container ref
		pCastC := s.Prog(wasm.ARefCast)
		pCastC.From = obj.Addr{Type: obj.TYPE_CONST, Offset: containerIdx}
		// 3: offset — re-fetch iptr (anyref) and re-cast.
		pGetTmp := s.Prog(wasm.ALocalGet)
		pGetTmp.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(iptrTmp)}
		pCast2 := s.Prog(wasm.ARefCast)
		pCast2.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wrapIdx)}
		pGet1 := s.Prog(wasm.AStructGet)
		pGet1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wrapIdx)}
		pGet1.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
		// 4: array.get with the right packed/unpacked variant. Float
		// elements ((array f32) / (array f64)) use plain array.get and
		// stay f32/f64 — no packed get_s/get_u and no i64-extension, since
		// the LoadInterior value's per-value local is f32/f64.
		isFloat := v.Type.IsFloat()
		var getOp obj.As
		switch {
		case isFloat:
			getOp = wasm.AArrayGet
		case elemSize == 1 || elemSize == 2:
			if signed {
				getOp = wasm.AArrayGetS
			} else {
				getOp = wasm.AArrayGetU
			}
		case elemSize == 4 || elemSize == 8:
			getOp = wasm.AArrayGet
		default:
			v.Fatalf("OpWasm3LoadInterior: unsupported elem size %d (composite pointees are deferred to a later piece)", elemSize)
		}
		pGetArr := s.Prog(getOp)
		pGetArr.From = obj.Addr{Type: obj.TYPE_CONST, Offset: containerIdx}
		if !isFloat && elemSize < 8 {
			if signed {
				s.Prog(wasm.AI64ExtendI32S)
			} else {
				s.Prog(wasm.AI64ExtendI32U)
			}
		}

	case ssa.OpWasm3SubSlice:
		// M3 Stage E phase 3: sub-slicing via deep copy. arg0 =
		// orig_backing (anyref), arg1 = lo (i64), arg2 = len (i64),
		// arg3 = cap (i64), arg4 = mem. v.Aux is the slice type;
		// the elem-keyed backing index resolves via
		// wasm3RegisterArrayAux. The emitted sequence allocates a
		// fresh backing of `cap` elements and array.copies `len`
		// elements out of orig_backing starting at lo:
		//
		//     i32.wrap(cap); array.new_default $T_elem ;; new backing
		//     local.tee $tmp                                 ;; stash + leave on stack
		//     i32.const 0                                    ;; dst offset
		//     ref.cast (ref $T_elem) orig_backing            ;; typed src
		//     i32.wrap(lo)                                   ;; src offset
		//     i32.wrap(len)                                  ;; element count
		//     array.copy $T_elem $T_elem                     ;; dst already on stack
		//     local.get $tmp                                 ;; result: new backing
		//
		// $tmp is a scratch anyref local allocated via
		// wasm3AllocTempLocal. The default case's localSetIdx
		// fall-through then stores the result in v's per-value
		// local.
		arrIdx := int64(wasm3RegisterArrayAux(s, v))
		tmpLocal := wasm3AllocAnyrefTempLocal(s)

		// Allocate new backing of `cap` elements.
		getValue64(s, v.Args[3])
		s.Prog(wasm.AI32WrapI64)
		pNew := s.Prog(wasm.AArrayNewDefault)
		pNew.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}

		// Stash the new backing in $tmp and leave a copy on the stack
		// (which will become array.copy's dst arg). The encoder for
		// ALocalSet/Tee reads the local index from p.To, not p.From.
		// $tmp is anyref-typed, so the value left on the stack after
		// tee is anyref. array.copy's dst slot needs the typed
		// (ref $T) — re-cast.
		pTee := s.Prog(wasm.ALocalTee)
		pTee.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(tmpLocal)}
		pCastDst := s.Prog(wasm.ARefCast)
		pCastDst.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}

		// dst offset = 0
		i32Const(s, 0)

		// Push the typed src: ref.cast orig_backing to (ref $T_elem).
		getValue64(s, v.Args[0])
		pCastSrc := s.Prog(wasm.ARefCast)
		pCastSrc.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}

		// src offset = lo, count = len.
		getValue64(s, v.Args[1])
		s.Prog(wasm.AI32WrapI64)
		getValue64(s, v.Args[2])
		s.Prog(wasm.AI32WrapI64)

		// array.copy $T_elem $T_elem. The encoder takes two
		// type-index operands: dst-array type (From) and src-array
		// type (To). Both are the same here — the new backing has
		// the same elem-type as orig_backing.
		pCopy := s.Prog(wasm.AArrayCopy)
		pCopy.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}
		pCopy.To = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}

		// Push the new backing as the op result.
		pGet := s.Prog(wasm.ALocalGet)
		pGet.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(tmpLocal)}

	case ssa.OpWasm3StackArray:
		// M3 Stage D: stack-allocated `var buf [N]T` auto. The
		// SSA-side replacement for the OpLocalAddr ssagen would
		// emit on PAUTO with TARRAY type. v.Aux is the *ir.Name;
		// the Name's type is *[N]T. Emit:
		//     i32.const <N>
		//     array.new_default $arr_T_elem
		// The default case's localSetIdx fall-through then stores
		// the resulting ref in v's per-value local (typed
		// `(ref null any)`, per wasm3ValueType's OpWasm3StackArray
		// case).
		name, ok := v.Aux.(*ir.Name)
		if !ok {
			v.Fatalf("OpWasm3StackArray: v.Aux is not *ir.Name: %T", v.Aux)
		}
		arrType := name.Type() // [N]T
		if !arrType.IsArray() {
			v.Fatalf("OpWasm3StackArray: Name type is not array: %v", arrType)
		}
		i32Const(s, int32(arrType.NumElem()))
		p := s.Prog(wasm.AArrayNewDefault)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterArrayBacking(s.FuncInfo(), arrType.Elem()))}

	case ssa.OpWasm3StackStruct:
		// Stack-allocated `var s T` (struct). Build the zero value:
		// struct.new_default (scalars 0, ref fields null — a nil slice/
		// string/map/ptr field is correctly null), then fill each array
		// field with a fresh array.new_default so element access doesn't
		// hit a null ref. v.Aux is the *ir.Name (its type is *T).
		name, ok := v.Aux.(*ir.Name)
		if !ok {
			v.Fatalf("OpWasm3StackStruct: v.Aux is not *ir.Name: %T", v.Aux)
		}
		st := name.Type()
		if !st.IsStruct() {
			v.Fatalf("OpWasm3StackStruct: Name type is not struct: %v", st)
		}
		structIdx := int64(wasm3RegisterStruct(s.FuncInfo(), st))
		pNew := s.Prog(wasm.AStructNewDefault)
		pNew.From = obj.Addr{Type: obj.TYPE_CONST, Offset: structIdx}
		tmp := wasm3AllocAnyrefTempLocal(s)
		pSet := s.Prog(wasm.ALocalSet)
		pSet.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(tmp)}
		for _, f := range st.Fields() {
			if !f.Type.IsArray() {
				continue // scalars 0 / refs null from struct.new_default
			}
			fieldIdx := wasm3FieldIndexAtOffset(s.FuncInfo(), st, f.Offset)
			pg := s.Prog(wasm.ALocalGet)
			pg.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(tmp)}
			i32Const(s, int32(f.Type.NumElem()))
			pArr := s.Prog(wasm.AArrayNewDefault)
			pArr.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterArrayBacking(s.FuncInfo(), f.Type.Elem()))}
			pStructSet := s.Prog(wasm.AStructSet)
			pStructSet.From = obj.Addr{Type: obj.TYPE_CONST, Offset: structIdx}
			pStructSet.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(fieldIdx)}
		}
		pGetRes := s.Prog(wasm.ALocalGet)
		pGetRes.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(tmp)}

	default:
		v.Fatalf("unexpected op: %s", v.Op)

	}
}

func isCmp(v *ssa.Value) bool {
	switch v.Op {
	case ssa.OpWasm3I64Eqz, ssa.OpWasm3I64Eq, ssa.OpWasm3I64Ne, ssa.OpWasm3I64LtS, ssa.OpWasm3I64LtU, ssa.OpWasm3I64GtS, ssa.OpWasm3I64GtU, ssa.OpWasm3I64LeS, ssa.OpWasm3I64LeU, ssa.OpWasm3I64GeS, ssa.OpWasm3I64GeU,
		ssa.OpWasm3F32Eq, ssa.OpWasm3F32Ne, ssa.OpWasm3F32Lt, ssa.OpWasm3F32Gt, ssa.OpWasm3F32Le, ssa.OpWasm3F32Ge,
		ssa.OpWasm3F64Eq, ssa.OpWasm3F64Ne, ssa.OpWasm3F64Lt, ssa.OpWasm3F64Gt, ssa.OpWasm3F64Le, ssa.OpWasm3F64Ge,
		ssa.OpWasm3RefIsNull, ssa.OpWasm3RefTest:
		return true
	default:
		return false
	}
}

// wasm3NarrowABI reports whether t is a sub-word integer or boolean —
// a type the width-faithful wasm3 ABI carries in an i32 slot rather
// than the i64 register width. See attachWasmType.
func wasm3NarrowABI(t *types.Type) bool {
	return (t.IsInteger() || t.IsBoolean()) && t.Size() <= 4
}

// isNarrowWasmField reports whether a wasm field is carried in an i32
// slot rather than the i64 register width. Used by the call site to
// drive per-field narrowing when the callee carries a typeCollector-
// emitted WasmType signature, where one Go param can lower to several
// wasm fields with their own widths.
func isNarrowWasmField(f obj.WasmField) bool {
	switch f.Type {
	case obj.WasmI32, obj.WasmPtr, obj.WasmBool:
		return true
	}
	return false
}

func getValue32(s *ssagen.State, v *ssa.Value) {
	if v.OnWasmStack {
		s.OnWasmStackSkipped--
		ssaGenValueOnStack(s, v, false)
		if !isCmp(v) {
			s.Prog(wasm.AI32WrapI64)
		}
		return
	}

	// M3 Phase 3b: per-value locals always hold i64 for integer-
	// class values (see wasm3ValueType), so the i32.wrap is
	// unconditional here.
	if idx, ok := wasm3ValueLocalIdx(s, v); ok {
		localGetIdx(s, idx)
		s.Prog(wasm.AI32WrapI64)
		return
	}

	reg := v.Reg()
	getReg(s, reg)
	if reg != wasm.REG_SP {
		s.Prog(wasm.AI32WrapI64)
	}
}

func getValue64(s *ssagen.State, v *ssa.Value) {
	if v.OnWasmStack {
		s.OnWasmStackSkipped--
		ssaGenValueOnStack(s, v, true)
		return
	}

	if idx, ok := wasm3ValueLocalIdx(s, v); ok {
		localGetIdx(s, idx)
		return
	}

	// Defensive wasm3 fallback: if a value has no per-value local
	// (wasm3PlaceValues skipped it, e.g. OpSelectN from a multi-return
	// Call that survived to genssa) AND no register (wasm3 doesn't run
	// regalloc, so f.RegAlloc is nil), v.Reg() panics. Re-emit the
	// producer inline via ssaGenValueOnStack — pure ops are
	// idempotent; the risk of double-emit only manifests if a side-
	// effecting op reaches here, which wasm3PlaceValues + the new
	// wasm3PatchValues post-regalloc pass should have placed. Single-
	// consumer values are unaffected (they hit the OnWasmStack branch
	// above).
	if v.Block.Func.RegAlloc == nil {
		// Mem tokens are dependence markers, not values — they have no
		// codegen and no wasm stack representation. Silently swallow:
		// the consumer abstractly needs the mem for ordering but
		// doesn't emit any code for it. Without this, ssaGenValueOnStack's
		// recursion into a re-emitted op's mem-typed arg trips through
		// the SelectN<mem> case, which has no placement (expand_calls
		// keeps it in place for memForCall tracking but wasm3HasOutput
		// rejects mem) — yielding a false "unexpected op: SelectN"
		// when the mem token itself isn't actually being consumed.
		if v.Type != nil && v.Type.IsMemory() {
			return
		}
		ssaGenValueOnStack(s, v, true)
		return
	}

	reg := v.Reg()
	getReg(s, reg)
	if reg == wasm.REG_SP {
		s.Prog(wasm.AI64ExtendI32U)
	}
}

func i32Const(s *ssagen.State, val int32) {
	p := s.Prog(wasm.AI32Const)
	p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(val)}
}

func i64Const(s *ssagen.State, val int64) {
	p := s.Prog(wasm.AI64Const)
	p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: val}
}

func f32Const(s *ssagen.State, val float64) {
	p := s.Prog(wasm.AF32Const)
	p.From = obj.Addr{Type: obj.TYPE_FCONST, Val: val}
}

func f64Const(s *ssagen.State, val float64) {
	p := s.Prog(wasm.AF64Const)
	p.From = obj.Addr{Type: obj.TYPE_FCONST, Val: val}
}

func getReg(s *ssagen.State, reg int16) {
	p := s.Prog(wasm.AGet)
	p.From = obj.Addr{Type: obj.TYPE_REG, Reg: reg}
}

func setReg(s *ssagen.State, reg int16) {
	p := s.Prog(wasm.ASet)
	p.To = obj.Addr{Type: obj.TYPE_REG, Reg: reg}
}

// emitPhiCopies emits, for the outgoing edge from b to succ, one
// `local.get <src>; local.set <Lphi>` copy per OpPhi in succ —
// the per-edge Phi resolution that replaces regalloc's
// destination-register-sharing scheme under the M3 regalloc-
// bypass design (doc/wasm3-m3-no-regalloc.md, Phase 3b).
//
// The source value is read via its per-value local if
// wasm3PlaceValues placed it; otherwise it falls back to the
// regalloc-assigned register-local (the same fallback ssaGenValue's
// default case uses for unplaced values like OpArg*). The
// destination is always the Phi's per-value local — Phi is
// placed unconditionally now.
//
// Copies are emitted in dependency order: a copy whose source is
// another Phi's destination is held until the reader runs.
// Cycles between Phis (the classic "swap" problem in a loop where
// `a, b = b, f(a, b)` shares a Phi destination across reads and
// writes) are broken by stashing the cycle-edge source into a
// fresh temp local before the writers run, then emitting the
// readers against the temp.
//
// Triggered for example by runtime/algos `gcd(a, b)`'s
// `a, b = b, a%b`: without dependency ordering Phi_a would read
// Phi_b's local *after* Phi_b's copy already overwrote it with
// the new value (a%b), producing gcd(48, 18) = 0.
func emitPhiCopies(s *ssagen.State, b, succ *ssa.Block) {
	if len(succ.Preds) == 0 {
		return
	}
	predIdx := -1
	for i, e := range succ.Preds {
		if e.Block() == b {
			predIdx = i
			break
		}
	}
	if predIdx < 0 {
		return
	}

	// Collect the per-Phi copy descriptors.
	type phiCopy struct {
		dst uint32
		src *ssa.Value
	}
	var copies []phiCopy
	dsts := make(map[uint32]bool)
	for _, phi := range succ.Values {
		if phi.Op != ssa.OpPhi {
			continue
		}
		dst, ok := wasm3ValueLocalIdx(s, phi)
		if !ok {
			continue
		}
		copies = append(copies, phiCopy{dst: dst, src: phi.Args[predIdx]})
		dsts[dst] = true
	}
	if len(copies) == 0 {
		return
	}

	// Topological emit: pick a copy whose dst is not read by any
	// other still-pending copy. If none exist (cycle), break by
	// copying one cycle source to a temp local and rewriting
	// future reads of that source to read the temp.
	emitOne := func(c phiCopy) {
		readPhiSource(s, c.src)
		localSetIdx(s, c.dst)
	}
	srcReadsDst := func(c phiCopy, dst uint32) bool {
		// Source reads dst when src is a Phi placed at dst (the
		// "cycle source" case in parallel copies). Trace through
		// OpCopy chains the same way readPhiSource does.
		v := c.src
		for v.Op == ssa.OpCopy {
			if _, ok := wasm3ValueLocalIdx(s, v); ok {
				break
			}
			v = v.Args[0]
		}
		if v.Op != ssa.OpPhi {
			return false
		}
		idx, ok := wasm3ValueLocalIdx(s, v)
		return ok && idx == dst
	}
	for len(copies) > 0 {
		// Find a safe copy: dst is not read by any other pending copy.
		picked := -1
		for i := range copies {
			safe := true
			for j := range copies {
				if i == j {
					continue
				}
				if srcReadsDst(copies[j], copies[i].dst) {
					safe = false
					break
				}
			}
			if safe {
				picked = i
				break
			}
		}
		if picked >= 0 {
			emitOne(copies[picked])
			copies = append(copies[:picked], copies[picked+1:]...)
			continue
		}
		// All remaining copies form a cycle. Break by stashing
		// copies[0].dst's current value into a fresh wasm local
		// allocated after the placed ones, then rewrite any copy
		// whose source reads copies[0].dst to read the temp.
		tempIdx := wasm3AllocTempLocal(s, copies[0].src.Type)
		// Push the current value of copies[0].dst onto the stack
		// (it's the Phi value, not the per-value local of the
		// source — they share the same local). local.tee writes
		// to temp and leaves on stack, then local.set into the
		// final dst.
		brokenDst := copies[0].dst
		p := s.Prog(wasm.ALocalGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(brokenDst)}
		p2 := s.Prog(wasm.ALocalSet)
		p2.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(tempIdx)}
		// Replace future reads of brokenDst with reads from tempIdx
		// by rewriting cycle-source Phi values: substitute src with
		// a synthetic marker pointing at tempIdx. The simplest
		// implementation: emit those copies inline now, using
		// tempIdx as the source.
		for i := 0; i < len(copies); i++ {
			if srcReadsDst(copies[i], brokenDst) {
				pg := s.Prog(wasm.ALocalGet)
				pg.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(tempIdx)}
				localSetIdx(s, copies[i].dst)
				copies = append(copies[:i], copies[i+1:]...)
				i--
			}
		}
	}
}

// wasm3FuncTypeOf returns the Go func type carried by a wasm3 SSA
// value that represents a function value. Go's type system canonically
// wraps function values as *func(...) at the SSA level (see ssagen.go
// PFUNC handling, which produces types.NewPtr(n.Type())), but after
// SSA decomposition some sites lose the pointer wrap and carry the
// bare func(...) type. Accept both shapes; return nil if t is
// neither a func type nor a pointer-to-func.
func wasm3FuncTypeOf(t *types.Type) *types.Type {
	if t == nil {
		return nil
	}
	if t.IsPtr() {
		if elem := t.Elem(); elem != nil && elem.Kind() == types.TFUNC {
			return elem
		}
		return nil
	}
	if t.Kind() == types.TFUNC {
		return t
	}
	return nil
}

// wasm3SyntheticFuncType reconstructs a *types.Type matching the
// call's wasm-level signature from its abi.ABIParamResultInfo.
// Used by OpWasm3LoweredInterCall codegen to register a funcType
// for the call_indirect typeidx immediate. The receiver (if the
// callee is a method) is already the first entry in InParams() at
// this stage, so the synthesized func type has no receiver and
// each param flows through tryPrimitiveAttach / loweredStorages
// like any other function signature would.
func wasm3SyntheticFuncType(call *ssa.AuxCall) *types.Type {
	info := call.ABIInfo()
	var params []*types.Field
	for _, p := range info.InParams() {
		params = append(params, types.NewField(src.NoXPos, nil, p.Type))
	}
	var results []*types.Field
	for _, p := range info.OutParams() {
		results = append(results, types.NewField(src.NoXPos, nil, p.Type))
	}
	return types.NewSignature(nil, params, results)
}

// wasm3RegisterStructAux extracts the *types.Type from v.Aux and
// registers it with the function's per-function wasmgc.Table,
// returning its module-internal type index. Used by the codegen
// path for OpWasm3StructNew / OpWasm3StructNewDefault /
// OpWasm3StructGet / OpWasm3StructSet: the encoder needs a per-
// package type index so the linker can emit the R_WASMTYPE reloc.
func wasm3RegisterStructAux(s *ssagen.State, v *ssa.Value) uint32 {
	t, ok := v.Aux.(*types.Type)
	if !ok {
		v.Fatalf("wasm3RegisterStructAux: v.Aux is not *types.Type: %T", v.Aux)
	}
	if t.IsSlice() {
		// A boxed slice header ($go.slice.<T>) is reached via the same
		// StructNew/StructGet/StructSet ops as a Go struct; resolve its
		// type index through the slice-struct collector.
		return wasm3RegisterSliceStruct(s.FuncInfo(), t)
	}
	if t.IsString() {
		// A boxed string is the prelude $go.string type; prime the
		// per-function table so the index remaps.
		wasm3EnsureCollector(s.FuncInfo())
		return uint32(wasmgc.TypeGoString)
	}
	return wasm3RegisterStruct(s.FuncInfo(), t)
}

// wasm3FieldIndexAtOffset returns the WasmGC field index for the Go
// struct field at byte offset off in struct type st, accounting for
// lowerFields' per-Go-field expansion (a Go field may lower to several
// WasmGC fields — e.g. an inlined nested struct). The function's
// typeCollector must already exist (the caller registers st via
// wasm3RegisterStruct first). See doc/wasm3-slice-boxing.md.
func wasm3FieldIndexAtOffset(fi *obj.FuncInfo, st *types.Type, off int64) int {
	cAny, ok := wasm3LiveCollector.Load(fi)
	if !ok {
		base.Fatalf("wasm3FieldIndexAtOffset: no typeCollector for the function")
	}
	c := cAny.(*typeCollector)
	idx, ok := wasm3FieldIndexRec(c, st, off, 0)
	if !ok {
		base.Fatalf("wasm3FieldIndexAtOffset: no field at byte offset %d in %v", off, st)
	}
	return idx
}

// wasm3EmbeddedPtrFieldAtOffset detects the Go embedded-pointer-flatten
// pattern: when funcInfo embeds *_func and a caller writes f.nameOff,
// the compiler may collapse the promoted selector into a single
// OffPtr [4] on funcInfo (treating *_func as if it were value-embedded
// _func). The wasm3 layout has funcInfo with the *_func as its own
// ref field (not value-embedded), so the offset doesn't land on any
// funcInfo field. This helper walks the outer struct's pointer fields
// and, if the offset falls inside one whose pointee struct has a
// field at the corresponding interior offset, returns the chain
// (outerFieldIdx, innerStructTypeIdx, innerFieldIdx) the codegen
// needs to emit a struct.get-then-deref-then-struct.get pair.
func wasm3EmbeddedPtrFieldAtOffset(s *ssagen.State, st *types.Type, off int64) (outerIdx, innerStructIdx, innerFieldIdx int, ok bool) {
	fi := s.FuncInfo()
	cAny, cOk := wasm3LiveCollector.Load(fi)
	if !cOk {
		return 0, 0, 0, false
	}
	c := cAny.(*typeCollector)
	widx := 0
	for _, f := range st.Fields() {
		n := len(c.lowerFields(f.Type))
		if off >= f.Offset && off < f.Offset+f.Type.Size() && f.Type.IsPtr() && f.Type.Elem() != nil && f.Type.Elem().IsStruct() {
			inner := f.Type.Elem()
			innerOff := off - f.Offset
			if iIdx, iok := wasm3FieldIndexRec(c, inner, innerOff, 0); iok {
				return widx, c.collectStruct(inner), iIdx, true
			}
		}
		widx += n
	}
	return 0, 0, 0, false
}

// wasm3FieldIndexRec walks a Go struct's fields accumulating the WasmGC
// field index (base), returning the index of the field at byte offset
// off. An inlined nested struct field is recursed into (it lowers to
// several consecutive WasmGC fields). Returns ok=false if off does not
// land on an addressable field start — e.g. an offset into an array
// field, which is an element access (ArrayGet), not a struct field.
func wasm3FieldIndexRec(c *typeCollector, t *types.Type, off int64, base int) (int, bool) {
	widx := base
	for _, f := range t.Fields() {
		n := len(c.lowerFields(f.Type))
		if off == f.Offset {
			return widx, true
		}
		if off > f.Offset && off < f.Offset+f.Type.Size() && f.Type.IsStruct() {
			return wasm3FieldIndexRec(c, f.Type, off-f.Offset, widx)
		}
		widx += n
	}
	return 0, false
}

// wasm3BoxedComponentAtOffset handles a Load/Store at a byte offset that
// falls INSIDE a boxed slice/string/interface field of struct st (e.g. a
// slice field's len at +8 or cap at +16), which names no top-level wasm
// field. It returns the field's WasmGC index (fieldWasmIdx, the boxed
// header ref), the header's wasm type index (boxTypeIdx), and the
// component field index within that header (compField) for the sub-word
// being accessed. Codegen then emits a nested struct.get/set: the field
// ref, then its data/len/cap (slice 0/2/3), data/len (string 0/2), or
// itab/data (interface 0/1) component. ok=false if off names a top-level
// field or no boxed sub-word. Recurses into inlined nested structs.
func wasm3BoxedComponentAtOffset(s *ssagen.State, t *types.Type, off, base int64) (fieldWasmIdx, boxTypeIdx, compField int64, ok bool) {
	cAny, has := wasm3LiveCollector.Load(s.FuncInfo())
	if !has {
		return 0, 0, 0, false
	}
	c := cAny.(*typeCollector)
	widx := base
	const ptr = 8 // PtrSize
	for _, f := range t.Fields() {
		n := int64(len(c.lowerFields(f.Type)))
		if off == f.Offset {
			return 0, 0, 0, false // top-level field start — not our case
		}
		if off > f.Offset && off < f.Offset+f.Type.Size() {
			sub := off - f.Offset
			switch {
			case f.Type.IsSlice():
				comp := int64(-1)
				switch sub {
				case 0:
					comp = 0 // data
				case ptr:
					comp = 2 // len
				case 2 * ptr:
					comp = 3 // cap
				}
				if comp < 0 {
					return 0, 0, 0, false
				}
				return widx, int64(wasm3RegisterSliceStruct(s.FuncInfo(), f.Type)), comp, true
			case f.Type.IsString():
				comp := int64(-1)
				switch sub {
				case 0:
					comp = 0 // data
				case ptr:
					comp = 2 // len
				}
				if comp < 0 {
					return 0, 0, 0, false
				}
				return widx, int64(wasmgc.TypeGoString), comp, true
			case f.Type.IsInterface():
				comp := int64(-1)
				switch sub {
				case 0:
					comp = 0 // itab
				case ptr:
					comp = 1 // data
				}
				if comp < 0 {
					return 0, 0, 0, false
				}
				return widx, int64(wasmgc.TypeGoIface), comp, true
			case f.Type.IsStruct():
				return wasm3BoxedComponentAtOffset(s, f.Type, off-f.Offset, widx)
			}
			return 0, 0, 0, false
		}
		widx += n
	}
	return 0, 0, 0, false
}

// wasm3ArrayComponentAtOffset handles a Load/Store at a byte offset that
// falls INSIDE an inlined array field of struct t — e.g. the autogenerated
// hash/eq of internal/abi.TypeAssertCache{Mask; Entries [1]Entry} reads
// Entries[0].Itab at byte offset 16, which names no top-level wasm field. An
// array lowers to a single boxed (ref (array elem)) field, so reaching the
// component needs struct.get the array ref, array.get the (statically indexed)
// element, then struct.get the element's field. Returns the array field's
// WasmGC index, the array backing type index, the static element index, the
// element struct's box wasm type index, and the element field's WasmGC index.
// ok=false unless off lands inside an array field whose elements are structs
// and the within-element offset names a direct element field. Recurses into
// inlined nested structs to locate the array field. (Static element index
// only — the [N]T algs the compiler emits for small arrays unroll element
// accesses to constant offsets.)
func wasm3ArrayComponentAtOffset(s *ssagen.State, t *types.Type, off, base int64) (arrayFieldWasmIdx, arrayBackingIdx, elemIdx, elemBoxIdx, elemFieldIdx int64, ok bool) {
	cAny, has := wasm3LiveCollector.Load(s.FuncInfo())
	if !has {
		return
	}
	c := cAny.(*typeCollector)
	widx := base
	for _, f := range t.Fields() {
		n := int64(len(c.lowerFields(f.Type)))
		if off > f.Offset && off < f.Offset+f.Type.Size() {
			sub := off - f.Offset
			switch {
			case f.Type.IsArray() && f.Type.Elem().IsStruct():
				elemT := f.Type.Elem()
				esz := elemT.Size()
				if esz == 0 {
					return
				}
				efi, fok := wasm3FieldIndexRec(c, elemT, sub%esz, 0)
				if !fok {
					return
				}
				return widx, int64(wasm3RegisterArrayBacking(s.FuncInfo(), elemT)), sub / esz,
					int64(wasm3RegisterStruct(s.FuncInfo(), elemT)), int64(efi), true
			case f.Type.IsArray() && (f.Type.Elem().IsString() || f.Type.Elem().IsSlice() || f.Type.Elem().IsInterface()):
				// A string/slice/interface array element is a single boxed
				// ref (collectBacking gives (array (ref $go.string|...)). Only
				// whole-element access is handled (sub-component access into a
				// boxed element would need a further struct.get); the boxed
				// element ref IS the value, so elemFieldIdx = -1 signals "no
				// trailing struct.get — the array.get result is the result".
				elemT := f.Type.Elem()
				esz := elemT.Size()
				if esz == 0 || sub%esz != 0 {
					return
				}
				return widx, int64(wasm3RegisterArrayBacking(s.FuncInfo(), elemT)), sub / esz, 0, -1, true
			case f.Type.IsStruct():
				return wasm3ArrayComponentAtOffset(s, f.Type, off-f.Offset, widx)
			}
			return
		}
		widx += n
	}
	return
}

// wasm3FieldAtOffset returns the leaf (possibly nested) struct field at
// byte offset off in struct st — recursing into inlined nested struct
// fields so the innermost scalar/ref is returned, not the enclosing
// struct. Used by the interior-pointer codegen to pick the pointee class.
// Returns nil if off does not name a field.
func wasm3FieldAtOffset(st *types.Type, off int64) *types.Field {
	for _, f := range st.Fields() {
		if off >= f.Offset && off < f.Offset+f.Type.Size() {
			if f.Type.IsStruct() {
				if sub := wasm3FieldAtOffset(f.Type, off-f.Offset); sub != nil {
					return sub
				}
			}
			return f
		}
	}
	return nil
}

// wasm3InteriorPtrClass returns the $go.ptr.<class> wrapper, getter, and
// setter prelude type indices for an interior pointer whose pointee is t,
// plus whether it is the ref/anyref class. Integer/bool pointees use the
// i64 class (the converting accessors widen sub-word ints); unsafe.Pointer
// and other reference pointees use the ref class (anyref-valued
// accessors). See doc/wasm3-pointer-cutover.
func wasm3InteriorPtrClass(t *types.Type) (ptrIdx, getterIdx, setterIdx uint32, isRef bool) {
	if t != nil && (t.IsUnsafePtr() || t.IsPtr() || t.IsInterface() || t.IsSlice() || t.IsString()) {
		return wasmgc.TypeGoPtrRef, wasmgc.TypeGoGetterRef, wasmgc.TypeGoSetterRef, true
	}
	if t != nil && t.IsFloat() {
		if t.Kind() == types.TFLOAT32 {
			return wasmgc.TypeGoPtrF32, wasmgc.TypeGoGetterF32, wasmgc.TypeGoSetterF32, false
		}
		return wasmgc.TypeGoPtrF64, wasmgc.TypeGoGetterF64, wasmgc.TypeGoSetterF64, false
	}
	return wasmgc.TypeGoPtrI64, wasmgc.TypeGoGetterI64, wasmgc.TypeGoSetterI64, false
}

// wasm3PtrField emits the read of field `field` of the $go.ptr.<class>
// fat pointer (type index ptrIdx) stashed in anyref local `tmp`:
// local.get tmp; ref.cast (ref $go.ptr.<class>); struct.get
// $go.ptr.<class> field. Used by the interior-pointer deref codegen
// (PtrLoad/PtrStore). Both the i64 and ref classes share the {base,
// offset, get, set} layout, so only the type index varies.
func wasm3PtrField(s *ssagen.State, tmp uint32, field int64, ptrIdx uint32) {
	pg := s.Prog(wasm.ALocalGet)
	pg.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(tmp)}
	pc := s.Prog(wasm.ARefCast)
	pc.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(ptrIdx)}
	pf := s.Prog(wasm.AStructGet)
	pf.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(ptrIdx)}
	pf.To = obj.Addr{Type: obj.TYPE_CONST, Offset: field}
}

// wasm3RegisterArrayAux is the array.* counterpart of
// wasm3RegisterStructAux. v.Aux is the *types.Type of the whole
// array (`[N]T`), slice (`[]T`), or — for the OpArgIntReg-base
// rules added for slice-in-struct args — the slice's .array
// pointer type (`*T`). All three forms key the wasm array
// backing on the element type T.
func wasm3RegisterArrayAux(s *ssagen.State, v *ssa.Value) uint32 {
	t, ok := v.Aux.(*types.Type)
	if !ok {
		v.Fatalf("wasm3RegisterArrayAux: v.Aux is not *types.Type: %T", v.Aux)
	}
	switch {
	case t.IsArray(), t.IsSlice():
		return wasm3RegisterArrayBacking(s.FuncInfo(), t.Elem())
	case t.IsPtr():
		return wasm3RegisterArrayBacking(s.FuncInfo(), t.Elem())
	}
	v.Fatalf("wasm3RegisterArrayAux: v.Aux is not array/slice/pointer type: %v", t)
	return 0
}

// wasm3AllocAnyrefTempLocal appends a fresh anyref scratch local to
// the function's local-types vector and returns its absolute wasm
// local index. Used by OpWasm3SubSlice (and any other op that
// needs to stash a ref between intermediate wasm stack states).
func wasm3AllocAnyrefTempLocal(s *ssagen.State) uint32 {
	fi := s.FuncInfo()
	if fi == nil {
		panic("wasm3AllocAnyrefTempLocal: no FuncInfo")
	}
	idx := uint32(len(fi.Wasm3LocalTypes))
	fi.Wasm3LocalTypes = append(fi.Wasm3LocalTypes, 0x6E) // anyref
	base := uint32(0)
	if wt := fi.WasmType; wt != nil {
		base = uint32(len(wt.Params))
	}
	return base + idx
}

// wasm3AllocTempLocal appends a per-value local of the type
// appropriate for t to the function's local-types vector and
// returns its absolute wasm local index. Used by emitPhiCopies
// to break Phi-cycle ordering hazards.
func wasm3AllocTempLocal(s *ssagen.State, t *types.Type) uint32 {
	fi := s.FuncInfo()
	if fi == nil {
		panic("wasm3AllocTempLocal: no FuncInfo")
	}
	var typ byte
	if t.IsFloat() {
		switch t.Size() {
		case 4:
			typ = 0x7D // f32
		case 8:
			typ = 0x7C // f64
		default:
			typ = 0x7C
		}
	} else {
		typ = 0x7E // i64
	}
	idx := uint32(len(fi.Wasm3LocalTypes))
	fi.Wasm3LocalTypes = append(fi.Wasm3LocalTypes, typ)
	// Translate placement-index to absolute wasm-local index:
	// per-value locals start right after the wasm parameter locals.
	base := uint32(0)
	if wt := fi.WasmType; wt != nil {
		base = uint32(len(wt.Params))
	}
	return base + idx
}

// readPhiSource pushes the SSA value v onto the wasm stack, picking
// the right access path depending on where regalloc/place-values
// landed v:
//   - per-value local placement (the common case post-Phase 3b),
//   - a register-local (for OpArg* and any unplaced value),
//   - a spill local (a value regalloc spilled to a LocalSlot).
//
// Phi sources can land in any of the three, so this helper covers
// all three. Used by emitPhiCopies.
//
// Special case: OpCopy chains. Regalloc inserts OpCopy values as
// part of its Phi-resolution-via-register-sharing scheme; they
// have no per-value local (added after wasm3PlaceValues runs) and
// otherwise fall through to the register-local path. Tracing
// through them eliminates one copy per Phi resolution edge.
func readPhiSource(s *ssagen.State, v *ssa.Value) {
	for v.Op == ssa.OpCopy {
		if _, ok := wasm3ValueLocalIdx(s, v); ok {
			break // placed; reading its per-value local is fine
		}
		v = v.Args[0]
	}
	if v.OnWasmStack {
		s.OnWasmStackSkipped--
		ssaGenValueOnStack(s, v, true)
		return
	}
	if idx, ok := wasm3ValueLocalIdx(s, v); ok {
		localGetIdx(s, idx)
		return
	}
	// Rematerialized constants: regalloc duplicates rematerializable
	// const values (one per use site under register-sharing Phi
	// resolution). The duplicates land at value IDs past the place-
	// values pass and have no per-value local. Recreate the const
	// inline at the read site rather than going through getReg's
	// register-local — the inline emit is smaller (1-2 bytes for
	// the const opcode vs 2 bytes for `local.get N` + a wasted
	// register-local declaration).
	switch v.Op {
	case ssa.OpWasm3I64Const:
		i64Const(s, v.AuxInt)
		return
	case ssa.OpWasm3F32Const:
		f32Const(s, v.AuxFloat())
		return
	case ssa.OpWasm3F64Const:
		f64Const(s, v.AuxFloat())
		return
	}
	if _, isReg := v.Block.Func.RegAlloc[v.ID].(*ssa.Register); isReg {
		reg := v.Reg()
		getReg(s, reg)
		if reg == wasm.REG_SP {
			s.Prog(wasm.AI64ExtendI32U)
		}
		return
	}
	// LocalSlot: regalloc spilled v to a wasm spill local. The
	// obj backend translates an I64Load/F32Load/F64Load with an
	// AddrAuto operand into a `local.get <spill_local>` directly
	// (see encodeWasm3Body and wasm3Locals); no real linear-
	// memory load is emitted.
	p := s.Prog(spillLoadOp(v.Type))
	ssagen.AddrAuto(&p.From, v)
}

// localGetIdx emits a wasm `local.get N` with the given absolute
// wasm-local index N. M3 Phase 3 codegen helper: bypasses the
// register-name → local-index translation in the obj backend.
func localGetIdx(s *ssagen.State, idx uint32) {
	p := s.Prog(wasm.ALocalGet)
	p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(idx)}
}

// localSetIdx is the mirror of localGetIdx for `local.set`.
func localSetIdx(s *ssagen.State, idx uint32) {
	p := s.Prog(wasm.ALocalSet)
	p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(idx)}
}

// wasm3ValueLocalIdx returns the absolute wasm-local index that holds
// the SSA value v under the M3 regalloc-bypass scheme. Returns
// (idx, true) if v has a placement; (0, false) for values the
// wasm3PlaceValues pass skipped (Phi, void-result ops, etc.) or
// for non-wasm3 builds. The base offset is the wasm function's
// parameter count — per-value locals start right after the
// signature's parameter locals.
func wasm3ValueLocalIdx(s *ssagen.State, v *ssa.Value) (uint32, bool) {
	locals := v.Block.Func.Wasm3ValueLocals
	if locals == nil || int(v.ID) >= len(locals) {
		return 0, false
	}
	const noLocal = ^uint32(0)
	if locals[v.ID] == noLocal {
		return 0, false
	}
	// Base offset: the wasm function's parameter count. Each
	// parameter lives in locals 0..nparams-1; per-value locals
	// start at nparams.
	base := wasm3ParamLocalCount(v.Block.Func)
	return base + locals[v.ID], true
}

// wasm3OpArgParamInfo returns the wasm parameter local index for an
// OpArgIntReg or OpArgFloatReg value, plus whether that parameter's
// wasm type is narrow (i32-class in the wasm signature: WasmI32,
// WasmPtr, or WasmBool) and therefore needs i64.extend_i32_u on the
// way into the SSA i64 register width.
//
// v.AuxInt is the per-class index — for OpArgIntReg it counts only
// int params before v in declaration order; OpArgFloatReg counts
// only float params. The wasm signature interleaves the two classes
// by declaration order, so the absolute wasm param index is found
// by walking WasmType.Params and counting until the per-class N-th
// param of the right class is reached.
func wasm3OpArgParamInfo(v *ssa.Value) (paramIdx uint32, narrow bool) {
	isFloat := v.Op == ssa.OpArgFloatReg
	wantIdx := v.AuxInt
	ifn := v.Block.Func.Frontend().Func()
	if ifn == nil || ifn.LSym == nil {
		return 0, false
	}
	wt := ifn.LSym.Func().WasmType
	if wt == nil {
		return 0, false
	}
	var intCount, floatCount int64
	for i, f := range wt.Params {
		switch f.Type {
		case obj.WasmF32, obj.WasmF64:
			if isFloat && floatCount == wantIdx {
				return uint32(i), false
			}
			floatCount++
		default: // WasmI32, WasmI64, WasmPtr, WasmBool, WasmRef
			if !isFloat && intCount == wantIdx {
				narrow = f.Type == obj.WasmI32 || f.Type == obj.WasmPtr || f.Type == obj.WasmBool
				return uint32(i), narrow
			}
			intCount++
		}
	}
	return 0, false
}

// wasm3ParamLocalCount returns the number of wasm locals that the
// function's parameters occupy (== the number of fields in the
// declared WasmType signature's Params). Cached at the *Func level
// via the LSym's WasmType aux.
func wasm3ParamLocalCount(f *ssa.Func) uint32 {
	ifn := f.Frontend().Func()
	if ifn == nil || ifn.LSym == nil {
		return 0
	}
	wt := ifn.LSym.Func().WasmType
	if wt == nil {
		return 0
	}
	return uint32(len(wt.Params))
}

func loadOp(t *types.Type) obj.As {
	if t.IsFloat() {
		switch t.Size() {
		case 4:
			return wasm.AF32Load
		case 8:
			return wasm.AF64Load
		default:
			panic("bad load type")
		}
	}

	switch t.Size() {
	case 1:
		if t.IsSigned() {
			return wasm.AI64Load8S
		}
		return wasm.AI64Load8U
	case 2:
		if t.IsSigned() {
			return wasm.AI64Load16S
		}
		return wasm.AI64Load16U
	case 4:
		if t.IsSigned() {
			return wasm.AI64Load32S
		}
		return wasm.AI64Load32U
	case 8:
		return wasm.AI64Load
	default:
		panic("bad load type")
	}
}

func storeOp(t *types.Type) obj.As {
	if t.IsFloat() {
		switch t.Size() {
		case 4:
			return wasm.AF32Store
		case 8:
			return wasm.AF64Store
		default:
			panic("bad store type")
		}
	}

	switch t.Size() {
	case 1:
		return wasm.AI64Store8
	case 2:
		return wasm.AI64Store16
	case 4:
		return wasm.AI64Store32
	case 8:
		return wasm.AI64Store
	default:
		panic("bad store type")
	}
}

// spillStoreOp and spillLoadOp give the wasm store/load opcode for a
// register spill of a value of type t. Unlike storeOp/loadOp — which
// pick a sub-word opcode for a linear-memory access — a spill targets a
// wasm local that holds the whole register, so the opcode is keyed only
// by register class: i64 for every integer or pointer, f32/f64 for
// floats. The obj backend uses the class to type the spill local.
func spillStoreOp(t *types.Type) obj.As {
	if t.IsFloat() {
		if t.Size() == 4 {
			return wasm.AF32Store
		}
		return wasm.AF64Store
	}
	return wasm.AI64Store
}

func spillLoadOp(t *types.Type) obj.As {
	if t.IsFloat() {
		if t.Size() == 4 {
			return wasm.AF32Load
		}
		return wasm.AF64Load
	}
	return wasm.AI64Load
}
