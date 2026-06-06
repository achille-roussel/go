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
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/obj/wasm"
	"cmd/internal/src"
	"cmd/internal/wasmgc"
	"go/constant"
	"strconv"
	"strings"
	"sync"
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

	// Lazy polymorphic-fat-pointer accessor materialisation. The wasm3
	// SSA backend generates accessor LSyms on demand at caller-side
	// genssa (when the accessor's own FuncInfo doesn't yet exist) and
	// records the (elemT, isSet) meta in a side-table. The obj-side
	// assemble3 calls this hook at the start of every wasm3 function
	// body assembly; the hook checks the side-table and, on match,
	// registers the accessor's (ref (array T)) backing in s.Func()'s
	// wasmgc.Table and installs the Wasm3PolyAccessor on the FuncInfo.
	// assemble3 then sees the marker and dispatches to
	// assembleWasm3PolyAccessor, replacing the trivial SSA body.
	obj.Wasm3SetupPolyAccessor = wasm3SetupPolyAccessor

	// Phase D step 5: route OCALLINTER lowering through a per-(I,
	// methodIdx) trampoline LSym instead of the legacy
	// itab.Fun-load + indirect-call sequence. The trampoline body
	// is hand-assembled by assembleWasm3IfaceDispatch.
	ssagen.Arch.Wasm3IfaceDispatchTrampoline = wasm3GetOrGenIfaceDispatchTrampoline

	// Phase D step 3: register $go.vtab.<I> in a per-itab wasmgc
	// collector and stash its type index + serialized table on the
	// itab LSym, so the linker can emit a typed (ref $go.vtab.<I>)
	// global initialised with the per-(I,T) method funcrefs. Hook
	// lives in reflectdata.writeITab.
	reflectdata.Wasm3PrepareItabVtabHook = Wasm3PrepareItabVtab
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
			// Inspect the WASM-level signature to see which Go results
			// lowered to a single (ref X) at the wasm boundary. For
			// those, the regabi-decomposed args coming through
			// MakeResult are autotmp/zerobase pointers that have lost
			// the actual field values upstream (see doc/wasm3 composite
			// SSA gap). Drain the args and emit struct.new_default so
			// the function validates against the (ref X) signature —
			// field values default to zero. Genuine struct-value flow
			// (where field values survive in SSA, e.g. through
			// OpStructMake at the trampoline level) still needs the
			// full composite-rewrite pass to produce the right values.
			var wasmResults []obj.WasmField
			if fi := s.FuncInfo(); fi != nil && fi.WasmType != nil {
				wasmResults = fi.WasmType.Results
			}
			resultIdx := 0
			for _, p := range b.Func.OwnAux.ABIInfo().OutParams() {
				regTypes, _ := p.RegisterTypesAndOffsets()
				// Does this Go result map to a single wasm ref?
				if p.Type != nil && p.Type.IsStruct() && p.Type.Size() > 0 &&
					resultIdx < len(wasmResults) && wasmResults[resultIdx].Type == obj.WasmRef {
					structIdx := wasm3RegisterStruct(s.FuncInfo(), p.Type)
					nRegs := len(p.Registers)
					if nRegs == 0 {
						nRegs = 1
					}
					// If the SSA arg is a single ref-typed value (i.e.
					// the body already constructed the struct as a
					// WasmGC ref — e.g. trampoline body that returns
					// OpWasm3ChanBufGet's typed result), pass it through
					// with a ref.cast to the exact wasm result type. The
					// regabi-collapsed pointer case (i64 autotmp address
					// — body went through linear-memory autotmp + zero-
					// init) is detected by the arg's per-value local
					// type being i64 rather than anyref; for it, drop
					// the args and emit struct.new_default so the func
					// validates with a zero struct (field values not
					// recoverable without the SSA composite pass).
					if nRegs == 1 && ssa.Wasm3IsAnyrefValue(mr.Args[argIdx]) {
						a := mr.Args[argIdx]
						argIdx++
						getValue64(s, a)
						pc := s.Prog(wasm.ARefCast)
						pc.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
						resultIdx++
						continue
					}
					for ri := 0; ri < nRegs; ri++ {
						a := mr.Args[argIdx]
						argIdx++
						getValue64(s, a)
						s.Prog(wasm.ADrop)
					}
					pn := s.Prog(wasm.AStructNewDefault)
					pn.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
					resultIdx++
					continue
				}
				for ri := range p.Registers {
					a := mr.Args[argIdx]
					argIdx++
					narrow := wasm3NarrowABI(regTypes[ri])
					if narrow {
						getValue32(s, a)
					} else {
						getValue64(s, a)
					}
					resultIdx++
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
				var wfType obj.WasmFieldType
				var wfTypeIdx int64
				if calleeWasmImport != nil && wasmFieldIdx < len(calleeWasmImport.Params) {
					wf := calleeWasmImport.Params[wasmFieldIdx]
					narrow = isNarrowWasmField(wf)
					wfType = wf.Type
					wfTypeIdx = wf.Offset
				}
				wasmFieldIdx++
				// Runtime type-descriptor / generic dictionary
				// args: the SSA arg is OpWasm3LoweredAddr (a symbol
				// address that materialises as an i64 linear-memory
				// constant), but the wasm3-lowered callee param is
				// a typed ref ((ref null \$T) for *struct T). Route
				// the arg through the descriptor's WasmGC identity
				// ref-global (R_WASMDESCRIPTOR, same mechanism the
				// IfaceMake itab/data wiring uses). This is what
				// makes generic stenciled functions callable: their
				// first arg is the GC-shape dictionary pointer
				// (*runtime.dict-shape-T), and without this
				// conversion the call validation fails with
				// `call[0] expected anyref, found i64.const`.
				if a.Op == ssa.OpWasm3LoweredAddr && calleeWasmImport == nil && wasm3ParamIsRef(regTypes[ri]) {
					if sym, ok := a.Aux.(*obj.LSym); ok && a.AuxInt == 0 {
						// Consume the linear-address operand (only
						// materialises the i64 symbol addr; no
						// linear-memory read) + drop, to keep the
						// OnWasmStack accounting balanced. Then push
						// the descriptor's WasmGC identity ref-
						// global. Mirrors IfaceMake's word emitter.
						getValue64(s, a)
						s.Prog(wasm.ADrop)
						pGet := s.Prog(wasm.AGlobalGet)
						pGet.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: sym}
						pGet.Mark = wasm.Wasm3DescriptorRef
						continue
					}
				}
				if narrow {
					getValue32(s, a)
				} else {
					getValue64(s, a)
				}
				// //go:wasmimport with WasmRef param: the import
				// signature declares the param as (ref null (exact $T))
				// per the boxed-ref ABI (paramsToWasmFields →
				// wasm3RefField for strings, etc.). The body's
				// per-value local for the arg is anyref; widen to the
				// exact ref via ref.cast before the call so the
				// validator accepts the arg type. Prime the per-
				// function collector with the prelude type so the
				// R_WASMTYPE remap finds it.
				if wfType == obj.WasmRef {
					wasm3EnsureCollector(s.FuncInfo())
					pCast := s.Prog(wasm.ARefCastNull)
					pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: wfTypeIdx}
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
		// Sub-word integer/bool fields are stored as wasm i32. The
		// SSA value being set (v.Args[1]) lives in an i64 per-value
		// local, so a narrowing i32.wrap_i64 must precede struct.set
		// when the field is sub-word. Symmetric to the i64.extend_i32_u
		// after FieldGet of the same field width. Use the leaf field
		// type (resolved via wasm3FieldAtOffset) rather than v.Args[1].
		// Type — SSA optimisations can leave the value's type as the
		// wider int64 even though the user wrote a narrowing conversion.
		var needsNarrow bool
		var refCastTo int64 = -1 // -1 means no ref-cast needed
		if leaf := wasm3FieldAtOffset(st, v.AuxInt); leaf != nil {
			needsNarrow = wasm3FieldGetNeedsI64Extend(leaf.Type)
			// Ref-typed fields (pointers, interfaces, slices, strings,
			// unsafe.Pointer) hold exact-typed references in the WasmGC
			// struct. The SSA value being stored lives in an anyref
			// local — ref.cast it to the field's exact ref type before
			// struct.set. Without this, the validator rejects with
			// `expected (ref null (exact $T)), found anyref`.
			if wasm3FieldValueNeedsRefCast(leaf.Type) {
				refCastTo = int64(wasm3RefSlotTypeIdx(s.FuncInfo(), leaf.Type))
			}
		}
		// Restrict to iface components: container/ring's autogen eq stores
		// the iface field's itab/data halves into a temp via per-field
		// stores at off+0/+8 (FieldSet path), which previously panicked.
		// String/slice components also reach this offset but pass a value
		// with no per-value local in some SSA shapes (multi-return tuple
		// extraction in internal/gover.Parse) — Reg() crashes in
		// getValue64. Until that shape is understood, fire only for ifaces.
		if fwidx, boxIdx, comp, ok := wasm3BoxedComponentAtOffset(s, st, v.AuxInt, 0); ok {
			if !wasm3MaybeEmitLoweredAddrAsRef(s, v.Args[0]) {
				getValue64(s, v.Args[0])
			}
			pCast := s.Prog(wasm.ARefCast)
			pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
			pf := s.Prog(wasm.AStructGet)
			pf.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
			pf.To = obj.Addr{Type: obj.TYPE_CONST, Offset: fwidx}
			pbc := s.Prog(wasm.ARefCast)
			pbc.From = obj.Addr{Type: obj.TYPE_CONST, Offset: boxIdx}
			getValue64(s, v.Args[1])
			// Slice/string boxed-component len/off/cap (comp != 0 for slice
			// data / string data / iface itab) are i32 fields per the
			// $go.slice / $go.string layout; narrow the i64 source.
			boxedCompNeedsNarrow := false
			if leaf := wasm3FieldAtOffset(st, v.AuxInt); leaf != nil {
				switch {
				case leaf.Type.IsSlice() && comp != 0:
					boxedCompNeedsNarrow = true
				case leaf.Type.IsString() && comp != 0:
					boxedCompNeedsNarrow = true
				}
			}
			if needsNarrow || boxedCompNeedsNarrow {
				s.Prog(wasm.AI32WrapI64)
			} else if refCastTo >= 0 {
				pRefCast := s.Prog(wasm.ARefCastNull)
				pRefCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: refCastTo}
			}
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
			if !wasm3MaybeEmitLoweredAddrAsRef(s, v.Args[0]) {
				getValue64(s, v.Args[0])
			}
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
				if needsNarrow {
					s.Prog(wasm.AI32WrapI64)
				} else if refCastTo >= 0 {
					pRefCast := s.Prog(wasm.ARefCastNull)
					pRefCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: refCastTo}
				}
				pe := s.Prog(wasm.AStructSet)
				pe.From = obj.Addr{Type: obj.TYPE_CONST, Offset: ebIdx}
				pe.To = obj.Addr{Type: obj.TYPE_CONST, Offset: efi}
			} else if ebIdx == -1 {
				// Scalar primitive element ([N]uint8/[N]int32/[N]float32/...):
				// the array.set with the static element index IS the access.
				// Stack: arrayref, index, value. Narrow the i64 source to i32
				// for sub-i64 integer storage (packed i8/i16 + i32) per the
				// same logic as OpWasm3ArraySet — floats stay at their wasm
				// width, i64 stays i64.
				elemT := wasm3FieldArrayElemAtOffset(st, v.AuxInt)
				getValue64(s, v.Args[1])
				if elemT != nil && !elemT.IsFloat() && elemT.Size() < 8 {
					s.Prog(wasm.AI32WrapI64)
				}
				pset := s.Prog(wasm.AArraySet)
				pset.From = obj.Addr{Type: obj.TYPE_CONST, Offset: abIdx}
			} else {
				// Whole boxed element (string/slice/interface): array.set the
				// element ref directly. Stack: arrayref, index, value.
				getValue64(s, v.Args[1])
				pset := s.Prog(wasm.AArraySet)
				pset.From = obj.Addr{Type: obj.TYPE_CONST, Offset: abIdx}
			}
			break
		}
		// Nested-struct write: mirror of the FieldGet nested path
		// (see comment there). `schedt.lock.key = v` no longer reaches
		// key's i32 via a single struct.set on a flattened box — lock
		// is now (ref $go.box.mutex), so we walk struct.get +
		// ref.cast chains down to the leaf's parent box, then
		// struct.set the leaf field. The inner-box mutate is shared
		// state (ref semantics), which is what we want for nested
		// struct fields under the de-flattening model.
		if path, leafField, ok := wasm3NestedStructPath(s, st, v.AuxInt); ok {
			if !wasm3MaybeEmitLoweredAddrAsRef(s, v.Args[0]) {
				getValue64(s, v.Args[0])
			}
			pCast := s.Prog(wasm.ARefCast)
			pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
			pTop := s.Prog(wasm.AStructGet)
			pTop.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
			pTop.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(path[0].fieldIdx)}
			for i := 0; i < len(path)-1; i++ {
				pCastN := s.Prog(wasm.ARefCast)
				pCastN.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(path[i].boxIdx)}
				pNext := s.Prog(wasm.AStructGet)
				pNext.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(path[i].boxIdx)}
				pNext.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(path[i+1].fieldIdx)}
			}
			leafBoxIdx := path[len(path)-1].boxIdx
			pCastLeaf := s.Prog(wasm.ARefCast)
			pCastLeaf.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(leafBoxIdx)}
			getValue64(s, v.Args[1])
			if needsNarrow {
				s.Prog(wasm.AI32WrapI64)
			} else if refCastTo >= 0 {
				pRefCast := s.Prog(wasm.ARefCastNull)
				pRefCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: refCastTo}
			}
			pset := s.Prog(wasm.AStructSet)
			pset.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(leafBoxIdx)}
			pset.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(leafField)}
			break
		}
		fieldIdx := wasm3FieldIndexAtOffset(s.FuncInfo(), st, v.AuxInt)
		if fieldIdx < 0 {
			// Sub-field byte offset (no wasm3 lowering). Drop the
			// args + value and emit nothing; the store is dropped.
			getValue64(s, v.Args[0])
			s.Prog(wasm.ADrop)
			getValue64(s, v.Args[1])
			s.Prog(wasm.ADrop)
			break
		}
		if !wasm3MaybeEmitLoweredAddrAsRef(s, v.Args[0]) {
			getValue64(s, v.Args[0])
		}
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
		getValue64(s, v.Args[1])
		if needsNarrow {
			s.Prog(wasm.AI32WrapI64)
		} else if refCastTo >= 0 {
			pRefCast := s.Prog(wasm.ARefCastNull)
			pRefCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: refCastTo}
		}
		p := s.Prog(wasm.AStructSet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(fieldIdx)}

	case ssa.OpWasm3BoxStore:
		// Write a *string/*slice/*interface cell: ref.cast the cell ref to
		// (ref $go.box.T) and struct.set field 0 = the boxed value. v.Aux is
		// the boxed pointee Go type, registered as the cell via collectBox.
		//
		// Narrow the value to the field's declared ref type before struct.set
		// — anyref isn't a subtype of (ref null $go.object) / (ref null
		// $go.box.<T>), so the validator rejects struct.set with "expected
		// (ref null N), found local.get of type anyref" without an explicit
		// ref.cast. Hits godebug_setUpdate's `*p = update` (cell holds a func
		// value typed as $go.object) and other heap-cell stores.
		bt := v.Aux.(*types.Type)
		cellIdx := wasm3RegisterStruct(s.FuncInfo(), bt)
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(cellIdx)}
		getValue64(s, v.Args[1])
		if wasm3FieldValueNeedsRefCast(bt) {
			refCastTo := int64(wasm3RefSlotTypeIdx(s.FuncInfo(), bt))
			pRefCast := s.Prog(wasm.ARefCastNull)
			pRefCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: refCastTo}
		}
		p := s.Prog(wasm.AStructSet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(cellIdx)}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}

	case ssa.OpWasm3NewScalarPtr:
		// TODO(polyptr-step-2): emit the construction sequence
		//   global.get $class.array.<T>
		//   i32.const 1; array.new_default $go.array.<T>
		//   i32.const 0  ;; offset
		//   struct.new $go.ptr.array.<X>
		// Until the class-instance globals + per-T cell-accessor
		// generation land, this op is unreachable.
		v.Fatalf("OpWasm3NewScalarPtr: codegen not yet implemented (polyptr migration step 2)")

	case ssa.OpWasm3PolyPtrLoad:
		// TODO(polyptr-step-2): polymorphic dispatch
		//   read $class field via struct.get $go.ptr.poly.<X> 0
		//   read $get funcref via struct.get $go.refclass.<X> 0
		//   call_ref $go.getter.<X> with the polymorphic ptr
		v.Fatalf("OpWasm3PolyPtrLoad: codegen not yet implemented (polyptr migration step 2)")

	case ssa.OpWasm3PolyPtrStore:
		// TODO(polyptr-step-2): polymorphic dispatch (set funcref).
		v.Fatalf("OpWasm3PolyPtrStore: codegen not yet implemented (polyptr migration step 2)")

	case ssa.OpWasm3PtrStore:
		// Polymorphic write through $go.ptr.poly.<X>: extract (base,
		// off) from the ptr, read the class's set funcref, call_ref
		// with (base, off, value). arg0=ptr, arg1=value, arg2=mem.
		wasm3EnsureCollector(s.FuncInfo())
		polyIdx, refclassIdx, _, setterIdx, _ := wasm3InteriorPtrClass(v.Args[1].Type)
		tmp := wasm3AllocAnyrefTempLocal(s)
		getValue64(s, v.Args[0])
		localSetIdx(s, tmp)
		// Stack target: [base, off, value, setter-funcref]; call_ref
		// pops in reverse.
		wasm3PtrField(s, tmp, 1, polyIdx) // base (anyref)
		wasm3PtrField(s, tmp, 2, polyIdx) // off (i32)
		getValue64(s, v.Args[1])          // value (i64/anyref/f32/f64)
		wasm3PtrField(s, tmp, 0, polyIdx) // class
		pGetSet := s.Prog(wasm.AStructGet)
		pGetSet.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(refclassIdx)}
		pGetSet.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1} // class.set
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

	case ssa.OpWasm3ResetLinearMemory:
		// M3.5 linear-memory bridge: rewind the bump pointer to
		// args[0] (i32). Single global.set on wasm global 0.
		getValue32(s, v.Args[0])
		p := s.Prog(wasm.AGlobalSet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm.Wasm3GlobalIndexLinearBumpPtr)}

	case ssa.OpWasm3Suspend:
		// M4 Phase 3: suspend $Wasm3TagIndexPark — yield to the
		// enclosing resume with a park-tag handler installed.
		ps := s.Prog(wasm.ASuspend)
		ps.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm.Wasm3TagIndexPark)}

	case ssa.OpWasm3RunInContCatchSuspend:
		// M4 Phase 4: wrap Aux's function in a cont, resume it with
		// a (on $park switch 0) handler that catches a wasm.Suspend
		// inside the body. Because V8 only implements the "switch"
		// clause kind (kHandlerSwitch=0x01) — the spec-accurate
		// "catch" kind (kHandlerCatch=0x00) fails compile with
		// "handler generates 0 operands, target block returns 1" —
		// the handler doesn't push the suspended cont onto the
		// operand stack at the block label. The enclosing block has
		// no result type; both completed-normally and suspended-and-
		// caught paths converge at the block end with an empty stack.
		// Phase 5's gp.wasm3Cont stash will need cont.bind or
		// explicit-stack-drop encoding once V8 supports it.
		sym, ok := v.Aux.(*obj.LSym)
		if !ok {
			v.Fatalf("OpWasm3RunInContCatchSuspend: v.Aux is not *obj.LSym: %T", v.Aux)
		}
		wasm3EnsureCollector(s.FuncInfo())
		// block — void result type (V8 (on $tag switch $label)
		// semantics: handler pushes only tag payloads, which is empty
		// for our park tag).
		s.Prog(wasm.ABlock)
		// ref.func $sym
		pf := s.Prog(wasm.ARefFunc)
		pf.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: sym}
		// cont.new $go.cont
		pn := s.Prog(wasm.AContNew)
		pn.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasmgc.TypeGoCont)}
		// resume $go.cont (on $park switch 0)
		pr := s.Prog(wasm.AResume)
		pr.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasmgc.TypeGoCont)}
		pr.To = obj.Addr{Type: obj.TYPE_BRANCH, Offset: 0}
		// end — both completed-normally and suspended-and-caught
		// converge here with empty stack.
		s.Prog(wasm.AEnd)

	case ssa.OpWasm3RunInCont:
		// M4 Phase 2: wrap Aux's function in a cont, resume it. The
		// cont's body type is (func) — no params, no results — so
		// the emitted sequence is:
		//   ref.func $sym                ;; (ref $go.goroutine.entry)
		//   cont.new $go.cont            ;; (ref $go.cont)
		//   resume $go.cont {}           ;; no result
		// Mem-typed op — handled in ssaGenValue (not ssaGenValueOnStack)
		// so the default-mem return path doesn't drop it.
		sym, ok := v.Aux.(*obj.LSym)
		if !ok {
			v.Fatalf("OpWasm3RunInCont: v.Aux is not *obj.LSym: %T", v.Aux)
		}
		wasm3EnsureCollector(s.FuncInfo())
		pf := s.Prog(wasm.ARefFunc)
		pf.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: sym}
		pn := s.Prog(wasm.AContNew)
		pn.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasmgc.TypeGoCont)}
		pr := s.Prog(wasm.AResume)
		pr.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasmgc.TypeGoCont)}
		pr.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0} // empty handler vec

	case ssa.OpWasm3ReadLinearMemory:
		// M3.5 linear-memory bridge: copy len(dst) bytes from linear
		// memory at src offset args[1] into the caller's []byte slice
		// args[0]. No allocation. Mirror of WriteLinearMemory:
		//
		//   $arr (anyref)     ← struct.get $go.slice.u8 0 (backing)
		//   $sliceOff (i32)   ← i32.wrap(struct.get $go.slice.u8 1)
		//   $n (i32)          ← i32.wrap(struct.get $go.slice.u8 2)
		//   $srcOff (i32)     ← args[1]
		//   $i (i32)          ← copy index
		//
		//   block { loop {
		//     local.get $i; local.get $n; i32.ge_u; br_if 1
		//     ;; array.set $go.bytes $arr (sliceOff+i) i32.load8_u(srcOff+i)
		//     local.get $arr; ref.cast $go.bytes
		//     local.get $sliceOff; local.get $i; i32.add
		//     local.get $srcOff;   local.get $i; i32.add; i32.load8_u 0
		//     array.set $go.bytes
		//     local.get $i; i32.const 1; i32.add; local.set $i
		//     br 0
		//   } end } end
		wasm3EnsureCollector(s.FuncInfo())
		bytesIdx := int64(wasmgc.TypeGoBytes)
		sliceIdx := int64(wasm3RegisterSliceStruct(s.FuncInfo(), types.NewSlice(types.Types[types.TUINT8])))

		// Cache slice arg locally; getValue64 can't be called repeatedly
		// on an OnWasmStack value without unbalancing the wasm stack.
		sliceLocal := wasm3AllocAnyrefTempLocal(s)
		arrLocal := wasm3AllocAnyrefTempLocal(s)
		sliceOffLocal := wasm3AllocI32TempLocal(s)
		nLocal := wasm3AllocI32TempLocal(s)
		srcOffLocal := wasm3AllocI32TempLocal(s)
		iLocal := wasm3AllocI32TempLocal(s)

		getValue64(s, v.Args[0])
		localSetIdx(s, sliceLocal)

		// $arr = struct.get $go.slice.u8 0
		localGetIdx(s, sliceLocal)
		pC1 := s.Prog(wasm.ARefCast)
		pC1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceIdx}
		pGB := s.Prog(wasm.AStructGet)
		pGB.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceIdx}
		pGB.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		localSetIdx(s, arrLocal)

		// $sliceOff = struct.get $go.slice.u8 1 (already i32)
		localGetIdx(s, sliceLocal)
		pC2 := s.Prog(wasm.ARefCast)
		pC2.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceIdx}
		pGO := s.Prog(wasm.AStructGet)
		pGO.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceIdx}
		pGO.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
		localSetIdx(s, sliceOffLocal)

		// $n = struct.get $go.slice.u8 2 (already i32)
		localGetIdx(s, sliceLocal)
		pC3 := s.Prog(wasm.ARefCast)
		pC3.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceIdx}
		pGN := s.Prog(wasm.AStructGet)
		pGN.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceIdx}
		pGN.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 2}
		localSetIdx(s, nLocal)

		// $srcOff = args[1] (already i32)
		getValue32(s, v.Args[1])
		localSetIdx(s, srcOffLocal)

		// $i = 0
		i32Const(s, 0)
		localSetIdx(s, iLocal)

		// block { loop { ... ; br 0 } end } end
		s.Prog(wasm.ABlock)
		s.Prog(wasm.ALoop)
		// if (i >= n) br 1
		localGetIdx(s, iLocal)
		localGetIdx(s, nLocal)
		s.Prog(wasm.AI32GeU)
		pBrEx := s.Prog(wasm.ABrIf)
		pBrEx.From = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
		// array.set $arr (sliceOff+i) (load8_u (srcOff+i))
		localGetIdx(s, arrLocal)
		pCG := s.Prog(wasm.ARefCast)
		pCG.From = obj.Addr{Type: obj.TYPE_CONST, Offset: bytesIdx}
		localGetIdx(s, sliceOffLocal)
		localGetIdx(s, iLocal)
		s.Prog(wasm.AI32Add)
		localGetIdx(s, srcOffLocal)
		localGetIdx(s, iLocal)
		s.Prog(wasm.AI32Add)
		pLd := s.Prog(wasm.AI32Load8U)
		pLd.From = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		pAS := s.Prog(wasm.AArraySet)
		pAS.From = obj.Addr{Type: obj.TYPE_CONST, Offset: bytesIdx}
		// i++
		localGetIdx(s, iLocal)
		i32Const(s, 1)
		s.Prog(wasm.AI32Add)
		localSetIdx(s, iLocal)
		// br 0
		pBrLp := s.Prog(wasm.ABr)
		pBrLp.From = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		s.Prog(wasm.AEnd) // end loop
		s.Prog(wasm.AEnd) // end block

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
		if wasm3IsRefSliceElem(auxSliceTypeSet.Elem()) {
			// Ref-typed element (string/slice/interface/etc.): the
			// value lives in an anyref local; cast to the array's
			// element ref type before array.set.
			elemRef := int64(wasm3RegisterSliceElemRef(s.FuncInfo(), auxSliceTypeSet.Elem()))
			pCastV := s.Prog(wasm.ARefCastNull)
			pCastV.From = obj.Addr{Type: obj.TYPE_CONST, Offset: elemRef}
		} else if !auxSliceTypeSet.Elem().IsFloat() && elemSize < 8 {
			s.Prog(wasm.AI32WrapI64)
		}
		p := s.Prog(wasm.AArraySet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: idx}

	case ssa.OpWasm3StoreInterior:
		// Write through a polymorphic fat pointer. arg0 is the polyptr
		// ref (always $go.ptr.poly.<X> since OpWasm3InteriorPtr
		// migrated); arg1 is the value; arg2 is mem. v.Aux is the
		// container *types.Type. Dual of LoadInterior; lives in
		// ssaGenValue (not ssaGenValueOnStack) because it is a
		// memory op — ssaGenValue's default returns early for memory
		// before delegating to ssaGenValueOnStack.
		//   1. Recover the typed container (local.tee the polyptr;
		//      ref.cast + struct.get 1 + ref.cast to container);
		//   2. Get the offset (local.get; ref.cast + struct.get 2);
		//   3. Push the value (narrowed/downcast as appropriate);
		//   4. array.set $container.
		containerType := v.Aux.(*types.Type)
		polyIdx, _, _, _, _ := wasm3InteriorPtrClass(containerType.Elem())
		containerIdx := int64(wasm3RegisterArrayBacking(s.FuncInfo(), containerType.Elem()))
		elemSize := containerType.Elem().Size()
		if elemSize > 8 {
			v.Fatalf("OpWasm3StoreInterior: unsupported elem size %d (composite pointees are deferred to a later piece)", elemSize)
		}
		iptrTmp := wasm3AllocAnyrefTempLocal(s)
		getValue64(s, v.Args[0])
		pTee := s.Prog(wasm.ALocalTee)
		pTee.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(iptrTmp)}
		// 1: typed container — polyptr field 1 (base anyref) then
		// downcast to (ref $container).
		pCast1 := s.Prog(wasm.ARefCast)
		pCast1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(polyIdx)}
		pGet0 := s.Prog(wasm.AStructGet)
		pGet0.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(polyIdx)}
		pGet0.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
		pCastC := s.Prog(wasm.ARefCast)
		pCastC.From = obj.Addr{Type: obj.TYPE_CONST, Offset: containerIdx}
		// 2: offset — polyptr field 2.
		pGetTmp := s.Prog(wasm.ALocalGet)
		pGetTmp.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(iptrTmp)}
		pCast2 := s.Prog(wasm.ARefCast)
		pCast2.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(polyIdx)}
		pGet1 := s.Prog(wasm.AStructGet)
		pGet1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(polyIdx)}
		pGet1.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 2}
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
		// MapKeysSet / MapValuesSet receive a slice ([]K / []V) from
		// the per-type mapassign generator. The map field stores the
		// backing array directly (ref null exact $array). Extract the
		// backing via struct.get $go.slice.<elem> field 0, then cast
		// to the exact array type before struct.set.
		if v.Op == ssa.OpWasm3MapKeysSet || v.Op == ssa.OpWasm3MapValuesSet {
			var elemT *types.Type
			if v.Op == ssa.OpWasm3MapKeysSet {
				elemT = mapType.Key()
			} else {
				elemT = mapType.Elem()
			}
			sliceT := types.NewSlice(elemT)
			sliceStructIdx := int64(wasm3RegisterSliceStruct(s.FuncInfo(), sliceT))
			arrIdx := int64(wasm3RegisterArrayBacking(s.FuncInfo(), elemT))
			// ref.cast slice value to typed slice struct
			pSCast := s.Prog(wasm.ARefCastNull)
			pSCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceStructIdx}
			// struct.get $go.slice.<T> field 0 — backing array ref
			pBacking := s.Prog(wasm.AStructGet)
			pBacking.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceStructIdx}
			pBacking.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
			// ref.cast backing to exact array type for the field slot
			pVCast := s.Prog(wasm.ARefCastNull)
			pVCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}
		} else {
			// MapUsedSet / MapCapSet: the used/cap fields are i32, and
			// the i64 source value lives in the per-value local — narrow
			// before struct.set.
			s.Prog(wasm.AI32WrapI64)
		}
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
		// Stash arg0 (the map ref) in a scratch anyref local: each of
		// the 4 struct.set sub-ops needs the ref pushed again, and a
		// single SSA Value can only be consumed by getValue64 once
		// (OnWasmStack/OnWasmStackSkipped is a one-shot count, per the
		// contract in ssaGenValue's default branch).
		mapRefTmp := wasm3AllocAnyrefTempLocal(s)
		getValue64(s, v.Args[0])
		pTeeMap := s.Prog(wasm.ALocalTee)
		pTeeMap.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(mapRefTmp)}
		// used = 0 — i32 (used field is i32 per collectMapStruct)
		pCast1 := s.Prog(wasm.ARefCast)
		pCast1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		i32Const(s, 0)
		pUsed := s.Prog(wasm.AStructSet)
		pUsed.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		pUsed.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		// cap = 0 — i32 (cap field is i32 per collectMapStruct)
		pGetMap2 := s.Prog(wasm.ALocalGet)
		pGetMap2.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(mapRefTmp)}
		pCast2 := s.Prog(wasm.ARefCast)
		pCast2.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		i32Const(s, 0)
		pCap := s.Prog(wasm.AStructSet)
		pCap.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		pCap.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
		// keys = null
		keyArrIdx := int64(wasm3RegisterArrayBacking(s.FuncInfo(), mapType.Key()))
		pGetMap3 := s.Prog(wasm.ALocalGet)
		pGetMap3.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(mapRefTmp)}
		pCast3 := s.Prog(wasm.ARefCast)
		pCast3.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		pNullK := s.Prog(wasm.ARefNull)
		pNullK.From = obj.Addr{Type: obj.TYPE_CONST, Offset: keyArrIdx}
		pKeys := s.Prog(wasm.AStructSet)
		pKeys.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		pKeys.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 2}
		// values = null
		valArrIdx := int64(wasm3RegisterArrayBacking(s.FuncInfo(), mapType.Elem()))
		pGetMap4 := s.Prog(wasm.ALocalGet)
		pGetMap4.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(mapRefTmp)}
		pCast4 := s.Prog(wasm.ARefCast)
		pCast4.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		pNullV := s.Prog(wasm.ARefNull)
		pNullV.From = obj.Addr{Type: obj.TYPE_CONST, Offset: valArrIdx}
		pVals := s.Prog(wasm.AStructSet)
		pVals.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		pVals.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 3}

	case ssa.OpWasm3ChanSlotsSet:
		// M4 chan-subtyping (doc/wasm3-chan-subtyping.md): write
		// $go.chan.<T>.slots[i] = v. ref.cast chan; struct.get $chan 7;
		// ref.cast non-null; <push i>; <push v>; array.set $arr_T.
		// Packed elements (i8/i16) need i32 narrowing; ref-typed
		// elements need ref.cast to the element's exact ref type.
		chanType, ok := v.Aux.(*types.Type)
		if !ok || !chanType.IsChan() {
			v.Fatalf("OpWasm3ChanSlotsSet: v.Aux is not a chan type: %v", v.Aux)
		}
		chanIdx := int64(wasm3RegisterChanStruct(s.FuncInfo(), chanType))
		bufArrIdx := int64(wasm3RegisterArrayBacking(s.FuncInfo(), chanType.Elem()))
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: chanIdx}
		pBuf := s.Prog(wasm.AStructGet)
		pBuf.From = obj.Addr{Type: obj.TYPE_CONST, Offset: chanIdx}
		pBuf.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 7}
		pNN := s.Prog(wasm.ARefCast)
		pNN.From = obj.Addr{Type: obj.TYPE_CONST, Offset: bufArrIdx}
		getValue64(s, v.Args[1])
		s.Prog(wasm.AI32WrapI64)
		getValue64(s, v.Args[2])
		elem := chanType.Elem()
		// Value width narrowing: a sub-i64 scalar slot (i8/i16/i32)
		// requires i32.wrap on the i64 register value before array.set
		// (the array's elem type is i32 / packed i8|i16 and a 64-bit
		// stack value fails validation). i64 / f32 / f64 / ref slots
		// match their register width and need no conversion.
		if p, ok := scalarPrim(elem.Kind()); ok && p == wasmgc.I32 {
			s.Prog(wasm.AI32WrapI64)
		}
		if elemRefIdx, ok := wasm3ChanBufElemRefIdx(s.FuncInfo(), elem); ok {
			pCastV := s.Prog(wasm.ARefCastNull)
			pCastV.From = obj.Addr{Type: obj.TYPE_CONST, Offset: elemRefIdx}
		}
		pSet := s.Prog(wasm.AArraySet)
		pSet.From = obj.Addr{Type: obj.TYPE_CONST, Offset: bufArrIdx}

	case ssa.OpWasm3ChanSlotsAlloc:
		// M4 chan-subtyping: $go.chan.<T>.slots = array.new_default
		// $arr_T cap; struct.set $chan 7.
		chanType, ok := v.Aux.(*types.Type)
		if !ok || !chanType.IsChan() {
			v.Fatalf("OpWasm3ChanSlotsAlloc: v.Aux is not a chan type: %v", v.Aux)
		}
		chanIdx := int64(wasm3RegisterChanStruct(s.FuncInfo(), chanType))
		bufArrIdx := int64(wasm3RegisterArrayBacking(s.FuncInfo(), chanType.Elem()))
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: chanIdx}
		getValue64(s, v.Args[1])
		s.Prog(wasm.AI32WrapI64)
		pNew := s.Prog(wasm.AArrayNewDefault)
		pNew.From = obj.Addr{Type: obj.TYPE_CONST, Offset: bufArrIdx}
		pSet := s.Prog(wasm.AStructSet)
		pSet.From = obj.Addr{Type: obj.TYPE_CONST, Offset: chanIdx}
		pSet.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 7}

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
		//
		// Type-assertion path: `(EqPtr itab target)` reaches here with
		// itab as anyref (IfaceItab → struct.get $go.iface 0) and
		// target as OpWasm3LoweredAddr {type:T} — the i64 linear
		// address of T's type descriptor. The descriptor also exists
		// as a WasmGC ref-global (R_WASMDESCRIPTOR), so substitute the
		// LoweredAddr's emission with drop + global.get to recover an
		// anyref operand and route through the ref.eq path. Same
		// rationale as OpWasm3IfaceMake's emitWord helper.
		isDescAddr := func(a *ssa.Value) bool {
			if a.Op != ssa.OpWasm3LoweredAddr {
				return false
			}
			_, ok := a.Aux.(*obj.LSym)
			return ok
		}
		emitAnyrefOperand := func(a *ssa.Value) {
			if ssa.Wasm3IsAnyrefValue(a) {
				getValue64(s, a)
				s.Prog(wasm.ARefCastEqref)
				return
			}
			// LoweredAddr of a descriptor sym: drop the materialized i64
			// address, then global.get the descriptor's identity
			// ref-global (R_WASMDESCRIPTOR rewrite at link time).
			sym := a.Aux.(*obj.LSym)
			getValue64(s, a)
			s.Prog(wasm.ADrop)
			pGet := s.Prog(wasm.AGlobalGet)
			pGet.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: sym}
			pGet.Mark = wasm.Wasm3DescriptorRef
			s.Prog(wasm.ARefCastEqref)
		}
		a0Ref := ssa.Wasm3IsAnyrefValue(v.Args[0]) || isDescAddr(v.Args[0])
		a1Ref := ssa.Wasm3IsAnyrefValue(v.Args[1]) || isDescAddr(v.Args[1])
		if a0Ref && a1Ref {
			emitAnyrefOperand(v.Args[0])
			emitAnyrefOperand(v.Args[1])
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
			if i == 0 && castIdx >= 0 && (a.Op == ssa.OpWasm3RefNullAny || a.Op == ssa.OpWasm3RefNull) {
				// Null backing for a zero string/slice ($go.string.bytes
				// and $go.slice.<T>.data are non-nullable refs): a
				// ref.cast on null would trap at runtime, so consume the
				// null and substitute a freshly allocated zero-length
				// array. This is the canonical zero value for these
				// aggregates — every empty "" or nil slice still owns a
				// real (possibly empty) backing.
				getValue64(s, a)
				s.Prog(wasm.ADrop)
				i32Const(s, 0)
				pf := s.Prog(wasm.AArrayNewDefault)
				pf.From = obj.Addr{Type: obj.TYPE_CONST, Offset: castIdx}
				continue
			}
			getValue64(s, a)
			if i == 0 && castIdx >= 0 {
				pc := s.Prog(wasm.ARefCast)
				pc.From = obj.Addr{Type: obj.TYPE_CONST, Offset: castIdx}
			}
			// $go.string / $go.slice.<T> header off/len/cap fields are i32
			// (see TypeGoString in cmd/internal/wasmgc/type.go and
			// collectSliceStruct in wasmtype.go). The arg flows as i64 in
			// the per-value local — narrow with i32.wrap_i64 before
			// struct.new consumes it. i == 0 is the backing ref (handled
			// above via castIdx).
			if i > 0 && aggT != nil && (aggT.IsSlice() || aggT.IsString()) {
				s.Prog(wasm.AI32WrapI64)
			}
			// Sub-word integer/bool struct fields are stored as wasm
			// i32. The arg lives in an i64 per-value local — narrow it.
			// Only applies for true Go-struct aggregates where each Go
			// field maps 1:1 to a struct.new arg (slice/string/iface
			// have their own multi-slot shape and use the castIdx
			// path above).
			if aggT != nil && aggT.IsStruct() && i < aggT.NumFields() {
				if wasm3FieldGetNeedsI64Extend(aggT.Field(i).Type) {
					s.Prog(wasm.AI32WrapI64)
				}
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
		// Sub-word integer/bool fields are stored as wasm i32 in the
		// struct but live in i64 per-value locals; widen so the local
		// type matches. Same treatment as OpWasm3FieldGet.
		if wasm3FieldGetNeedsI64Extend(v.Type) {
			s.Prog(wasm.AI64ExtendI32U)
		}

	case ssa.OpWasm3SliceData, ssa.OpWasm3SliceLength, ssa.OpWasm3SliceCapacity:
		// Boxed slice header field reads (doc/wasm3-slice-boxing.md):
		// struct.get $go.slice.<T> at the field's fixed index. data=0
		// (backing array ref, left as anyref in the per-value local),
		// len=2, cap=3 (i32 — see collectSliceStruct comment). ref.cast
		// the anyref base to (ref null exact $go.slice.<T>) before
		// struct.get. The SSA len/cap ops are typed Int64 (per-value
		// local is i64) so widen with i64.extend_i32_u after the
		// struct.get for len/cap.
		field := int64(0)
		switch v.Op {
		case ssa.OpWasm3SliceLength:
			field = 2
		case ssa.OpWasm3SliceCapacity:
			field = 3
		}
		sliceAux := v.Aux.(*types.Type)
		// The rewrite at Wasm3.rules:738 sets Aux={s.Type}; for some
		// shapes (e.g. mapaccessfull.w3.<map[*K]V> trampolines in
		// regexp/syntax) s ends up typed as *T (a polymorphic boxed
		// pointer rep) instead of []T. Deref to recover the slice
		// type so collectSliceStruct accepts it.
		if sliceAux != nil && sliceAux.IsPtr() && sliceAux.Elem() != nil && sliceAux.Elem().IsSlice() {
			sliceAux = sliceAux.Elem()
		}
		// Stub fallback: if Aux still isn't a slice (Aux is *T for some
		// non-slice T, observed in the map accessor trampolines for
		// map[*K]V on wasm3), emit a no-op that drops the input ref and
		// pushes a typed zero. Wasm3 codegen for these trampolines is
		// not reachable at runtime — the dispatch trampoline path was
		// changed in Phase D and the wasm3MapKeys/Values helpers
		// receive typed refs directly. The compiler still walks them so
		// it must not crash on the unreachable codegen path.
		if sliceAux == nil || !sliceAux.IsSlice() {
			getValue64(s, v.Args[0])
			s.Prog(wasm.ADrop)
			if v.Op == ssa.OpWasm3SliceLength || v.Op == ssa.OpWasm3SliceCapacity {
				s.Prog(wasm.AI64Const)
			} else {
				s.Prog(wasm.ARefNullAny)
			}
			break
		}
		sliceTypeIdx := int64(wasm3RegisterSliceStruct(s.FuncInfo(), sliceAux))
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCastNull)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceTypeIdx}
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceTypeIdx}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: field}
		if v.Op == ssa.OpWasm3SliceLength || v.Op == ssa.OpWasm3SliceCapacity {
			s.Prog(wasm.AI64ExtendI32U)
		}

	case ssa.OpWasm3SubString:
		// `s[i:j]` on a wasm3 string: emit struct.new $go.string
		// {bytes = s.bytes, off = s.off + i, len = j - i}. Strings
		// are immutable so the shared bytes backing is safe.
		// arg0 = original string ref, arg1 = i (i64), arg2 = j (i64).
		// Result on stack: the new $go.string ref (anyref).
		// Both args are cached in locals because getValue64 on an
		// OnWasmStack value isn't replayable. The string off/len
		// fields are i32 — arithmetic uses i32, narrowing the i64
		// indices once at cache time.
		wasm3EnsureCollector(s.FuncInfo())
		stringIdx := int64(wasmgc.TypeGoString)
		sLocal := wasm3AllocAnyrefTempLocal(s)
		iLocal := wasm3AllocI32TempLocal(s)
		getValue64(s, v.Args[0])
		localSetIdx(s, sLocal)
		getValue64(s, v.Args[1])
		s.Prog(wasm.AI32WrapI64)
		localSetIdx(s, iLocal)

		// field 0: bytes = struct.get $go.string s 0 (ref.cast first)
		localGetIdx(s, sLocal)
		pCB := s.Prog(wasm.ARefCastNull)
		pCB.From = obj.Addr{Type: obj.TYPE_CONST, Offset: stringIdx}
		pGB := s.Prog(wasm.AStructGet)
		pGB.From = obj.Addr{Type: obj.TYPE_CONST, Offset: stringIdx}
		pGB.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}

		// field 1: new_off = struct.get $go.string s 1 + i (all i32)
		localGetIdx(s, sLocal)
		pCO := s.Prog(wasm.ARefCastNull)
		pCO.From = obj.Addr{Type: obj.TYPE_CONST, Offset: stringIdx}
		pGO := s.Prog(wasm.AStructGet)
		pGO.From = obj.Addr{Type: obj.TYPE_CONST, Offset: stringIdx}
		pGO.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
		localGetIdx(s, iLocal)
		s.Prog(wasm.AI32Add)

		// field 2: new_len = j - i (i32, j arrives as i64; wrap)
		getValue64(s, v.Args[2])
		s.Prog(wasm.AI32WrapI64)
		localGetIdx(s, iLocal)
		s.Prog(wasm.AI32Sub)

		// struct.new $go.string
		pNew := s.Prog(wasm.AStructNew)
		pNew.From = obj.Addr{Type: obj.TYPE_CONST, Offset: stringIdx}

	case ssa.OpWasm3StringByte:
		// `s[i]` on a wasm3 string: emit array.get_u $go.bytes on the
		// string's bytes ref at index (s.off + i). arg0 = string ref,
		// arg1 = index (i64). No mem arg — PtrLoad (the lowered
		// pattern this rule matches) also doesn't carry one.
		//
		//   $s = arg0 (cached for two field reads)
		//   <get bytes> = struct.get $go.string 0 (ref.cast $s to $go.string)
		//   <get off>   = struct.get $go.string 1 (...) ; already i32
		//   array index = off + i (i32.add after wrap of arg1)
		//   array.get_u $go.bytes <bytes> <index> ; i64.extend_i32_u
		wasm3EnsureCollector(s.FuncInfo())
		bytesIdx := int64(wasmgc.TypeGoBytes)
		stringIdx := int64(wasmgc.TypeGoString)
		// Cache the string ref (read twice).
		sLocal := wasm3AllocAnyrefTempLocal(s)
		getValue64(s, v.Args[0])
		localSetIdx(s, sLocal)
		// bytes = struct.get $go.string 0
		localGetIdx(s, sLocal)
		pCastB := s.Prog(wasm.ARefCastNull)
		pCastB.From = obj.Addr{Type: obj.TYPE_CONST, Offset: stringIdx}
		pGB := s.Prog(wasm.AStructGet)
		pGB.From = obj.Addr{Type: obj.TYPE_CONST, Offset: stringIdx}
		pGB.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		// index = struct.get $go.string 1 (i32) + i32.wrap(arg1)
		localGetIdx(s, sLocal)
		pCastO := s.Prog(wasm.ARefCastNull)
		pCastO.From = obj.Addr{Type: obj.TYPE_CONST, Offset: stringIdx}
		pGO := s.Prog(wasm.AStructGet)
		pGO.From = obj.Addr{Type: obj.TYPE_CONST, Offset: stringIdx}
		pGO.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
		getValue64(s, v.Args[1])
		s.Prog(wasm.AI32WrapI64)
		s.Prog(wasm.AI32Add)
		// array.get_u $go.bytes
		pAG := s.Prog(wasm.AArrayGetU)
		pAG.From = obj.Addr{Type: obj.TYPE_CONST, Offset: bytesIdx}
		// widen byte (i32) to i64 to match the per-value local
		s.Prog(wasm.AI64ExtendI32U)

	case ssa.OpWasm3StringOffset:
		// struct.get $go.string s 1 — i32 offset field. Used by the
		// SliceMake(StringData)+StringOffset rule so a slice aliasing
		// a substring's bytes uses the right starting offset. The SSA
		// op is typed Int64 (the per-value local is i64) so widen with
		// i64.extend_i32_u after the struct.get.
		wasm3EnsureCollector(s.FuncInfo())
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCastNull)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasmgc.TypeGoString)}
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasmgc.TypeGoString)}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
		s.Prog(wasm.AI64ExtendI32U)

	case ssa.OpWasm3StringData, ssa.OpWasm3StringLength:
		// Boxed string component reads: struct.get $go.string at the fixed
		// field index (0=backing $go.bytes ref, 2=length i32). $go.string
		// is the prelude type TypeGoString; wasm3EnsureCollector primes the
		// per-function table so the R_WASMTYPE reloc remaps the index.
		// ref.cast the anyref base to (ref null exact $go.string) first —
		// struct.get rejects anyref bases.
		//
		// length field is i32 (per the int=i32 prelude). The SSA op is
		// typed Int64 and the per-value local is i64, so widen with
		// i64.extend_i32_u after the struct.get for the length case.
		wasm3EnsureCollector(s.FuncInfo())
		field := int64(0)
		if v.Op == ssa.OpWasm3StringLength {
			field = 2
		}
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCastNull)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasmgc.TypeGoString)}
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasmgc.TypeGoString)}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: field}
		if v.Op == ssa.OpWasm3StringLength {
			s.Prog(wasm.AI64ExtendI32U)
		}

	case ssa.OpWasm3IfaceItab, ssa.OpWasm3IfaceData:
		// Boxed interface component reads: struct.get $go.iface at the
		// fixed field index (0=itab ref, 1=data ref). $go.iface is the
		// prelude type TypeGoIface; both fields are anyref. ref.cast the
		// anyref base to (ref null exact $go.iface) first.
		wasm3EnsureCollector(s.FuncInfo())
		field := int64(0)
		if v.Op == ssa.OpWasm3IfaceData {
			field = 1
		}
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCastNull)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasmgc.TypeGoIface)}
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
		// Phase D step 2: register $go.vtab.<I> (and transitively the
		// per-method funcref types) and $go.iface.<I> at the construction
		// site so the linker's type table contains them even for an
		// interface I whose only use is construction (e.g. stored into a
		// struct field and never dispatched on in this object). The
		// linker's typed-vtab-global emission (step 3) depends on the
		// presence of $go.vtab.<I> in the module-wide type table.
		// Construction's v.Type is the static interface type (preserved
		// by the IMake -> IfaceMake rewrite via Value.reset, which does
		// not touch Type).
		if v.Type != nil && v.Type.IsInterface() && !v.Type.IsEmptyInterface() && len(v.Type.AllMethods()) > 0 {
			wasm3RegisterIfaceVtab(s.FuncInfo(), v.Type)
			wasm3RegisterIfaceSubtype(s.FuncInfo(), v.Type)
		}
		emitWord := func(a *ssa.Value, isDataSlot bool) {
			// ConstNil of itab/data slots: a Go SSA value of type uintptr
			// (the standard itab repr) or unsafe.Pointer rewrites to
			// I64Const [0] via the default ConstNil rule, but the
			// struct.new $go.iface field is anyref. Detect the zero-i64
			// case, consume the i64 value (to keep OnWasmStack accounting
			// balanced), drop it, then emit ref.null any so the field
			// type matches.
			if a.Op == ssa.OpWasm3I64Const && a.AuxInt == 0 {
				getValue64(s, a)
				s.Prog(wasm.ADrop)
				s.Prog(wasm.ARefNullAny)
				return
			}
			if a.Op == ssa.OpWasm3LoweredAddr {
				if sym, ok := a.Aux.(*obj.LSym); ok {
					// Consume + drop the linear address operand (only
					// materializes the symbol address; no linear read) to
					// keep the OnWasmStack accounting balanced, then read
					// the symbol's identity ref-global. Two flavors:
					//   - Type descriptor LSyms (`type:T`, `go:itab.I,T`)
					//     get the descriptorGlobals entry (struct.new_default
					//     $go.object), routed via Wasm3DescriptorRef →
					//     R_WASMDESCRIPTOR. These are the itab/type word of
					//     a non-empty interface and the type word of an
					//     empty interface.
					//   - Static-data LSyms (`main..stmp_N`, package vars)
					//     get the boxedGlobals entry, routed via Wasm3GlobalRef
					//     → R_WASMGLOBAL. The init expression for a stmp_N
					//     string-header static materializes a $go.string ref
					//     (see wasm3DecodeStringTemp); other statics get
					//     written by the package init. The data slot then
					//     needs the value wrapped in $go.box.<pointee> so
					//     the assertion site's ref.cast (ref $go.box.<T>);
					//     struct.get $go.box.<T> 0 pair finds the expected
					//     shape (matching the convention OpWasm3BoxLoad
					//     reads back).
					getValue64(s, a)
					s.Prog(wasm.ADrop)
					p := s.Prog(wasm.AGlobalGet)
					p.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: sym}
					if wasm3IsTypeDescriptorSym(sym) {
						p.Mark = wasm.Wasm3DescriptorRef
						return
					}
					p.Mark = wasm.Wasm3GlobalRef
					// Wrap the loaded value in a $go.box.<pointee> for the
					// data slot. a.Type is the SSA pointer type (e.g.
					// *string); the box wraps its element. The itab/type
					// slot never reaches this branch (its LSym is always a
					// descriptor and was handled above).
					//
					// The boxed-var global is declared (mut anyref) — the
					// linker doesn't know the variable's precise wasm
					// type — so the loaded anyref must be ref.cast down
					// to the box field's typed-ref shape (e.g. (ref null
					// $go.string)) before struct.new validates. Read the
					// field's heap-type from the collector's table after
					// registering the box so this works for any pointee
					// whose box has a single-ref field (string, slice,
					// interface — same set OpWasm3BoxLoad reads back).
					if isDataSlot && a.Type != nil && a.Type.IsPtr() {
						pointee := a.Type.Elem()
						boxIdx := wasm3RegisterStruct(s.FuncInfo(), pointee)
						if cAny, ok := wasm3LiveCollector.Load(s.FuncInfo()); ok {
							c := cAny.(*typeCollector)
							if len(c.table[boxIdx].Fields) == 1 &&
								c.table[boxIdx].Fields[0].Storage.RefType >= 0 {
								fieldHeapIdx := c.table[boxIdx].Fields[0].Storage.RefType
								pCast := s.Prog(wasm.ARefCastNull)
								pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(fieldHeapIdx)}
								pBox := s.Prog(wasm.AStructNew)
								pBox.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(boxIdx)}
							}
						}
					}
					return
				}
			}
			getValue64(s, a)
		}
		emitWord(v.Args[0], false)
		emitWord(v.Args[1], true)
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
		polyIdx, refclassIdx, _, _, _ := wasm3InteriorPtrClass(leaf.Type)
		// Build the polymorphic fat pointer. Field order is
		// {class, base, off}, so the inline class struct.new pops
		// first (deepest), then base (the container arg), then off.
		pg := s.Prog(wasm.ARefFunc)
		pg.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: reflectdata.WasmGCFieldGetter(st, v.AuxInt).Linksym()}
		ps := s.Prog(wasm.ARefFunc)
		ps.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: reflectdata.WasmGCFieldSetter(st, v.AuxInt).Linksym()}
		pcls := s.Prog(wasm.AStructNew)
		pcls.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(refclassIdx)}
		getValue64(s, v.Args[0]) // base = container ref
		i32Const(s, 0)            // off (unused for struct-field accessors; baked into the body)
		pn := s.Prog(wasm.AStructNew)
		pn.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(polyIdx)}

	case ssa.OpWasm3PtrLoad:
		// Polymorphic read through $go.ptr.poly.<X>: extract (base,
		// off), read class.get funcref via class indirection,
		// call_ref with (base, off). The ptr is read multiple times,
		// so stash in a scratch anyref local first.
		wasm3EnsureCollector(s.FuncInfo())
		polyIdx, refclassIdx, getterIdx, _, _ := wasm3InteriorPtrClass(v.Type)
		tmp := wasm3AllocAnyrefTempLocal(s)
		getValue64(s, v.Args[0])
		localSetIdx(s, tmp)
		// Stack target: [base, off, getter-funcref]; call_ref pops in reverse.
		wasm3PtrField(s, tmp, 1, polyIdx) // base (anyref)
		wasm3PtrField(s, tmp, 2, polyIdx) // off (i32)
		wasm3PtrField(s, tmp, 0, polyIdx) // class
		pGetGet := s.Prog(wasm.AStructGet)
		pGetGet.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(refclassIdx)}
		pGetGet.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0} // class.get
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
		// Sub-word integer/bool fields are stored as wasm i32 (per
		// wasm3IntField). The per-value local for v is i64 (wasm3
		// SSA register width), so a widening i64.extend_i32_u must
		// follow struct.get to match the local's type. Set the flag
		// now so each branch below can emit it after its terminal
		// struct.get / array.get.
		needsExtend := wasm3FieldGetNeedsI64Extend(v.Type)
		// Source base ptr: for a heap-allocated *T the per-value
		// local is anyref, and getValue64 + ref.cast just narrows
		// the supertype. For a LoweredAddr {LSym} the per-value
		// local is i64 (linear-memory address); ref.cast on an i64
		// fails wasm validation. Substitute the i64 emission with a
		// drop + global.get of the descriptor or boxed-var
		// counterpart so ref.cast sees an anyref. Same trick that
		// OpWasm3IfaceMake's emitWord, the call-arg passing path,
		// and OpWasm3I64Eq already use; this consumer is what was
		// missing for runtime accesses like sched.sudoglock
		// (`LoweredAddr {runtime.sched}` → struct.get sudoglock).
		if !wasm3MaybeEmitLoweredAddrAsRef(s, v.Args[0]) {
			getValue64(s, v.Args[0])
		}
		// The base (*struct) is now an anyref on the wasm stack —
		// either from getValue64 over an anyref-typed local, or
		// from the global.get we just emitted. ref.cast to (ref
		// $go.struct.T) narrows it for struct.get, which rejects
		// the anyref supertype.
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
		// Rung 34 "stop at inner box": if the result type is a pointer
		// to a struct (`*T` where T is a struct), and the byte offset
		// lands at the start of a top-level struct-typed field of st,
		// the field IS the *T value the caller wants — emit one
		// struct.get and stop. Without this short-circuit the nested-
		// struct path below would drill INTO the inner box and read its
		// leaf scalar, mismatching the anyref result local. The pattern
		// surfaces at every `lock(&sched.fooLock)` / `&g.someStruct`
		// site in the runtime.
		if v.Type.IsPtr() && v.Type.Elem() != nil && v.Type.Elem().IsStruct() {
			if fieldIdx := wasm3FieldIndexAtOffset(s.FuncInfo(), st, v.AuxInt); fieldIdx >= 0 {
				p := s.Prog(wasm.AStructGet)
				p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
				p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(fieldIdx)}
				break
			}
		}
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
			// Slice/string len/off/cap components (comp != 0) are i32
			// fields per the $go.slice / $go.string layout; the SSA op's
			// v.Type is the wider Go int (8 bytes), so needsExtend is
			// false — but we still must widen the i32 struct.get result
			// to the i64 per-value local. Iface components are anyref —
			// no widening.
			boxedCompNeedsExtend := false
			if leaf := wasm3FieldAtOffset(st, v.AuxInt); leaf != nil {
				switch {
				case leaf.Type.IsSlice() && comp != 0:
					boxedCompNeedsExtend = true
				case leaf.Type.IsString() && comp != 0:
					boxedCompNeedsExtend = true
				}
			}
			if needsExtend || boxedCompNeedsExtend {
				s.Prog(wasm.AI64ExtendI32U)
			}
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
			// Scalar-primitive elements ([N]uint8/[N]int32/...) need
			// array.get_u/get_s for packed (i8/i16) storage; whole-boxed-
			// ref and struct-element paths use plain array.get. Mirrors
			// the OpWasm3ArrayGet logic — pick get_u/get_s/get based on
			// elem size and signedness, then widen on load.
			scalarLoad := efi < 0 && ebIdx == -1
			var scalarElemT *types.Type
			var scalarSigned bool
			var scalarSize int64
			if scalarLoad {
				scalarElemT = wasm3FieldArrayElemAtOffset(st, v.AuxInt)
				if scalarElemT != nil {
					scalarSigned = scalarElemT.IsSigned()
					scalarSize = scalarElemT.Size()
				}
			}
			var getOp obj.As = wasm.AArrayGet
			if scalarLoad && scalarElemT != nil && !scalarElemT.IsFloat() {
				switch scalarSize {
				case 1, 2:
					if scalarSigned {
						getOp = wasm.AArrayGetS
					} else {
						getOp = wasm.AArrayGetU
					}
				}
			}
			pg := s.Prog(getOp)
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
			// interface) — the array.get result IS the value (a ref) —
			// or a scalar primitive — the array.get_u/get_s/get result
			// is the scalar that needs the standard sub-i32 → i64 widen.
			if scalarLoad && scalarElemT != nil && !scalarElemT.IsFloat() && scalarSize < 8 {
				// Sub-i64 integer/bool elements live in i64 per-value
				// locals; widen with the polarity matching get_u/get_s.
				if scalarSigned {
					s.Prog(wasm.AI64ExtendI32S)
				} else {
					s.Prog(wasm.AI64ExtendI32U)
				}
			} else if needsExtend {
				s.Prog(wasm.AI64ExtendI32U)
			}
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
			if needsExtend {
				s.Prog(wasm.AI64ExtendI32U)
			}
			break
		}
		// Nested-struct path: under Rung 34's de-flattening,
		// `pt.u.v` for atomic.Bool{u Uint8} doesn't reach v's i8
		// via a single struct.get on a flattened box — `u` is now a
		// (ref null $go.box.Uint8) field, and v lives one struct.get
		// deeper. wasm3NestedStructPath walks t.Fields() from
		// v.AuxInt, returns the chain of (fieldIdx, boxType) pairs
		// needed to drill through nested-struct boxes to the leaf
		// field. Used when the offset doesn't match a top-level
		// scalar/composite field directly but does land inside a
		// nested struct.
		if path, leafField, ok := wasm3NestedStructPath(s, st, v.AuxInt); ok {
			// Emit each step: top-level struct.get of the box-ref,
			// then ref.cast + struct.get the next level, ... until
			// we reach the leaf.
			pTop := s.Prog(wasm.AStructGet)
			pTop.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
			pTop.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(path[0].fieldIdx)}
			for i := 0; i < len(path); i++ {
				pCastN := s.Prog(wasm.ARefCast)
				pCastN.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(path[i].boxIdx)}
				if i == len(path)-1 {
					pLeaf := s.Prog(wasm.AStructGet)
					pLeaf.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(path[i].boxIdx)}
					pLeaf.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(leafField)}
				} else {
					pNext := s.Prog(wasm.AStructGet)
					pNext.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(path[i].boxIdx)}
					pNext.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(path[i+1].fieldIdx)}
				}
			}
			if needsExtend {
				s.Prog(wasm.AI64ExtendI32U)
			}
			break
		}
		fieldIdx := wasm3FieldIndexAtOffset(s.FuncInfo(), st, v.AuxInt)
		if fieldIdx < 0 {
			// Sub-field byte offset (no wasm3 lowering for byte-7-of-
			// uintptr-style access). Drop the ref input and push a
			// zero typed for the read; the consuming op gets a
			// well-typed but garbage value. Affects rare paths only
			// (reflect.Value.Method's sub-byte flag access).
			getValue64(s, v.Args[0])
			s.Prog(wasm.ADrop)
			s.Prog(wasm.AI64Const)
			break
		}
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(structIdx)}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(fieldIdx)}
		if needsExtend {
			s.Prog(wasm.AI64ExtendI32U)
		}

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

	case ssa.OpWasm3RefNullAny:
		// ref.null any — used for ConstNil of ref-typed Go values
		// (interface, slice, *T where T isn't a specific struct, etc.)
		// whose per-value local is anyref. The abstract anyref null
		// is a single (opcode, heaptype) byte pair with no typeidx
		// relocation. See ARefNullAny encoder in wasm3obj.go.
		s.Prog(wasm.ARefNullAny)

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
		// MapKeys/MapValues' SSA result type is []K / []V (per the
		// callMapHelper's `any → K/V` substitution). The map field
		// holds a bare array ref though. Wrap the array in a real
		// $go.slice.<elem> struct so SliceLen / IndexExpr / etc.
		// further down can use the standard wasm3 slice ops.
		if v.Op == ssa.OpWasm3MapKeys || v.Op == ssa.OpWasm3MapValues {
			var elemT *types.Type
			var field int64
			if v.Op == ssa.OpWasm3MapKeys {
				elemT = mapType.Key()
				field = 2
			} else {
				elemT = mapType.Elem()
				field = 3
			}
			sliceT := types.NewSlice(elemT)
			sliceStructIdx := int64(wasm3RegisterSliceStruct(s.FuncInfo(), sliceT))
			arrIdx := int64(wasm3RegisterArrayBacking(s.FuncInfo(), elemT))
			// First, get the backing array ref: ref.cast map; struct.get $map field
			getValue64(s, v.Args[0])
			pCast := s.Prog(wasm.ARefCast)
			pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
			pBacking := s.Prog(wasm.AStructGet)
			pBacking.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
			pBacking.To = obj.Addr{Type: obj.TYPE_CONST, Offset: field}
			// Map's keys/values field is (ref null exact $array). Slice
			// struct's data field is (ref exact $array) — non-null.
			// Coerce via ref.cast non-null. If the map's field is null
			// (empty map), this traps — callers must check used > 0
			// before invoking the slice indexing path.
			pCast.As = wasm.ARefCast // ensure non-null cast
			pNN := s.Prog(wasm.ARefCast)
			pNN.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}
			// Then the offset (i32 0 — slice off field is i32).
			i32Const(s, 0)
			// Then len = cap (the slice exposes the full backing
			// length so insert/refresh-in-place loops in mapassign
			// can index [0, cap)). The map's `used` is tracked
			// separately and used to bound the loop count. Both the
			// map's cap field and the slice's len/cap fields are i32,
			// so no width conversion is needed.
			getValue64(s, v.Args[0])
			pCast2 := s.Prog(wasm.ARefCast)
			pCast2.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
			pLen := s.Prog(wasm.AStructGet)
			pLen.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
			pLen.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1} // cap field
			// Then cap = struct.get $map field 1 (cap)
			getValue64(s, v.Args[0])
			pCast3 := s.Prog(wasm.ARefCast)
			pCast3.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
			pCap := s.Prog(wasm.AStructGet)
			pCap.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
			pCap.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
			// struct.new $go.slice.<elem> {backing, off, len, cap}
			pNew := s.Prog(wasm.AStructNew)
			pNew.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceStructIdx}
			break
		}
		var field int64
		switch v.Op {
		case ssa.OpWasm3MapUsed:
			field = 0
		case ssa.OpWasm3MapCap:
			field = 1
		}
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		p := s.Prog(wasm.AStructGet)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: mapIdx}
		p.To = obj.Addr{Type: obj.TYPE_CONST, Offset: field}
		// used/cap are i32 fields (see collectMapStruct comment); the
		// SSA op is typed Int64 so widen for the per-value local.
		s.Prog(wasm.AI64ExtendI32U)

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

	case ssa.OpWasm3MakeChan:
		// M4: `make(chan T[, n])` allocates a fresh $go.chan.<T>
		// WasmGC struct via struct.new_default $go.chan.<T>.
		// v.Aux is the chan *types.Type; wasm3RegisterChanStruct
		// resolves it to the wasm type index. The result is a
		// zero-init chan ref. Per-T send/recv ops are follow-up
		// work — for now this just gets the allocation past the
		// runtime.makechan calling-convention mismatch (which the
		// wasm3 backend can't currently marshal because the
		// *chantype arg lowers as raw i64 instead of anyref).
		chanType, ok := v.Aux.(*types.Type)
		if !ok || !chanType.IsChan() {
			v.Fatalf("OpWasm3MakeChan: v.Aux is not a chan type: %v", v.Aux)
		}
		p := s.Prog(wasm.AStructNewDefault)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm3RegisterChanStruct(s.FuncInfo(), chanType))}

	case ssa.OpWasm3ChanSlotsGet:
		// M4 chan-subtyping (doc/wasm3-chan-subtyping.md): read
		// $go.chan.<T>.slots[i]. ref.cast chan; struct.get $chan 7
		// (yields the typed slots array); <push i>; array.get /
		// array.get_u $arr_T (yields T). Packed elements (i8/i16)
		// need array.get_u; reference/i32/i64 use array.get.
		chanType, ok := v.Aux.(*types.Type)
		if !ok || !chanType.IsChan() {
			v.Fatalf("OpWasm3ChanSlotsGet: v.Aux is not a chan type: %v", v.Aux)
		}
		chanIdx := int64(wasm3RegisterChanStruct(s.FuncInfo(), chanType))
		bufArrIdx := int64(wasm3RegisterArrayBacking(s.FuncInfo(), chanType.Elem()))
		getValue64(s, v.Args[0])
		pCast := s.Prog(wasm.ARefCast)
		pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: chanIdx}
		pBuf := s.Prog(wasm.AStructGet)
		pBuf.From = obj.Addr{Type: obj.TYPE_CONST, Offset: chanIdx}
		pBuf.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 7}
		pNN := s.Prog(wasm.ARefCast)
		pNN.From = obj.Addr{Type: obj.TYPE_CONST, Offset: bufArrIdx}
		getValue64(s, v.Args[1])
		s.Prog(wasm.AI32WrapI64)
		// Element-kind-aware array.get + i32→i64 widening. The wasm3
		// SSA backend uses i64 GP registers (wasm3ValueType's generic
		// default is i64), but a (array i8/i16/i32) elem-typed
		// array.get pushes i32. Without widening, the result slips
		// into an i64 local and wasm validation rejects the local.set.
		// Signedness follows the Go kind: TINT* sign-extends,
		// TUINT*/TBOOL/etc. zero-extends.
		elem := chanType.Elem()
		signed := elem.IsSigned()
		getOp := wasm.AArrayGet
		needExtend := false
		switch elem.Size() {
		case 1, 2:
			if signed {
				getOp = wasm.AArrayGetS
			} else {
				getOp = wasm.AArrayGetU
			}
			needExtend = true
		case 4:
			if p, ok := scalarPrim(elem.Kind()); ok && p == wasmgc.I32 {
				needExtend = true
			}
		}
		pGet := s.Prog(getOp)
		pGet.From = obj.Addr{Type: obj.TYPE_CONST, Offset: bufArrIdx}
		if needExtend {
			if signed {
				s.Prog(wasm.AI64ExtendI32S)
			} else {
				s.Prog(wasm.AI64ExtendI32U)
			}
		}

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
		sliceType := v.Aux.(*types.Type)
		elem := sliceType.Elem()
		arrIdx := int64(wasm3RegisterArrayAux(s, v))
		getValue64(s, v.Args[1])
		s.Prog(wasm.AI32WrapI64)
		p := s.Prog(wasm.AArrayNewDefault)
		p.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}
		if elem.IsStruct() && elem.Size() > 0 {
			// Struct elements: each slot is a (ref null $go.struct.T)
			// that array.new_default initialised to null. Later
			// `slice[i].F = v` lowers to array.get → ref.cast → struct.set
			// and the ref.cast traps on the null slot. Pre-fill every
			// slot with a fresh struct.new_default so writes find a
			// real cell to mutate. Per-T fill loop emitted inline:
			//   $arr (anyref) ← tee from the array.new_default above
			//   $cap (i32)    ← i32.wrap(args[1])
			//   $i   (i32)    ← 0
			//   block { loop {
			//     local.get $i; local.get $cap; i32.ge_u; br_if 1
			//     local.get $arr; ref.cast $arr_T;
			//     local.get $i; struct.new_default $T; array.set $arr_T
			//     local.get $i; i32.const 1; i32.add; local.set $i
			//     br 0
			//   } end } end
			//   local.get $arr   ;; re-push for the localSet fall-through
			wasm3EnsureCollector(s.FuncInfo())
			structIdx := int64(wasm3RegisterStruct(s.FuncInfo(), elem))
			arrLocal := wasm3AllocAnyrefTempLocal(s)
			capLocal := wasm3AllocI32TempLocal(s)
			iLocal := wasm3AllocI32TempLocal(s)
			// stack currently has the newly-allocated array ref on top.
			localSetIdx(s, arrLocal)
			// $cap = i32.wrap(args[1])
			getValue64(s, v.Args[1])
			s.Prog(wasm.AI32WrapI64)
			localSetIdx(s, capLocal)
			// $i = 0
			i32Const(s, 0)
			localSetIdx(s, iLocal)
			s.Prog(wasm.ABlock)
			s.Prog(wasm.ALoop)
			localGetIdx(s, iLocal)
			localGetIdx(s, capLocal)
			s.Prog(wasm.AI32GeU)
			pBr := s.Prog(wasm.ABrIf)
			pBr.From = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
			localGetIdx(s, arrLocal)
			pCast := s.Prog(wasm.ARefCast)
			pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}
			localGetIdx(s, iLocal)
			pNew := s.Prog(wasm.AStructNewDefault)
			pNew.From = obj.Addr{Type: obj.TYPE_CONST, Offset: structIdx}
			pSet := s.Prog(wasm.AArraySet)
			pSet.From = obj.Addr{Type: obj.TYPE_CONST, Offset: arrIdx}
			localGetIdx(s, iLocal)
			i32Const(s, 1)
			s.Prog(wasm.AI32Add)
			localSetIdx(s, iLocal)
			pBrLp := s.Prog(wasm.ABr)
			pBrLp.From = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
			s.Prog(wasm.AEnd) // loop
			s.Prog(wasm.AEnd) // block
			// re-push the array ref so the default localSet sees it.
			localGetIdx(s, arrLocal)
		}

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
		// Push captures in order. For ref-typed captures
		// (pointer / chan / map / func / unsafe.Pointer) the
		// per-closureCtx field is a typed (ref null $T); the
		// getValue64 push yields anyref (the per-value local's
		// type), so an explicit ref.cast is needed before
		// struct.new consumes the value — anyref isn't a
		// subtype of (ref null $T).
		col, _ := wasm3LiveCollector.Load(s.FuncInfo())
		c := col.(*typeCollector)
		// Walk captureTypes (Go-level: one entry per Go capture) in
		// lockstep with v.Args (SSA-level: one entry per wasm field
		// in the closureCtx — composite captures like []T / string /
		// [N]int decompose into 3 / 2 / N args). captureClosureFields
		// is the per-Go-type expansion the body side already uses to
		// declare the matching struct fields; we mirror it here so the
		// caller's push count and field count agree.
		//
		// Refs only need an explicit ref.cast in the single-component
		// case (captureClosureFieldIsRef): a *T / chan / map / func /
		// unsafe.Pointer capture is one typed-ref field that the
		// anyref per-value local must be downcast to before struct.new.
		// Multi-component captures push only scalars (i64 for slice
		// len/cap and string len/data, i64 for array elements) plus
		// the slice's anyref backing — none need a cast.
		argIdx := 0
		for _, ct := range captureTypes {
			fields := c.captureClosureFields(ct)
			if len(fields) == 1 {
				if argIdx >= len(v.Args) {
					v.Fatalf("OpWasm3MakeClosureRefInline: %v argIdx %d out of range (len v.Args %d, captureTypes %v)", sym.Name, argIdx, len(v.Args), captureTypes)
				}
				getValue64(s, v.Args[argIdx])
				if refIdx, ok := c.captureClosureFieldIsRef(ct); ok {
					pCast := s.Prog(wasm.ARefCastNull)
					pCast.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(refIdx)}
				}
				argIdx++
				continue
			}
			for j := 0; j < len(fields); j++ {
				if argIdx >= len(v.Args) {
					v.Fatalf("OpWasm3MakeClosureRefInline: %v argIdx %d out of range mid-composite (capture %v field %d, len v.Args %d)", sym.Name, argIdx, ct, j, len(v.Args))
				}
				getValue64(s, v.Args[argIdx])
				argIdx++
			}
		}
		if argIdx != len(v.Args) {
			v.Fatalf("OpWasm3MakeClosureRefInline: %v consumed %d of %d v.Args from captureTypes %v", sym.Name, argIdx, len(v.Args), captureTypes)
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
		// Polymorphic fat-pointer for an interior slot of a wasmgc
		// container (slice/array element). v.Aux is the container's
		// *types.Type; arg0 is the container ref (anyref at the SSA
		// level); arg1 is the i64 element offset (narrowed to i32).
		//
		// Build a $go.ptr.poly.<X> with an inline $go.refclass.<X>
		// holding refs to the per-T array-element accessors
		// (assembleWasm3PolyAccessor emits their hand-assembled
		// `ref.cast (ref (array T)); array.get/set` bodies). The
		// emitted layout is {class, base, off}. Pointer transit
		// through chan slots, returns, and globals all see this
		// uniform polyptr shape, removing the iptr-vs-polyptr
		// mismatch the prior split caused. Local `*p = v` / `*p`
		// still goes through LoadInterior/StoreInterior, which now
		// consume the polyptr layout (fields 1/2 instead of the old
		// iptr's 0/1) — see those cases below.
		containerType := v.Aux.(*types.Type)
		elemT := containerType.Elem()
		polyIdx, refclassIdx, _, _, _ := wasm3InteriorPtrClass(elemT)
		wasm3EnsureCollector(s.FuncInfo())
		getterSym := wasm3GetOrGenArrayAccessor(elemT, false)
		setterSym := wasm3GetOrGenArrayAccessor(elemT, true)
		pgRef := s.Prog(wasm.ARefFunc)
		pgRef.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: getterSym}
		psRef := s.Prog(wasm.ARefFunc)
		psRef.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: setterSym}
		pcls := s.Prog(wasm.AStructNew)
		pcls.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(refclassIdx)}
		getValue64(s, v.Args[0])
		getValue64(s, v.Args[1])
		s.Prog(wasm.AI32WrapI64)
		pNew := s.Prog(wasm.AStructNew)
		pNew.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(polyIdx)}

	case ssa.OpWasm3LoadInterior:
		// Read through a polymorphic fat pointer. arg0 is the polyptr
		// ref (always $go.ptr.poly.<X> since OpWasm3InteriorPtr
		// migrated); v.Aux is the container's *types.Type. Emits:
		//   1. ref.cast anyref to (ref $go.ptr.poly.<X>);
		//      struct.get field 1 -> base anyref;
		//   2. ref.cast to (ref $container_wasmgc_type);
		//   3. ref.cast polyptr again; struct.get field 2 -> off i32;
		//   4. array.get_s/_u/plain on $container.
		// Skips the class indirection (no struct.get class; call_ref)
		// because the container element type is known statically here
		// — the polymorphic path through PtrLoad's call_ref is for
		// callers that don't know the elem type at the deref site
		// (e.g. ptr-typed function parameters).
		containerType := v.Aux.(*types.Type)
		polyIdx, _, _, _, _ := wasm3InteriorPtrClass(containerType.Elem())
		containerIdx := int64(wasm3RegisterArrayBacking(s.FuncInfo(), containerType.Elem()))
		elemSize := containerType.Elem().Size()
		signed := v.Type.IsSigned()
		iptrTmp := wasm3AllocAnyrefTempLocal(s)
		getValue64(s, v.Args[0])
		pTee := s.Prog(wasm.ALocalTee)
		pTee.To = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(iptrTmp)}
		// 1: base ref — cast polyptr from anyref to polyptr-typed,
		// struct.get field 1 (base, anyref).
		pCast1 := s.Prog(wasm.ARefCast)
		pCast1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(polyIdx)}
		pGet0 := s.Prog(wasm.AStructGet)
		pGet0.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(polyIdx)}
		pGet0.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
		// 2: typed container ref
		pCastC := s.Prog(wasm.ARefCast)
		pCastC.From = obj.Addr{Type: obj.TYPE_CONST, Offset: containerIdx}
		// 3: offset — re-fetch polyptr (anyref) and re-cast; field 2.
		pGetTmp := s.Prog(wasm.ALocalGet)
		pGetTmp.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(iptrTmp)}
		pCast2 := s.Prog(wasm.ARefCast)
		pCast2.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(polyIdx)}
		pGet1 := s.Prog(wasm.AStructGet)
		pGet1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(polyIdx)}
		pGet1.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 2}
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

	case ssa.OpWasm3WriteLinearMemory:
		// M3.5 linear-memory bridge: copy a []byte slice's bytes into
		// the linear-memory scratch arena and return the byte offset
		// of the copy. The bump pointer is wasm global 0
		// (writeGlobalSec3 reserves it; previously dead, repurposed
		// here). Linear memory is grown on demand via memory.grow.
		// arg0 = data []byte (anyref / ref $go.slice.u8), arg1 = mem.
		// The slice header is {backing $go.bytes, off i64, len i64,
		// cap i64}; cracks into backing+off+len to honor sub-sliced
		// inputs (where off > 0 or len < cap < array.len(backing)).
		//
		//     local $arr  (anyref)  ← data, ref.cast'd to $go.bytes
		//     local $n    (i32)     ← array.len $arr
		//     local $off  (i32)     ← saved bump pointer (return value)
		//     local $i    (i32)     ← byte-copy index
		//
		//     ;; cast and stash data ref
		//     <getValue arg0>; ref.cast $go.bytes; local.set $arr
		//
		//     ;; length
		//     local.get $arr; array.len; local.set $n
		//
		//     ;; saved offset
		//     global.get 0; local.set $off
		//
		//     ;; grow memory if needed:
		//     ;;   pages_needed = ((off+n) - memory.size*65536 + 65535) >> 16
		//     ;;   if pages_needed > 0: memory.grow pages_needed
		//     ;;     (trap on -1 via unreachable)
		//     local.get $off; local.get $n; i32.add        ;; end = off+n
		//     memory.size; i32.const 16; i32.shl           ;; bytes available
		//     i32.gt_u                                     ;; end > bytes ?
		//     if
		//         ;; pages = (end - bytes_avail + 65535) >> 16, but simpler:
		//         ;; pages = ((end-1)>>16) - memory.size + 1
		//         local.get $off; local.get $n; i32.add
		//         i32.const 1; i32.sub
		//         i32.const 16; i32.shr_u
		//         memory.size
		//         i32.sub
		//         i32.const 1; i32.add
		//         memory.grow
		//         i32.const -1; i32.eq
		//         if; unreachable; end                     ;; OOM trap
		//     end
		//
		//     ;; byte copy: for i := 0; i < n; i++ {
		//     ;;     i32.store8(off+i, array.get_u arr i)
		//     ;; }
		//     i32.const 0; local.set $i
		//     block
		//         loop
		//             local.get $i; local.get $n; i32.ge_u
		//             br_if 1                              ;; exit block on i >= n
		//             ;; addr = off+i
		//             local.get $off; local.get $i; i32.add
		//             ;; byte = array.get_u $arr $i
		//             local.get $arr; local.get $i; array.get_u $go.bytes
		//             i32.store8 offset=0
		//             ;; i++
		//             local.get $i; i32.const 1; i32.add; local.set $i
		//             br 0
		//         end
		//     end
		//
		//     ;; advance bump and produce result on stack
		//     local.get $off; local.get $n; i32.add; global.set 0
		//     local.get $off
		wasm3EnsureCollector(s.FuncInfo())
		bytesIdx := int64(wasmgc.TypeGoBytes)
		sliceIdx := int64(wasm3RegisterSliceStruct(s.FuncInfo(), types.NewSlice(types.Types[types.TUINT8])))

		// Allocate temp locals.
		//   $slice (anyref) ← cached slice arg (read three times for
		//                     backing / off / len; getValue64 can't be
		//                     called repeatedly on an OnWasmStack value)
		//   $arr (anyref) ← the $go.bytes backing
		//   $sliceOff (i32) ← the slice's offset into its backing
		//   $n (i32) ← the slice's length
		//   $off (i32) ← saved bump pointer (return value)
		//   $i (i32) ← byte-copy index
		sliceLocal := wasm3AllocAnyrefTempLocal(s)
		arrLocal := wasm3AllocAnyrefTempLocal(s)
		sliceOffLocal := wasm3AllocI32TempLocal(s)
		nLocal := wasm3AllocI32TempLocal(s)
		offLocal := wasm3AllocI32TempLocal(s)
		iLocal := wasm3AllocI32TempLocal(s)

		// Cache the slice arg in $slice; subsequent field reads use
		// local.get + ref.cast.
		getValue64(s, v.Args[0])
		localSetIdx(s, sliceLocal)

		// $arr = struct.get $go.slice.u8 0
		localGetIdx(s, sliceLocal)
		pCastSlice1 := s.Prog(wasm.ARefCast)
		pCastSlice1.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceIdx}
		pGetBacking := s.Prog(wasm.AStructGet)
		pGetBacking.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceIdx}
		pGetBacking.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		localSetIdx(s, arrLocal)

		// $sliceOff = struct.get $go.slice.u8 1 (already i32)
		localGetIdx(s, sliceLocal)
		pCastSlice2 := s.Prog(wasm.ARefCast)
		pCastSlice2.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceIdx}
		pGetSliceOff := s.Prog(wasm.AStructGet)
		pGetSliceOff.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceIdx}
		pGetSliceOff.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
		localSetIdx(s, sliceOffLocal)

		// $n = struct.get $go.slice.u8 2 (already i32)
		localGetIdx(s, sliceLocal)
		pCastSlice3 := s.Prog(wasm.ARefCast)
		pCastSlice3.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceIdx}
		pGetSliceLen := s.Prog(wasm.AStructGet)
		pGetSliceLen.From = obj.Addr{Type: obj.TYPE_CONST, Offset: sliceIdx}
		pGetSliceLen.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 2}
		localSetIdx(s, nLocal)

		// $off = global.get 0 (current bump pointer)
		pGG := s.Prog(wasm.AGlobalGet)
		pGG.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm.Wasm3GlobalIndexLinearBumpPtr)}
		localSetIdx(s, offLocal)

		// Grow-if-needed: if off+n > memory.size*65536, grow.
		// end_addr = $off + $n
		localGetIdx(s, offLocal)
		localGetIdx(s, nLocal)
		s.Prog(wasm.AI32Add)
		// bytes_avail = memory.size << 16
		s.Prog(wasm.ACurrentMemory)
		i32Const(s, 16)
		s.Prog(wasm.AI32Shl)
		// end_addr > bytes_avail ?
		s.Prog(wasm.AI32GtU)
		// if (need-grow) ...
		pIfGrow := s.Prog(wasm.AIf)
		_ = pIfGrow
		// pages = ((end_addr - 1) >> 16) - memory.size + 1
		// (compute end_addr again from off+n; locals make this cheap)
		localGetIdx(s, offLocal)
		localGetIdx(s, nLocal)
		s.Prog(wasm.AI32Add)
		i32Const(s, 1)
		s.Prog(wasm.AI32Sub)
		i32Const(s, 16)
		s.Prog(wasm.AI32ShrU)
		s.Prog(wasm.ACurrentMemory)
		s.Prog(wasm.AI32Sub)
		i32Const(s, 1)
		s.Prog(wasm.AI32Add)
		// memory.grow
		s.Prog(wasm.AGrowMemory)
		// if (== -1) unreachable
		i32Const(s, -1)
		s.Prog(wasm.AI32Eq)
		pIfOOM := s.Prog(wasm.AIf)
		_ = pIfOOM
		s.Prog(wasm.AUnreachable)
		s.Prog(wasm.AEnd) // end of OOM-if
		s.Prog(wasm.AEnd) // end of grow-if

		// $i = 0
		i32Const(s, 0)
		localSetIdx(s, iLocal)

		// block { loop { ... ; br 0 } end } end
		s.Prog(wasm.ABlock)
		s.Prog(wasm.ALoop)
		//   if (i >= n) br 1  (out of block)
		localGetIdx(s, iLocal)
		localGetIdx(s, nLocal)
		s.Prog(wasm.AI32GeU)
		pBrExit := s.Prog(wasm.ABrIf)
		pBrExit.From = obj.Addr{Type: obj.TYPE_CONST, Offset: 1}
		//   addr = off + i
		localGetIdx(s, offLocal)
		localGetIdx(s, iLocal)
		s.Prog(wasm.AI32Add)
		//   byte = array.get_u $go.bytes (ref.cast $arr) (sliceOff+i)
		localGetIdx(s, arrLocal)
		pCastG := s.Prog(wasm.ARefCast)
		pCastG.From = obj.Addr{Type: obj.TYPE_CONST, Offset: bytesIdx}
		localGetIdx(s, sliceOffLocal)
		localGetIdx(s, iLocal)
		s.Prog(wasm.AI32Add)
		pAG := s.Prog(wasm.AArrayGetU)
		pAG.From = obj.Addr{Type: obj.TYPE_CONST, Offset: bytesIdx}
		//   i32.store8 offset=0 (align=0)
		pSt := s.Prog(wasm.AI32Store8)
		pSt.To = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		//   i++
		localGetIdx(s, iLocal)
		i32Const(s, 1)
		s.Prog(wasm.AI32Add)
		localSetIdx(s, iLocal)
		//   br 0 (back to loop)
		pBrLoop := s.Prog(wasm.ABr)
		pBrLoop.From = obj.Addr{Type: obj.TYPE_CONST, Offset: 0}
		s.Prog(wasm.AEnd) // end loop
		s.Prog(wasm.AEnd) // end block

		// advance bump: global 0 = off + n
		localGetIdx(s, offLocal)
		localGetIdx(s, nLocal)
		s.Prog(wasm.AI32Add)
		pGS := s.Prog(wasm.AGlobalSet)
		pGS.From = obj.Addr{Type: obj.TYPE_CONST, Offset: int64(wasm.Wasm3GlobalIndexLinearBumpPtr)}

		// Result (left on stack via the default localSetIdx fall-
		// through): the saved offset. wasm3 per-value locals for
		// scalar values are i64, so widen the i32 offset.
		localGetIdx(s, offLocal)
		s.Prog(wasm.AI64ExtendI32U)

	case ssa.OpEmpty:
		// Zero-sized value sentinel (e.g. a struct{} or [0]T arg passed
		// to a call). The SSA generic rules elide most consumers via
		// `(Store _ (Empty) mem) => mem` etc., but some survive — most
		// notably call args via per-T chan trampolines (chan struct{}
		// in internal/gate) and generic instantiations (net/netip via
		// unique.canonmap). Emit nothing; codegen for a zero-sized
		// value is a no-op. checkLower's wasm3 exemption (see
		// cmd/compile/internal/ssa/lower.go) lets these survive that
		// far when t.Size() == 0.

	case ssa.OpWasm3ContNew, ssa.OpWasm3ContBind, ssa.OpWasm3Resume, ssa.OpWasm3ResumeThrow, ssa.OpWasm3Switch:
		// M4 Phase 1: the stack-switching SSA ops are declared so the
		// SSA framework generates the constants and the assembler
		// recognises their opcode bytes (cmd/internal/obj/wasm). No
		// rewrite rule emits them yet; Phase 3 wires runtime.contNew /
		// contSuspend / contResume / contSwitch through ssagen
		// intrinsics and lands the real lowering here (handler-vec
		// encoding, typeidx / tagidx Aux unpacking, multi-result
		// SelectN decomposition for Resume / Switch). Any value
		// landing here today is a bug.
		v.Fatalf("wasm3 stack-switching SSA op %s reached codegen before Phase 3 lowering exists", v.Op)

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

