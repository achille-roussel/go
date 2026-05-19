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
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/ssagen"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/obj/wasm"
	"cmd/internal/src"
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
		panic("wasm: bad stack")
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
		elemSize := v.Aux.(*types.Type).Elem().Size()
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
		for _, a := range v.Args {
			getValue64(s, a)
		}
		p := s.Prog(wasm.AStructNew)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterStructAux(s, v))}

	case ssa.OpWasm3StructNewDefault:
		p := s.Prog(wasm.AStructNewDefault)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterStructAux(s, v))}

	case ssa.OpWasm3StructGet:
		getValue64(s, v.Args[0])
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterStructAux(s, v))}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: v.AuxInt}

	case ssa.OpWasm3ArrayNew:
		getValue64(s, v.Args[0])
		getValue64(s, v.Args[1])
		p := s.Prog(wasm.AArrayNew)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterArrayAux(s, v))}

	case ssa.OpWasm3ArrayNewDefault:
		getValue64(s, v.Args[0])
		p := s.Prog(wasm.AArrayNewDefault)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterArrayAux(s, v))}

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
		elemSize := v.Aux.(*types.Type).Elem().Size()
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
		captureTypes := make([]*types.Type, len(v.Args))
		for i, a := range v.Args {
			captureTypes[i] = a.Type
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
		pg.To = obj.Addr{Type: obj.TYPE_CONST, Offset: v.AuxInt + 2}

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
	return wasm3RegisterStruct(s.FuncInfo(), t)
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
