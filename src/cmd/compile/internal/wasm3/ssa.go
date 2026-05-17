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
		if v.Op == ssa.OpWasm3LoweredClosureCall {
			getValue64(s, v.Args[1])
			setReg(s, wasm.REG_CTXT)
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
		} else {
			getValue64(s, v.Args[0])
			p := s.Prog(obj.ACALL)
			p.To = obj.Addr{Type: obj.TYPE_NONE}
			p.Pos = v.Pos
			if v.Op == ssa.OpWasm3LoweredTailCallInter {
				p.As = obj.ARET
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
		getValue64(s, v.Args[0])
		s.Prog(wasm.AI64Eqz)
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

	case ssa.OpWasm3ArraySet:
		// array.set $type. Not yet emitted by any rule.
		getValue64(s, v.Args[0])
		getValue64(s, v.Args[1])
		getValue64(s, v.Args[2])
		s.Prog(wasm.AArraySet)

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
			p.From.Reg = v.Args[0].Reg()
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
		getValue64(s, v.Args[0])
		s.Prog(v.Op.Asm())
		if extend {
			s.Prog(wasm.AI64ExtendI32U)
		}

	case ssa.OpWasm3I64Eq, ssa.OpWasm3I64Ne, ssa.OpWasm3I64LtS, ssa.OpWasm3I64LtU, ssa.OpWasm3I64GtS, ssa.OpWasm3I64GtU, ssa.OpWasm3I64LeS, ssa.OpWasm3I64LeU, ssa.OpWasm3I64GeS, ssa.OpWasm3I64GeU,
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
		s.Prog(wasm.AStructNew)

	case ssa.OpWasm3StructNewDefault:
		s.Prog(wasm.AStructNewDefault)

	case ssa.OpWasm3StructGet:
		getValue64(s, v.Args[0])
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: v.AuxInt}

	case ssa.OpWasm3ArrayNew:
		getValue64(s, v.Args[0])
		getValue64(s, v.Args[1])
		s.Prog(wasm.AArrayNew)

	case ssa.OpWasm3ArrayNewDefault:
		getValue64(s, v.Args[0])
		s.Prog(wasm.AArrayNewDefault)

	case ssa.OpWasm3ArrayGet:
		getValue64(s, v.Args[0])
		getValue64(s, v.Args[1])
		s.Prog(wasm.AArrayGet)

	case ssa.OpWasm3ArrayLen:
		getValue64(s, v.Args[0])
		s.Prog(wasm.AArrayLen)

	case ssa.OpWasm3RefNull:
		s.Prog(wasm.ARefNull)

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
// Copies are emitted in OpPhi order. Cycles between Phis
// (the "swap" problem) would require a temp local; none of the
// M2 regression test programs hit one, and the standard exit-
// SSA algorithm for handling them is documented in the design
// doc as a follow-up.
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
	for _, phi := range succ.Values {
		if phi.Op != ssa.OpPhi {
			continue
		}
		dst, ok := wasm3ValueLocalIdx(s, phi)
		if !ok {
			continue // Phi-of-memory, etc.
		}
		src := phi.Args[predIdx]
		readPhiSource(s, src)
		localSetIdx(s, dst)
	}
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