// wasm3IsTypeDescriptorSym reports whether sym is a Go type-system
// descriptor (a `*runtime._type` for a concrete type, an itab, or a
// type-switch descriptor) as opposed to a regular data symbol.
//
// The wasm3 linker maintains two parallel ref-global tables:
//   - descriptorGlobals (R_WASMDESCRIPTOR / Wasm3DescriptorRef): one
//     opaque struct.new_default $go.object per descriptor sym, used
//     only for type identity (ref.eq comparison) and method dispatch
//     (for itab descriptors with the per-itab vtab aux).
//   - boxedGlobals (R_WASMGLOBAL / Wasm3GlobalRef): one (mut anyref)
//     per package-level var or static, holding the actual value ref
//     (e.g. a $go.string for a stmp_N string-header static).
//
// The same LoweredAddr+LSym SSA shape reaches both. The sym name is
// the deciding signal: type descriptors land at `type:` (concrete
// types) and `go:itab.` (itabs) names. Everything else — package
// vars, compiler-generated statics (`pkg..stmp_N`), string literal
// data (`go:string."..."`) — needs the value-carrying boxed global.
//
// Used by OpWasm3IfaceMake's emitWord to pick which global table the
// `iface.{itab,data}` slots index into; mis-routing the data slot to
// the descriptor table is what made `panic("boom") + recover() +
// r.(string)` trap with "illegal cast" at the assertion site.
func wasm3IsTypeDescriptorSym(sym *obj.LSym) bool {
	if sym == nil {
		return false
	}
	name := sym.Name
	return strings.HasPrefix(name, "type:") ||
		strings.HasPrefix(name, "go:itab.")
}

// wasm3MaybeEmitLoweredAddrAsRef checks whether a is an
// OpWasm3LoweredAddr {LSym} and, if so, emits the appropriate
// "load the descriptor/var as an anyref" sequence — `drop +
// global.get $sym` marked Wasm3DescriptorRef or Wasm3GlobalRef
// depending on the LSym class — and returns true. The drop
// consumes the LoweredAddr's i64 emission to keep the
// OnWasmStack accounting balanced; the global.get pushes the
// anyref the caller wants.
//
// Returns false (no emission) if a isn't a usable LoweredAddr +
// LSym pair; the caller falls back to plain getValue64.
//
// This is the consolidation of the LoweredAddr-as-ref trick that
// previously lived inline in OpWasm3IfaceMake's emitWord, the
// call-arg passing path for typed-ref params, the OpWasm3I64Eq
// type-assert path, and several others. As of the acquireSudog
// fix it also fires from OpWasm3FieldGet (and any other ref-
// expecting consumer of LoweredAddr).
func wasm3MaybeEmitLoweredAddrAsRef(s *ssagen.State, a *ssa.Value) bool {
	if a.Op != ssa.OpWasm3LoweredAddr {
		return false
	}
	sym, ok := a.Aux.(*obj.LSym)
	if !ok {
		return false
	}
	getValue64(s, a)
	s.Prog(wasm.ADrop)
	pGet := s.Prog(wasm.AGlobalGet)
	pGet.From = obj.Addr{Type: obj.TYPE_MEM, Name: obj.NAME_EXTERN, Sym: sym}
	if wasm3IsTypeDescriptorSym(sym) {
		pGet.Mark = wasm.Wasm3DescriptorRef
	} else {
		pGet.Mark = wasm.Wasm3GlobalRef
	}
	return true
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
		// Sub-field byte access (e.g. byte 7 of a uintptr in
		// reflect.Value, or an offset inside a [N]primitive array
		// that wasm3ArrayComponentAtOffset hasn't handled). Return
		// -1 so the caller can emit a stub instead of fataling at
		// compile time. The runtime semantics are undefined for the
		// rare cases that reach this codegen path; they typically
		// originate from generic-rule decompositions that the wasm3
		// backend can't fully handle.
		return -1
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
		// Two cases for embedded pointer flattening:
		//  - off lands inside the pointer field's own byte range
		//    [f.Offset, f.Offset+PtrSize): the compiler treats this
		//    as if the pointed-to struct were value-embedded.
		//  - off lands at or after f.Offset and the corresponding
		//    interior offset is a valid field of the pointed-to
		//    struct: same compiler flattening, with the offset
		//    measured against the pointed-to struct's layout
		//    (e.g. funcInfo.pcfile = embedded *_func + offset 20
		//    where 20 is _func.pcfile).
		if f.Type.IsPtr() && f.Type.Elem() != nil && f.Type.Elem().IsStruct() && f.Embedded != 0 {
			inner := f.Type.Elem()
			innerOff := off - f.Offset
			if innerOff >= 0 && innerOff < inner.Size() {
				if iIdx, iok := wasm3FieldIndexRec(c, inner, innerOff, 0); iok {
					return widx, c.collectStruct(inner), iIdx, true
				}
			}
		}
		widx += n
	}
	return 0, 0, 0, false
}

// nestedStructStep is one hop in a nested-struct access chain: the
// field index to read from the *previous* container, and the box
// type to ref.cast the result to before reading the next field.
type nestedStructStep struct {
	fieldIdx int  // wasm field index in the previous-step container
	boxIdx   uint32 // $go.box.<T> for the nested struct at this step (cast target)
}

// wasm3NestedStructPath walks t's fields looking for the path through
// nested-struct boxes that reaches byte offset off. Returns the chain
// of (fieldIdx, boxIdx) steps + the wasm field index of the leaf
// inside the deepest box. The codegen replays this as:
//
//	struct.get $st path[0].fieldIdx          ;; -> (ref box.A)
//	ref.cast box.A                            ;; for any A in the chain
//	struct.get box.A path[1].fieldIdx         ;; -> (ref box.B)
//	... (one ref.cast + struct.get per step) ...
//	struct.get box.Last leafField             ;; the leaf primitive/ref
//
// ok=false when off lands on a direct top-level field of t (no
// nesting needed — the caller's plain struct.get path is correct)
// or when off doesn't correspond to any addressable leaf (e.g.
// reaches into an array element).
//
// This is the byte-offset-through-nested-box codegen path Rung 34
// needs because the de-flattened layout no longer makes a nested
// struct's inner fields directly addressable from the outer
// container's wasm field list.
func wasm3NestedStructPath(s *ssagen.State, t *types.Type, off int64) (path []nestedStructStep, leafField int, ok bool) {
	cAny, has := wasm3LiveCollector.Load(s.FuncInfo())
	if !has {
		return nil, 0, false
	}
	c := cAny.(*typeCollector)
	cur := t
	curOff := off
	for {
		var match *types.Field
		var fieldIdx int
		for i, f := range cur.Fields() {
			if !f.Type.IsStruct() {
				// Only nested-struct fields trigger the chain.
				// Non-struct fields at the current offset are
				// handled by the caller's plain or
				// wasm3BoxedComponentAtOffset path.
				continue
			}
			if curOff >= f.Offset && curOff < f.Offset+f.Type.Size() {
				match = f
				fieldIdx = i
				break
			}
		}
		if match == nil {
			return nil, 0, false
		}
		// Compute the wasm field index for `match` in cur. The
		// nested struct itself occupies 1 wasm field (the box ref)
		// under Rung 34, so the wasm index equals the count of
		// `len(lowerFields(prev_field.Type))` summed across
		// preceding fields.
		widx := 0
		for i := 0; i < fieldIdx; i++ {
			widx += len(c.lowerFields(cur.Fields()[i].Type))
		}
		path = append(path, nestedStructStep{
			fieldIdx: widx,
			boxIdx:   uint32(c.collectBox(match.Type)),
		})
		// Descend into the nested struct.
		cur = match.Type
		curOff -= match.Offset
		// Check whether the (now-inner) offset lands on a deeper
		// nested struct or on a leaf field. If it lands on a leaf,
		// resolve via wasm3FieldIndexRec and return — that's the
		// final struct.get the codegen emits inside the deepest
		// box.cast. If it lands on yet another nested struct, the
		// next loop iteration extends the path.
		hasDeeperNest := false
		for _, f := range cur.Fields() {
			if f.Type.IsStruct() && curOff >= f.Offset && curOff < f.Offset+f.Type.Size() {
				hasDeeperNest = true
				break
			}
		}
		if !hasDeeperNest {
			if leaf, lok := wasm3FieldIndexRec(c, cur, curOff, 0); lok {
				return path, leaf, true
			}
			return nil, 0, false
		}
		// Loop body retries with the new (cur, curOff).
	}
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
//
// elemBoxIdx/elemFieldIdx encoding for the caller:
//
//   - struct element field: elemBoxIdx >= 0 (element struct's wasm type),
//     elemFieldIdx >= 0 (field index within that struct). Codegen emits
//     array.get then struct.get/struct.set on the resolved field.
//   - whole boxed ref element (string/slice/iface array, array-of-array):
//     elemBoxIdx == 0, elemFieldIdx == -1. The array.get/array.set result
//     IS the value (a ref).
//   - scalar primitive element ([N]uint8/[N]int32/[N]float32/...):
//     elemBoxIdx == -1, elemFieldIdx == -1. The array.get_u/get_s/get
//     result is the scalar (needs i64 widening on load, i32-narrowing
//     on store for sub-i32 packed elements). elemBoxIdx==-1 is the
//     unambiguous sentinel that distinguishes "scalar" from "whole
//     boxed ref" so codegen knows whether to ref.cast the value.
func wasm3ArrayComponentAtOffset(s *ssagen.State, t *types.Type, off, base int64) (arrayFieldWasmIdx, arrayBackingIdx, elemIdx, elemBoxIdx, elemFieldIdx int64, ok bool) {
	cAny, has := wasm3LiveCollector.Load(s.FuncInfo())
	if !has {
		return
	}
	c := cAny.(*typeCollector)
	widx := base
	for _, f := range t.Fields() {
		n := int64(len(c.lowerFields(f.Type)))
		if off >= f.Offset && off < f.Offset+f.Type.Size() {
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
			case f.Type.IsArray() && f.Type.Elem().IsArray():
				// Array-of-array (e.g. pcvalueCache.entries
				// [2][8]pcvalueCacheEnt): outer element is itself an
				// array. Resolve to whole-inner-array access. The
				// elemBoxIdx is the inner array's backing type and
				// elemFieldIdx = -1 signals "no trailing struct.get".
				elemT := f.Type.Elem()
				esz := elemT.Size()
				if esz == 0 || sub%esz != 0 {
					return
				}
				return widx, int64(wasm3RegisterArrayBacking(s.FuncInfo(), elemT)), sub / esz, 0, -1, true
			case f.Type.IsArray() && wasm3ArrayElemIsScalar(f.Type.Elem()):
				// Scalar-primitive array element ([N]uint8 / [N]int32 /
				// [N]float32 / ...). The array lowers to a single
				// (array i8|i16|i32|i64|f32|f64) backing; the array.get/
				// array.set with the static element index IS the access
				// — no trailing struct.get. Use elemBoxIdx = -1 as the
				// sentinel that distinguishes the scalar path from the
				// whole-boxed-ref path (elemBoxIdx = 0 there) so codegen
				// knows to narrow/widen the scalar value rather than
				// ref.cast it.
				//
				// Restrict to sub > 0: a whole-field access (sub == 0)
				// is a read/write of the array REF itself (e.g.
				// FieldGet <[N]uint8> for the InteriorPtr base), which
				// wasm3FieldIndexAtOffset handles via plain struct.get/
				// struct.set on the array-ref field. Only sub > 0 means
				// "into the array's bytes" — the abiDesc.inRegPtrs[1]
				// case at byte offset 177 that this path exists for.
				if sub == 0 {
					return
				}
				elemT := f.Type.Elem()
				esz := elemT.Size()
				if esz == 0 || sub%esz != 0 {
					return
				}
				return widx, int64(wasm3RegisterArrayBacking(s.FuncInfo(), elemT)), sub / esz, -1, -1, true
			case f.Type.IsStruct():
				return wasm3ArrayComponentAtOffset(s, f.Type, off-f.Offset, widx)
			}
			return
		}
		widx += n
	}
	return
}

// wasm3ArrayElemIsScalar reports whether elemT is a scalar primitive
// type ([N]elemT can be lowered to a single (array packed/i32/i64/f32/
// f64) backing). Mirrors the scalar set the array-backing collector
// supports — bool/int*/uint*/uintptr/unsafe.Pointer-as-scalar/float*.
// Note unsafe.Pointer is excluded: it lowers to anyref, not a scalar
// (array (ref any)) element which lives outside this scalar path.
func wasm3ArrayElemIsScalar(elemT *types.Type) bool {
	if elemT == nil {
		return false
	}
	if elemT.IsBoolean() || elemT.IsInteger() || elemT.IsFloat() {
		return true
	}
	return false
}

// wasm3ParamIsRef reports whether a Go parameter type t lowers to a
// typed WasmGC reference at the wasm-level call ABI (vs. a primitive
// i64/i32/f32/f64). Mirrors wasm3PointerIsRef's logic from the SSA
// layer's local-typing — used at the call site to decide whether an
// OpWasm3LoweredAddr arg needs the descriptor-ref-global routing.
//
// All pointer-shaped Go types lower to anyref on wasm3 per the WasmGC-
// only memory model: pointers reference heap objects which are WasmGC
// refs, never linear-memory addresses. The previous restriction to
// *struct / *array / unsafe.Pointer missed *uint8 (used as an opaque
// type-descriptor placeholder by reflectdata.TypePtrAt /
// reflectdata.ConvIfaceSrcRType) and missed maps / chans / funcs /
// interfaces (all heap-object refs at the wasm3 ABI). M5 iface phase B:
// broaden the gate so the OpWasm3LoweredAddr-of-descriptor routing
// fires at every iface conversion site, not just the few that happen
// to take *struct or unsafe.Pointer.
func wasm3ParamIsRef(t *types.Type) bool {
	if t == nil {
		return false
	}
	if t.IsPtr() {
		return true
	}
	if t.IsUnsafePtr() {
		return true
	}
	if t.IsMap() || t.IsChan() || t.IsInterface() {
		return true
	}
	if t.Kind() == types.TFUNC {
		return true
	}
	return false
}

// wasm3FieldValueNeedsRefCast reports whether a value of Go type t,
// when stored into a wasm3 struct field of that type, needs a
// ref.cast from anyref to the field's exact ref type. wasm3 stores
// pointers / interfaces / slices / strings / unsafe.Pointer as
// typed WasmGC references; the SSA value lives in an anyref local,
// so an explicit ref.cast is required before struct.set to match the
// field's exact-ref slot type.
func wasm3FieldValueNeedsRefCast(t *types.Type) bool {
	if t == nil {
		return false
	}
	if t.IsUnsafePtr() {
		return true
	}
	if t.IsPtr() && t.Elem() != nil && (t.Elem().IsStruct() || t.Elem().IsArray()) {
		return true
	}
	// *scalar (e.g. *uint32, *int64) lowers to (ref $go.ptr.poly.<class>)
	// via the polyptr scheme — a typed ref that's distinct from anyref.
	// FieldSet of a *scalar value (sitting in an anyref local) into a
	// polyptr-typed field needs the narrowing ref.cast, same as the
	// *struct / *array cases above. Surfaces at runtime.semaWaiters.addr =
	// addr (the addr field is typed *uint32 → $go.ptr.poly.i64).
	if t.IsPtr() && t.Elem() != nil {
		elem := t.Elem()
		if (elem.IsInteger() || elem.IsBoolean() || elem.IsFloat()) && elem.Size() > 0 {
			return true
		}
	}
	// func / map / chan fields lower to (ref null TypeGoObject) per
	// lowerFieldsImpl's TFUNC/TMAP/TCHAN case; the SSA value lives in
	// an anyref local, so the same anyref→(ref null TypeGoObject)
	// narrowing applies.
	switch t.Kind() {
	case types.TFUNC, types.TMAP, types.TCHAN:
		return true
	}
	// String / slice / interface fields lower to a typed ref (the
	// boxed header struct) per lowerFieldsImpl. The SSA value lives
	// in an anyref local, so a ref.cast to the typed slot is
	// required when a struct literal or direct field assignment hits
	// the simple FieldSet path (not the boxed-component path). The
	// boxed-component path in FieldSet uses its own cast — this
	// covers the whole-field assignment.
	if t.IsString() || t.IsSlice() || t.IsInterface() {
		return true
	}
	return false
}

// wasm3RefSlotTypeIdx returns the wasm type index for the ref-slot
// type that field-value stores of Go type t require ref-casting to.
// For unsafe.Pointer that's TypeGoObject (the open base type) — the
// declared field is `(ref null (exact $go.object))`. For *T (struct
// or array) it's the WasmGC type index of T's $go.struct.<T> /
// $go.array.<T> entry.
func wasm3RefSlotTypeIdx(fi *obj.FuncInfo, t *types.Type) uint32 {
	if t.IsUnsafePtr() {
		wasm3EnsureCollector(fi)
		return uint32(wasmgc.TypeGoObject)
	}
	if t.IsPtr() && t.Elem() != nil {
		elem := t.Elem()
		if elem.IsStruct() {
			return wasm3RegisterStruct(fi, elem)
		}
		if elem.IsArray() {
			return wasm3RegisterArrayBacking(fi, elem.Elem())
		}
		// *scalar polyptr: resolve via wasm3InteriorPtrClass which
		// returns the $go.ptr.poly.<class> wasm type index. The
		// SSA value is a typed ref to that polyptr struct.
		if elem.IsInteger() || elem.IsBoolean() || elem.IsFloat() {
			wasm3EnsureCollector(fi)
			polyIdx, _, _, _, _ := wasm3InteriorPtrClass(elem)
			return polyIdx
		}
	}
	if t.IsString() {
		wasm3EnsureCollector(fi)
		return uint32(wasmgc.TypeGoString)
	}
	if t.IsSlice() {
		return wasm3RegisterSliceStruct(fi, t)
	}
	if t.IsInterface() {
		wasm3EnsureCollector(fi)
		return uint32(wasmgc.TypeGoIface)
	}
	return uint32(wasmgc.TypeGoObject)
}

// wasm3FieldGetNeedsI64Extend reports whether the result of a
// struct.get (or array.get) of a value of type t needs to be widened
// from wasm i32 to i64 before storing into the per-value local.
//
// wasm3IntField stores sub-word integer / bool fields as wasm i32 in
// the WasmGC struct (because int32, int16, etc. lower to obj.WasmI32).
// The per-value local for an SSA value of these Go types is i64 (per
// wasm3ValueType's default — the wasm3 SSA backend works in i64 GP
// registers, with narrowing only at the ABI boundary). So a FieldGet
// of a sub-word integer field needs an i64.extend_i32_u between the
// struct.get and the local.set, matching the OpArgIntReg prologue
// pattern. Wider integers (int / int64 / uintptr) are already i64.
// Floats are f32/f64 with matching local widths. Pointers/anyref
// don't go through this path.
func wasm3FieldGetNeedsI64Extend(t *types.Type) bool {
	if t == nil {
		return false
	}
	if !t.IsInteger() && !t.IsBoolean() {
		return false
	}
	return t.Size() <= 4
}

// wasm3FieldArrayElemAtOffset returns the element type of the array
// field of struct st that covers byte offset off — recursing into inlined
// nested struct fields. Used by the array-component codegen to recover
// the element width/signedness needed to pick array.get_u/get_s/get
// (load) or whether to narrow the i64 source for array.set (store). The
// caller already established (via wasm3ArrayComponentAtOffset) that off
// lands inside a scalar-element array field; this helper just looks up
// the element type. Returns nil if no array field covers off.
func wasm3FieldArrayElemAtOffset(st *types.Type, off int64) *types.Type {
	for _, f := range st.Fields() {
		if off >= f.Offset && off < f.Offset+f.Type.Size() {
			if f.Type.IsArray() {
				return f.Type.Elem()
			}
			if f.Type.IsStruct() {
				return wasm3FieldArrayElemAtOffset(f.Type, off-f.Offset)
			}
			return nil
		}
	}
	return nil
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

// wasm3InteriorPtrClass returns the polymorphic-fat-pointer prelude
// type indices for an interior pointer whose pointee is t:
//   polyIdx     — $go.ptr.poly.<class>, the polymorphic ptr struct
//                  with {class, base, off} layout.
//   refclassIdx — $go.refclass.<class>, the {get, set} vtable struct
//                  the polyptr's class field references.
//   getterIdx   — $go.getter.<class>, the legacy funcref sig the
//                  vtable's get field holds; signature is (anyref
//                  base, i32 off) -> leaf so the dispatcher can
//                  call_ref it with (base, off) extracted from the
//                  polyptr.
//   setterIdx   — $go.setter.<class>, the legacy setter funcref sig.
//   isRef       — true if the pointee uses the ref/anyref class.
//
// Integer/bool pointees use the i64 class (converting accessors widen
// sub-word ints); unsafe.Pointer and other reference pointees use the
// ref class (anyref-valued accessors); floats use f32/f64.
//
// Replaces the OLD $go.ptr.<class> design (which embedded the get/set
// funcrefs per pointer) with the class-indirected polymorphic design
// — class instances are shared across all pointers of a (container,
// field) pair, saving a ref's worth of storage per pointer in trade
// for one extra struct.get in the dispatch.
func wasm3InteriorPtrClass(t *types.Type) (polyIdx, refclassIdx, getterIdx, setterIdx uint32, isRef bool) {
	if t != nil && (t.IsUnsafePtr() || t.IsPtr() || t.IsInterface() || t.IsSlice() || t.IsString()) {
		return wasmgc.TypeGoPtrPolyRef, wasmgc.TypeGoRefClassRef, wasmgc.TypeGoGetterRef, wasmgc.TypeGoSetterRef, true
	}
	if t != nil && t.IsFloat() {
		if t.Kind() == types.TFLOAT32 {
			return wasmgc.TypeGoPtrPolyF32, wasmgc.TypeGoRefClassF32, wasmgc.TypeGoGetterF32, wasmgc.TypeGoSetterF32, false
		}
		return wasmgc.TypeGoPtrPolyF64, wasmgc.TypeGoRefClassF64, wasmgc.TypeGoGetterF64, wasmgc.TypeGoSetterF64, false
	}
	return wasmgc.TypeGoPtrPolyI64, wasmgc.TypeGoRefClassI64, wasmgc.TypeGoGetterI64, wasmgc.TypeGoSetterI64, false
}

// wasm3ArrayAccessorMeta is the side-table entry for a pending
// polymorphic-fat-pointer accessor LSym: it records what
// wasm3SetupPolyAccessor (the assemble3 hook) needs to install on
// the accessor's FuncInfo once it exists — the element Go type, the
// leaf class/size/signedness, and the get-vs-set flag. The element
// Go type is needed at hook time to register the (ref (array T))
// backing in the accessor's own wasmgc.Table.
type wasm3ArrayAccessorMeta struct {
	elemT  *types.Type
	leaf   byte // 0=i64, 1=ref, 2=f32, 3=f64
	isSet  bool
	signed bool
}

// wasm3ArrayAccessorCache memoises the per-(elemT, isSet) array-elem
// accessor LSyms so multiple OpWasm3InteriorPtr sites for the same
// element type share one accessor pair. Key is `<kind>.<linkstring>`.
// Serialization of the DeclFunc path is via the shared
// reflectdata.Wasm3AccessorMu (also used by WasmGCFieldGetter/Setter)
// — necessary because ir.CurFunc/funcStack are globals and a race
// between two such generators trips enqueueFunc's CurFunc guard.
var wasm3ArrayAccessorCache sync.Map // string -> *obj.LSym

// wasm3ArrayAccessorMetaTable maps each generated accessor LSym to
// the meta wasm3SetupPolyAccessor uses to lazily wire it. Populated
// by wasm3GetOrGenArrayAccessor; consumed (once) by the hook.
var wasm3ArrayAccessorMetaTable sync.Map // *obj.LSym -> *wasm3ArrayAccessorMeta

// wasm3GetOrGenArrayAccessor returns the obj.LSym of the per-T array-
// element accessor (get if !isSet, set if isSet) used as the
// $go.refclass.<X> vtable funcref for OpWasm3InteriorPtr's
// polymorphic fat pointer.
//
// The accessor's body is hand-assembled by assembleWasm3PolyAccessor
// (obj/wasm/wasm3obj.go) — `ref.cast (ref (array T))` and
// `array.get/set` aren't expressible in Go IR. The function is still
// registered as an ir.Func so the link symbol exists and the compile
// queue picks it up; the trivial Go body it carries is thrown away
// at assemble time.
//
// FuncInfo wiring (set Wasm3PolyAccessor, register the array backing
// in the accessor's own wasmgc.Table) is deferred to
// wasm3SetupPolyAccessor — it can't happen here because the LSym's
// FuncInfo isn't created until the accessor's own compile pass.
func wasm3GetOrGenArrayAccessor(elemT *types.Type, isSet bool) *obj.LSym {
	kind := "arrayget"
	if isSet {
		kind = "arrayset"
	}
	cacheKey := kind + "." + elemT.LinkString()
	if cached, ok := wasm3ArrayAccessorCache.Load(cacheKey); ok {
		return cached.(*obj.LSym)
	}
	reflectdata.Wasm3AccessorMu.Lock()
	defer reflectdata.Wasm3AccessorMu.Unlock()
	if cached, ok := wasm3ArrayAccessorCache.Load(cacheKey); ok {
		return cached.(*obj.LSym)
	}

	int64T := types.Types[types.TINT64]
	int32T := types.Types[types.TINT32]
	unsafePtrT := types.Types[types.TUNSAFEPTR]

	leafClass := byte(0) // i64 class — default for integer/bool
	leafType := int64T
	if elemT.IsFloat() {
		if elemT.Kind() == types.TFLOAT32 {
			leafClass = 2
			leafType = types.Types[types.TFLOAT32]
		} else {
			leafClass = 3
			leafType = types.Types[types.TFLOAT64]
		}
	} else if elemT.IsUnsafePtr() || elemT.IsPtr() || elemT.IsInterface() ||
		elemT.IsSlice() || elemT.IsString() {
		leafClass = 1
		leafType = unsafePtrT
	}

	sym := types.TypeSymLookup("." + kind + ".w3." + elemT.LinkString())
	if sym.Def != nil {
		ls := sym.Def.(*ir.Name).Func.Linksym()
		wasm3ArrayAccessorCache.Store(cacheKey, ls)
		return ls
	}
	pos := base.AutogeneratedPos
	base.Pos = pos

	var params []*types.Field
	params = append(params,
		types.NewField(pos, typecheck.Lookup("base"), unsafePtrT),
		types.NewField(pos, typecheck.Lookup("off"), int32T),
	)
	var results []*types.Field
	if isSet {
		params = append(params, types.NewField(pos, typecheck.Lookup("v"), leafType))
	} else {
		results = append(results, types.NewField(pos, nil, leafType))
	}
	fn := ir.NewFunc(pos, pos, sym, types.NewSignature(nil, params, results))
	sym.Def = fn.Nname
	fn.Pragma |= ir.Noinline
	typecheck.DeclFunc(fn)
	// Trivial body — assemble3's Wasm3PolyAccessor dispatch replaces
	// the actual emission, so the SSA-compiled output is thrown away.
	// Crucially, the body must be NON-EMPTY: enqueueFunc treats an
	// empty body as a bodyless intrinsic and skips compilation, which
	// means assemble3 never runs and the hand-assembled body never
	// emits. A single return statement suffices.
	if isSet {
		fn.Body.Append(ir.NewReturnStmt(pos, nil))
	} else {
		var zero ir.Node
		switch {
		case elemT.IsFloat():
			zero = ir.NewBasicLit(pos, leafType, constant.MakeFloat64(0))
		case leafClass == 1:
			zero = typecheck.NodNil()
		default:
			zero = ir.NewInt(pos, 0)
		}
		fn.Body.Append(ir.NewReturnStmt(pos, []ir.Node{zero}))
	}
	typecheck.FinishFuncBody()
	fn.SetDupok(true)
	ir.WithFunc(fn, func() { typecheck.Stmts(fn.Body) })
	fn.SetNilCheckDisabled(true)

	ls := fn.Linksym()
	wasm3ArrayAccessorMetaTable.Store(ls, &wasm3ArrayAccessorMeta{
		elemT:  elemT,
		leaf:   leafClass,
		isSet:  isSet,
		signed: elemT.IsSigned(),
	})
	wasm3ArrayAccessorCache.Store(cacheKey, ls)
	return ls
}

// wasm3SetupPolyAccessor is the obj.Wasm3SetupPolyAccessor hook. It
// fires from assemble3 at the start of every wasm3 function body
// assembly. For a pending accessor (one whose LSym is in
// wasm3ArrayAccessorMetaTable) it (a) registers the (ref (array T))
// backing in s.Func()'s wasmgc.Table — the body's R_WASMTYPE relocs
// reference its slot — and (b) installs the Wasm3PolyAccessor on the
// FuncInfo so the next assemble3 check dispatches to
// assembleWasm3PolyAccessor. No-op for any other symbol.
//
// It also handles iface-dispatch trampolines (phase D step 5): an
// LSym registered in wasm3IfaceDispatchMetaTable gets its
// $go.iface / $go.vtab.<I> / method-funcref types registered in
// the trampoline's own collector and a Wasm3IfaceDispatch installed
// on its FuncInfo so assemble3 routes to assembleWasm3IfaceDispatch.
func wasm3SetupPolyAccessor(s *obj.LSym) {
	if mAny, ok := wasm3ArrayAccessorMetaTable.Load(s); ok {
		m := mAny.(*wasm3ArrayAccessorMeta)
		fi := s.Func()
		if fi == nil {
			// Accessor's compile pass hasn't created the FuncInfo yet.
			// Shouldn't happen — assemble3 runs after Compile, which
			// creates FuncInfo unconditionally. Defensive only.
			return
		}
		arrayIdx := wasm3RegisterArrayBacking(fi, m.elemT)
		elemSize := int(m.elemT.Size())
		fi.Wasm3PolyAccessor = &obj.Wasm3PolyAccessor{
			Set:          m.isSet,
			LeafClass:    m.leaf,
			ElemSize:     elemSize,
			Signed:       m.signed,
			ArrayTypeIdx: int64(arrayIdx),
		}
		return
	}
	if mAny, ok := wasm3IfaceDispatchMetaTable.Load(s); ok {
		m := mAny.(*wasm3IfaceDispatchMeta)
		fi := s.Func()
		if fi == nil {
			return
		}
		// Register $go.vtab.<I> and the legacy $go.iface prelude type
		// in this trampoline's own collector — the body emits
		// R_WASMTYPE / R_WASMHEAPTYPE relocs referencing these slots,
		// and the linker maps per-function indices to module-global
		// indices via WasmType.Table. Registering the vtab also
		// transitively registers each method's anyref-recv funcref
		// type via collectIfaceMethodFuncSig (so the vtab's field
		// types resolve), and the trampoline's call_ref reuses the
		// same funcref-type index for its own method via
		// wasm3RegisterIfaceMethodFuncSigFor.
		wasm3EnsureCollector(fi)
		vtabIdx := wasm3RegisterIfaceVtab(fi, m.I)
		methods := m.I.AllMethods()
		if m.methodIdx < 0 || m.methodIdx >= len(methods) {
			return
		}
		funcIdx := wasm3RegisterIfaceMethodFuncSigFor(fi, methods[m.methodIdx].Type)
		fi.Wasm3IfaceDispatch = &obj.Wasm3IfaceDispatch{
			IfaceTypeIdx: int64(wasmgc.TypeGoIface),
			VtabTypeIdx:  int64(vtabIdx),
			FuncTypeIdx:  int64(funcIdx),
			MethodSlot:   int64(1 + m.methodIdx),
		}
		return
	}
}

// wasm3IfaceDispatchMeta is the side-table entry for a pending iface-
// dispatch trampoline. wasm3SetupPolyAccessor uses it to register the
// vtab and method-funcref types in the trampoline's own collector and
// install the Wasm3IfaceDispatch metadata on its FuncInfo.
type wasm3IfaceDispatchMeta struct {
	I         *types.Type // the interface type the dispatch targets
	methodIdx int         // position in I.AllMethods() of the selected method
}

// wasm3IfaceDispatchCache memoises trampoline LSyms keyed on
// "<I.LinkString>#<methodIdx>" so every OCALLINTER site for a given
// (I, method) shares one trampoline. Serialization of the DeclFunc
// path is via the shared reflectdata.Wasm3AccessorMu (also used by
// wasm3GetOrGenArrayAccessor) — ir.CurFunc and the funcStack are
// process-globals.
var wasm3IfaceDispatchCache sync.Map // string -> *obj.LSym

// wasm3IfaceDispatchMetaTable maps each generated trampoline LSym to
// the meta wasm3SetupPolyAccessor uses to lazily wire it.
var wasm3IfaceDispatchMetaTable sync.Map // *obj.LSym -> *wasm3IfaceDispatchMeta

// wasm3GetOrGenIfaceDispatchTrampoline returns the obj.LSym of a
// per-(I, methodIdx) interface-method dispatch trampoline used by the
// wasm3 lowering of OCALLINTER. Phase D step 5 — see
// doc/wasm3-iface-poly-derisk.wat and the Wasm3IfaceDispatch
// docstring in obj/link.go for the body shape.
//
// The trampoline's Go signature mirrors the interface method's, with
// the interface itself as receiver: func(I, args...) results. The
// trivial Go body (zero-value return) is thrown away at assemble
// time; assembleWasm3IfaceDispatch hand-encodes the wasm body.
//
// FuncInfo wiring (vtab/funcref type registration, install
// Wasm3IfaceDispatch) is deferred to wasm3SetupPolyAccessor — the
// trampoline's FuncInfo isn't created until its own compile pass.
func wasm3GetOrGenIfaceDispatchTrampoline(I *types.Type, methodIdx int) *obj.LSym {
	cacheKey := I.LinkString() + "#" + strconv.Itoa(methodIdx)
	if cached, ok := wasm3IfaceDispatchCache.Load(cacheKey); ok {
		return cached.(*obj.LSym)
	}
	reflectdata.Wasm3AccessorMu.Lock()
	defer reflectdata.Wasm3AccessorMu.Unlock()
	if cached, ok := wasm3IfaceDispatchCache.Load(cacheKey); ok {
		return cached.(*obj.LSym)
	}

	methods := I.AllMethods()
	if methodIdx < 0 || methodIdx >= len(methods) {
		base.Fatalf("wasm3GetOrGenIfaceDispatchTrampoline: methodIdx %d out of range for %v", methodIdx, I)
	}
	method := methods[methodIdx]
	mft := method.Type
	if mft == nil || mft.Kind() != types.TFUNC {
		base.Fatalf("wasm3GetOrGenIfaceDispatchTrampoline: method %v has no func type", method)
	}

	// Build the trampoline's Go signature: receiver = I, plus the
	// method's own params, returning the method's own results. Use
	// the method type's Params/Results directly — they already
	// exclude the abstract receiver slot.
	pos := base.AutogeneratedPos
	base.Pos = pos
	var params []*types.Field
	params = append(params, types.NewField(pos, typecheck.Lookup("iface"), I))
	for i, p := range mft.Params() {
		name := typecheck.Lookup("a" + strconv.Itoa(i))
		params = append(params, types.NewField(pos, name, p.Type))
	}
	var results []*types.Field
	for _, r := range mft.Results() {
		results = append(results, types.NewField(pos, nil, r.Type))
	}

	// Symbol name: ".ifacedispatch.w3.<I-linkstring>#<methodIdx>". The
	// leading dot keeps it out of the user namespace and matches the
	// array-accessor convention.
	sym := types.TypeSymLookup(".ifacedispatch.w3." + cacheKey)
	if sym.Def != nil {
		ls := sym.Def.(*ir.Name).Func.Linksym()
		wasm3IfaceDispatchCache.Store(cacheKey, ls)
		return ls
	}
	fn := ir.NewFunc(pos, pos, sym, types.NewSignature(nil, params, results))
	sym.Def = fn.Nname
	fn.Pragma |= ir.Noinline
	typecheck.DeclFunc(fn)
	// Trivial body — assemble3's Wasm3IfaceDispatch dispatch replaces
	// the actual emission, so the SSA-compiled output is thrown away.
	// Same NON-EMPTY-body invariant as wasm3GetOrGenArrayAccessor: an
	// empty body means enqueueFunc skips compilation and assemble3
	// never runs. Use zero-value returns for every result.
	if len(results) == 0 {
		fn.Body.Append(ir.NewReturnStmt(pos, nil))
	} else {
		zs := make([]ir.Node, 0, len(results))
		for _, r := range results {
			zs = append(zs, wasm3ZeroValueForReturn(pos, r.Type))
		}
		fn.Body.Append(ir.NewReturnStmt(pos, zs))
	}
	typecheck.FinishFuncBody()
	fn.SetDupok(true)
	ir.WithFunc(fn, func() { typecheck.Stmts(fn.Body) })
	fn.SetNilCheckDisabled(true)

	ls := fn.Linksym()
	wasm3IfaceDispatchMetaTable.Store(ls, &wasm3IfaceDispatchMeta{
		I:         I,
		methodIdx: methodIdx,
	})
	wasm3IfaceDispatchCache.Store(cacheKey, ls)
	return ls
}

// wasm3ZeroValueForReturn returns an ir.Node holding the zero value
// of t for use in the synthesised trampoline body's return statement.
// The body is thrown away at assemble time, so it only has to type-
// check and trip enqueueFunc into compiling the function. nil for
// reference-shaped Go kinds (pointer/interface/slice/string/map/chan/
// func), 0 for integers/booleans, 0.0 for floats; composite types
// (struct/array) get a composite literal of zero leaves.
func wasm3ZeroValueForReturn(pos src.XPos, t *types.Type) ir.Node {
	switch {
	case t.IsFloat():
		return ir.NewBasicLit(pos, t, constant.MakeFloat64(0))
	case t.IsString():
		return ir.NewBasicLit(pos, t, constant.MakeString(""))
	case t.IsBoolean():
		return ir.NewBasicLit(pos, t, constant.MakeBool(false))
	case t.IsInteger():
		return ir.NewInt(pos, 0)
	case t.IsPtr() || t.IsUnsafePtr() || t.IsInterface() || t.IsSlice() ||
		t.IsMap() || t.IsChan() || t.Kind() == types.TFUNC:
		return typecheck.NodNil()
	}
	// Composite (struct, array): a typed nil-ish composite literal of
	// the type. Use NewCompLitExpr with no fields — the typechecker
	// expands it to a zero value.
	cl := ir.NewCompLitExpr(pos, ir.OCOMPLIT, t, nil)
	return cl
}

// wasm3PtrField emits the read of field `field` of the
// $go.ptr.poly.<class> polymorphic fat pointer (type index ptrIdx)
// stashed in anyref local `tmp`: local.get tmp; ref.cast (ref
// $go.ptr.poly.<class>); struct.get $go.ptr.poly.<class> field.
// Used by the interior-pointer deref codegen (PtrLoad/PtrStore).
// All four classes share the {class, base, off} layout; only the
// type index varies. Pass field=0 for class, field=1 for base,
// field=2 for off.
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

// wasm3AllocI32TempLocal appends a fresh i32 scratch local for ops
// that need an i32-typed cursor / accumulator (e.g. M3.5 linear-memory
// bridge byte loops). Mirrors wasm3AllocAnyrefTempLocal.
func wasm3AllocI32TempLocal(s *ssagen.State) uint32 {
	fi := s.FuncInfo()
	if fi == nil {
		panic("wasm3AllocI32TempLocal: no FuncInfo")
	}
	idx := uint32(len(fi.Wasm3LocalTypes))
	fi.Wasm3LocalTypes = append(fi.Wasm3LocalTypes, 0x7F) // i32
	base := uint32(0)
	if wt := fi.WasmType; wt != nil {
		base = uint32(len(wt.Params))
	}
	return base + idx
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
