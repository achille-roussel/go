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
	"fmt"
	"math"
	"os"
	"sort"
	"sync"
)

// wasm3GlobalIndex returns the wasm3 module's global index for a Go
// "register" that is implemented as a wasm global rather than a
// per-function local. The wasm3 linker emits three fixed globals
// plus per-singleton globals (see cmd/link/internal/wasm/asm3.go
// writeGlobalSec3):
//
//	0: i32 — linear-memory bump-allocator pointer / SP.
//	1: i64 — CTXT, used by legacy closure-with-captures calls to
//	   pass the captures pointer from the call site to the closure
//	   body. Targeted for retirement once captures-in-struct lands
//	   for every closure path (see doc/wasm3-m3-captures-in-struct.md).
//	2: anyref — CTXT_REF, used by captures-in-struct closure bodies
//	   to recover their concrete closureCtx ref from the indirect-
//	   call site. Read via wasm3GlobalIndexCtxRef in compile-side
//	   codegen (no REG_* mapping; bodies emit AGlobalGet directly
//	   from the OpWasm3LoweredGetClosureRef SSA op).
//
// REG_SP is excluded here because SP-relative AGet needs offset
// arithmetic (global.get 0 → i64.extend → +offset), not a plain
// global.get; that's handled by a dedicated case in the AGet
// encoder.
func wasm3GlobalIndex(reg int16) (uint64, bool) {
	switch reg {
	case REG_CTXT:
		return 1, true
	}
	return 0, false
}

// Wasm3GlobalIndexLinearBumpPtr is the index of the i32 mutable
// module global used as the bump pointer for the M3.5 linear-memory
// bridge (runtime/wasm.{Write,Read,Reset}LinearMemory). The linker
// reserves this slot in writeGlobalSec3 (originally an M2-era
// placeholder; repurposed here).
const Wasm3GlobalIndexLinearBumpPtr = 0

// Wasm3TagIndexPark is the wasm tag index of the M4 "goroutine park"
// tag declared by cmd/link/internal/wasm/asm3.go writeTagSec3. Every
// suspend that yields back to the cont-running resume references this
// index. The tag is currently typed (param) — no payload.
const Wasm3TagIndexPark = 0

// Wasm3GlobalIndexCtxRef is the index of the CTXT_REF anyref module
// global (see writeGlobalSec3). Exposed so the compile-side codegen
// for OpWasm3LoweredGetClosureRef can emit `global.get 2` without
// hard-coding the literal.
const Wasm3GlobalIndexCtxRef = 2

// Wasm3ClosureBodyInfo carries, for a single closure body, the
// info wasm3 codegen needs to register the per-closure closureCtx
// type in this function's *obj.FuncInfo typeCollector (see
// doc/wasm3-m3-captures-in-struct.md). Stashed by ssagen at SSA
// build, read by wasm3 codegen.
//
// Both fields are typed `any` to keep this obj-leaf package free
// of cmd/compile/internal/types; cmd/compile/internal/wasm3 type-
// asserts them back to *types.Type / []*types.Type on read.
type Wasm3ClosureBodyInfo struct {
	FuncType any // *cmd/compile/internal/types.Type, the closure body's func signature
	Captures any // []*cmd/compile/internal/types.Type, captures in ClosureVars order
}

// Wasm3ClosureBodyCaptures is a side-channel published by
// cmd/compile/internal/ssagen and consumed by
// cmd/compile/internal/wasm3 to communicate, per closure body LSym,
// the info the body's GetClosureField / LoweredCastClosureRef
// codegen needs to register the per-closure closureCtx in *its
// own* FuncInfo typeCollector. The caller's
// OpWasm3MakeClosureRefInline registration lives in a *different*
// FuncInfo's collector and is invisible across the boundary.
//
// Why a side channel rather than putting captures on the SSA op
// directly: ssagen has *ir.Func.ClosureVars at SSA build, but
// cannot import cmd/compile/internal/wasm3 (wasm3 imports ssagen).
// This map, hosted in cmd/internal/obj/wasm (a leaf package both
// sides already import), bridges the gap.
//
// Keyed on the closure body's *obj.LSym (= fn.LSym at SSA build);
// concurrent compilation safe via sync.Map.
var Wasm3ClosureBodyCaptures sync.Map

// Wasm3StructuredPlan is the relooper output the compiler-side
// publishes to the obj-side encoder. When present for a given LSym,
// encodeWasm3Body bypasses the legacy wasm3AnalyzeCFG dispatch-mode
// fallback and emits structured wasm control flow (block / loop / if
// / br) per the plan.
//
// Construction: cmd/compile/internal/wasm3 builds the plan from the
// SSA function's dominators + natural loops. Lookup: the obj-encoder
// reads Wasm3EmitPlans at the start of encodeWasm3Body.
type Wasm3StructuredPlan struct {
	// PerBoundary[i] is the scope op applied at boundary i, for i in
	// [0, NumBlocks]. Boundary 0 is at function start (before the
	// first prog of block Layout[0]); boundary NumBlocks is at
	// function end (after the last prog of block Layout[NumBlocks-1]).
	PerBoundary []Wasm3BoundaryOp

	// Layout[i] is the block ID of the i-th block in layout order.
	// Used to recover the source block ID from the obj-encoder's
	// boundary counter, and target block IDs from boundaryOfPc.
	Layout []int32
}

// Wasm3BoundaryOp is the scope manipulation applied at one boundary.
// Closes are applied first (popping the stack); Opens are applied
// next, pushed in order onto the stack.
type Wasm3BoundaryOp struct {
	Closes int
	Opens  []Wasm3ScopeOpen
}

// Wasm3ScopeOpen describes one wasm scope being pushed onto the
// stack at a boundary. Kind is 0 for a `loop` scope (back-edge
// target) and 1 for a `block` scope (forward-merge target). Target
// is the block ID the scope corresponds to: the loop header for a
// loop scope, or the block ID immediately after the scope's `end`
// for a block scope.
type Wasm3ScopeOpen struct {
	Kind   uint8
	Target int32
}

const (
	Wasm3ScopeKindLoop  uint8 = 0
	Wasm3ScopeKindBlock uint8 = 1
)

// Wasm3EmitPlans is the registry of relooper plans, keyed by the
// function's LSym. cmd/compile/internal/wasm3 populates it once per
// function; encodeWasm3Body looks it up at the start of encoding.
var Wasm3EmitPlans sync.Map // *obj.LSym -> *Wasm3StructuredPlan

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
// The wasip1 entry symbol (runtime._rt0_wasm3_wasip1) is a plain Go
// function defined in src/runtime/rt0_wasip1_wasm3.go and assembled
// via the same encodeWasm3Body path as every other Go function — no
// obj-level substitution.
func assemble3(ctxt *obj.Link, s *obj.LSym, newprog obj.ProgAlloc) {
	if we := s.Func().WasmExport; we != nil && we.WrappedSym != nil {
		assembleWasm3ExportWrapper(ctxt, s, we)
		return
	}

	if wi := s.Func().WasmImport; wi != nil {
		assembleWasm3ImportWrapper(ctxt, s, wi)
		return
	}

	if body, ok := encodeWasm3Body(ctxt, s); ok {
		s.P = body
		return
	}

	// Debug: report which function falls back to stub. Remove after
	// the wasm3 op coverage is complete.
	if os.Getenv("WASM3STUBDBG") != "" {
		fmt.Fprintf(os.Stderr, "wasm3: stubbing function %s\n", s.Name)
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
// access (Get/Set/Tee of a register), integer and floating constants,
// direct calls, the operand-less arithmetic/comparison/conversion
// opcodes, and RET. A function that touches the linear-memory frame
// (Get SP, loads, stores) or makes an indirect call still falls back
// to the unreachable stub; those are later rungs.
//
// Stage C.2c — the branches rung — adds structured control flow.
// Wasm requires nested block/loop/if/end with `br N` jumping out of
// N enclosing levels; the SSA backend instead emits the two-headed
// AIf/AEnd pattern and arbitrary AJMPs to ARESUMEPOINT-delimited
// basic-block boundaries. wasm3CFG analyses the prog stream, opens
// one wasm `block` per distinct forward-edge AJMP target wrapping the
// body so each AJMP can be lowered to a `br <depth>` exiting the
// right enclosing block, and bails on back-edges (loops are the next
// sub-rung — they need an enclosing `loop` plus a back-edge `br`).
//
// The body opens with a prologue that copies each sub-word integer
// parameter from its i32 parameter local into the i64 SSA-register
// local the body uses (widening with i64.extend_i32_u); see
// wasm3Locals for why a narrow parameter cannot simply alias its
// register.
func encodeWasm3Body(ctxt *obj.Link, s *obj.LSym) (body []byte, ok bool) {
	localOf, spillOf, decls, prologue, nextLocal, ok := wasm3Locals(s)
	if !ok {
		return nil, false
	}
	cfg, ok := wasm3AnalyzeCFG(s)
	if !ok {
		return nil, false
	}

	// Relooper-plan integration. If the compiler-side has registered
	// a structured plan for this LSym, drive scope ops from the plan
	// instead of the legacy dispatch / forward-only schemes. The
	// plan covers loops natively, so dispatch mode is disabled and
	// the forward-only target list is cleared — both responsibilities
	// transfer to perBoundary scope ops applied at function start
	// and at each ARESUMEPOINT.
	var structuredPlan *Wasm3StructuredPlan
	if v, ok := Wasm3EmitPlans.Load(s); ok {
		structuredPlan = v.(*Wasm3StructuredPlan)
		cfg.dispatch = false
		cfg.forwardTargets = nil
	}

	// Dispatch mode needs a fresh i32 local to hold the next basic-block
	// index. Allocate it after every other declared local so its index
	// is stable as we walk the body.
	var dispatchLocal uint64
	if cfg.dispatch {
		dispatchLocal = nextLocal
		decls = append(decls, wasm3LocalDecl{count: 1, typ: 0x7F /* i32 */})
	}

	// Run-length encode the locals vector: consecutive entries
	// with the same type collapse into a single (count, type) pair.
	// wasm3Locals emits one wasm3LocalDecl per local; the SSA-side
	// wasm3PlaceValues output is largely all-i64 runs, so the
	// grouping saves several bytes per function (a 20-local run
	// drops from 40 declaration bytes to 3).
	groups := decls[:0:0]
	for _, d := range decls {
		if n := len(groups); n > 0 && groups[n-1].typ == d.typ {
			groups[n-1].count += d.count
			continue
		}
		groups = append(groups, d)
	}
	w := new(bytes.Buffer)
	writeUleb128(w, uint64(len(groups)))
	for _, d := range groups {
		writeUleb128(w, d.count)
		w.WriteByte(d.typ)
	}
	for _, c := range prologue {
		writeOpcode(w, ALocalGet)
		writeUleb128(w, c.paramLocal)
		writeOpcode(w, AI64ExtendI32U)
		writeOpcode(w, ALocalSet)
		writeUleb128(w, c.regLocal)
	}

	// Open the wrapping control structures.
	//
	// Forward-only mode: one wasm `block` per distinct forward-edge
	// target boundary, outermost first (largest target boundary) so the
	// innermost block ends at the smallest target. Each AJMP locates
	// its target's frame and emits `br <depth>`.
	//
	// Dispatch mode: `loop $L` plus N nested `block`s ending right
	// before each basic block, with a `br_table` at the top of $L
	// dispatching on the dispatch local. Initialize the dispatch local
	// to 0 (function entry → BB_0) before entering the loop. After the
	// loop closes, `unreachable` traps if execution falls past the
	// dispatch — every basic block is expected to either `return` or
	// AJMP back into the loop, never to fall off the bottom.
	//
	// The frame stack records the open structures innermost-first so
	// the depth lookup for `br <depth>` is a linear scan from index 0.
	// Each block/if/loop has empty result type (0x40) — values flow
	// between basic blocks via the register/spill locals, never via the
	// wasm operand stack.
	stack := make([]wasm3CFFrame, 0, len(cfg.forwardTargets)+cfg.numBlocks+4)
	if cfg.dispatch {
		// dispatch = 0
		writeOpcode(w, AI32Const)
		writeSleb128(w, 0)
		writeOpcode(w, ALocalSet)
		writeUleb128(w, dispatchLocal)
		// loop $L
		writeOpcode(w, ALoop)
		w.WriteByte(0x40)
		stack = append(stack, wasm3CFFrame{kind: wasm3CFLoop})
		// One nested `block` per basic block, outermost first.
		// stack ends as [b_0, b_1, ..., b_{N-1}, loop] (innermost first).
		for i := cfg.numBlocks - 1; i >= 0; i-- {
			writeOpcode(w, ABlock)
			w.WriteByte(0x40)
			stack = append([]wasm3CFFrame{{kind: wasm3CFDispatch, boundary: i}}, stack...)
		}
		// br_table at the top of the dispatch nest.
		writeOpcode(w, ALocalGet)
		writeUleb128(w, dispatchLocal)
		writeOpcode(w, ABrTable)
		// Vector length: numBlocks targets, last entry is the default.
		// We emit numBlocks entries (the default = numBlocks - 1, i.e.
		// the outermost dispatch block, which is harmless because
		// dispatch is always set to a valid boundary).
		writeUleb128(w, uint64(cfg.numBlocks-1))
		for i := 0; i < cfg.numBlocks; i++ {
			// Depth from current position to block-for-BB_i. The stack
			// (innermost first) is [b_0, b_1, ..., b_{N-1}, loop].
			// b_i is at depth i.
			writeUleb128(w, uint64(i))
		}
	} else {
		for i := len(cfg.forwardTargets) - 1; i >= 0; i-- {
			writeOpcode(w, ABlock)
			w.WriteByte(0x40)
			stack = append([]wasm3CFFrame{{kind: wasm3CFBlock, boundary: cfg.forwardTargets[i]}}, stack...)
		}
	}

	// In dispatch mode the innermost dispatch block ($b_0) is closed
	// immediately after the br_table — its `end` lands at start of BB_0,
	// the natural fall-through position when dispatch == 0. Subsequent
	// dispatch blocks are closed at each ARESUMEPOINT boundary by the
	// walker below.
	if cfg.dispatch && len(stack) > 0 && stack[0].kind == wasm3CFDispatch && stack[0].boundary == 0 {
		writeOpcode(w, AEnd)
		stack = stack[1:]
	}

	// Plan-mode helper: apply the relooper's per-boundary scope ops
	// (closes innermost-first, then opens outermost-first). Closes
	// pop plan frames off the front of the stack; opens push new
	// plan frames onto the front (innermost-first invariant).
	// Returns ok=false on a stack mismatch (typically a planner bug).
	applyPlanBoundary := func(boundaryIdx int) bool {
		if structuredPlan == nil || boundaryIdx >= len(structuredPlan.PerBoundary) {
			return true
		}
		bop := &structuredPlan.PerBoundary[boundaryIdx]
		for i := 0; i < bop.Closes; i++ {
			if len(stack) == 0 {
				return false
			}
			top := stack[0]
			if top.kind != wasm3CFPlanLoop && top.kind != wasm3CFPlanBlock {
				// AIf or legacy scope on top — closing a plan scope
				// past it would be malformed nesting.
				return false
			}
			writeOpcode(w, AEnd)
			stack = stack[1:]
		}
		for _, op := range bop.Opens {
			var kind wasm3CFKind
			var as obj.As
			switch op.Kind {
			case Wasm3ScopeKindLoop:
				kind = wasm3CFPlanLoop
				as = ALoop
			case Wasm3ScopeKindBlock:
				kind = wasm3CFPlanBlock
				as = ABlock
			default:
				return false
			}
			writeOpcode(w, as)
			w.WriteByte(0x40)
			stack = append([]wasm3CFFrame{{kind: kind, target: op.Target}}, stack...)
		}
		return true
	}

	// Plan-mode initial scope opens (boundary 0).
	if structuredPlan != nil && !applyPlanBoundary(0) {
		return nil, false
	}

	var relocs []obj.Reloc
	sawRet := false
	seenBoundary := 0
	nextTargetIdx := 0
	var lastP *obj.Prog
	defer func() {
		if !ok && os.Getenv("WASM3STUBDBG") != "" {
			if lastP != nil {
				fmt.Fprintf(os.Stderr, "wasm3: bail in %s at op %s (from=%+v to=%+v)\n", s.Name, lastP.As, lastP.From, lastP.To)
			} else {
				fmt.Fprintf(os.Stderr, "wasm3: bail in %s (before body loop)\n", s.Name)
			}
		}
	}()
	for p := s.Func().Text; p != nil; p = p.Link {
		lastP = p
		switch p.As {
		case obj.ATEXT, obj.AFUNCDATA, obj.APCDATA, obj.ANOP, ANop:
			// No body contribution: ATEXT carries the signature,
			// FUNCDATA/PCDATA carry metadata.

		case ARESUMEPOINT:
			// A basic-block boundary. In forward-only mode, close the
			// wrapping block opened for this boundary (if any). In
			// dispatch mode, close the next dispatch block (b_K), which
			// lands execution at start of BB_K. The very last boundary
			// (== cfg.numBlocks) has no dispatch block to close — the
			// outer loop close happens after the walker finishes.
			// In plan mode, apply the relooper's per-boundary closes
			// and opens.
			seenBoundary++
			if structuredPlan != nil {
				if !applyPlanBoundary(seenBoundary) {
					return nil, false
				}
			} else if cfg.dispatch {
				if seenBoundary < cfg.numBlocks {
					if len(stack) == 0 || stack[0].kind != wasm3CFDispatch || stack[0].boundary != seenBoundary {
						return nil, false
					}
					writeOpcode(w, AEnd)
					stack = stack[1:]
				}
			} else if nextTargetIdx < len(cfg.forwardTargets) && cfg.forwardTargets[nextTargetIdx] == seenBoundary {
				if len(stack) == 0 || stack[0].kind != wasm3CFBlock || stack[0].boundary != seenBoundary {
					return nil, false // stack mismatch (shouldn't happen)
				}
				writeOpcode(w, AEnd)
				stack = stack[1:]
				nextTargetIdx++
			}

		case obj.AJMP:
			tBoundary, isJmp := cfg.jmpTarget[p]
			if !isJmp {
				return nil, false
			}
			if structuredPlan != nil {
				// Look up target block ID via the plan's layout.
				if tBoundary < 0 || tBoundary >= len(structuredPlan.Layout) {
					return nil, false
				}
				targetBlockID := structuredPlan.Layout[tBoundary]
				// Walk the stack innermost-first; depth = position of
				// the matching plan scope (whose target equals the
				// target block ID). AIf frames count toward depth.
				depth := -1
				for i, f := range stack {
					if (f.kind == wasm3CFPlanLoop || f.kind == wasm3CFPlanBlock) &&
						f.target == targetBlockID {
						depth = i
						break
					}
				}
				if depth < 0 {
					return nil, false
				}
				writeOpcode(w, ABr)
				writeUleb128(w, uint64(depth))
				break
			}
			if cfg.dispatch {
				// Set the dispatch local to the target boundary, then
				// br to the loop label so the next iteration of $L's
				// br_table lands at start of BB_{tBoundary}.
				loopDepth := -1
				for i, f := range stack {
					if f.kind == wasm3CFLoop {
						loopDepth = i
						break
					}
				}
				if loopDepth < 0 {
					return nil, false
				}
				writeOpcode(w, AI32Const)
				writeSleb128(w, int64(tBoundary))
				writeOpcode(w, ALocalSet)
				writeUleb128(w, dispatchLocal)
				writeOpcode(w, ABr)
				writeUleb128(w, uint64(loopDepth))
			} else {
				depth := -1
				for i, f := range stack {
					if f.kind == wasm3CFBlock && f.boundary == tBoundary {
						depth = i
						break
					}
				}
				if depth < 0 {
					return nil, false
				}
				writeOpcode(w, ABr)
				writeUleb128(w, uint64(depth))
			}

		case AIf:
			writeOpcode(w, AIf)
			w.WriteByte(0x40)
			stack = append([]wasm3CFFrame{{kind: wasm3CFIf}}, stack...)

		case AElse:
			writeOpcode(w, AElse)

		case AEnd:
			// AEnd terminates either an SSA-emitted AIf, or an SSA
			// op's inline ABlock/ALoop pair (wasm3CFInline). Other
			// frames (CFBlock/CFLoop/CFDispatch/CFPlanBlock/CFPlanLoop)
			// are closed by the planner / boundary machinery above,
			// not by a raw AEnd in the op stream.
			if len(stack) == 0 || (stack[0].kind != wasm3CFIf && stack[0].kind != wasm3CFInline) {
				return nil, false
			}
			writeOpcode(w, AEnd)
			stack = stack[1:]

		case obj.ARET:
			writeOpcode(w, AReturn)
			sawRet = true

		case obj.AUNDEF:
			writeOpcode(w, AUnreachable)

		case AGet:
			// Get of a symbol address (NAME_EXTERN) lowers to
			// `i64.const <addr>` with an R_ADDR relocation; the linker
			// resolves the variable-length operand. Used by package-
			// level globals, where the SSA backend emits
			// `Get $sym; i32.wrap_i64; <load> $offset` to read the
			// value at the global's linear-memory address.
			if p.From.Type == obj.TYPE_ADDR && p.From.Name == obj.NAME_EXTERN {
				writeOpcode(w, AI64Const)
				relocs = append(relocs, obj.Reloc{
					Type: objabi.R_ADDR,
					Off:  int32(w.Len()),
					Siz:  1, // variable-sized; the linker writes the address
					Sym:  p.From.Sym,
					Add:  p.From.Offset,
				})
				break
			}
			if gidx, ok := wasm3GlobalIndex(p.From.Reg); ok {
				writeOpcode(w, AGlobalGet)
				writeUleb128(w, gidx)
				break
			}
			idx, isLocal := localOf[p.From.Reg]
			if !isLocal {
				return nil, false
			}
			writeOpcode(w, ALocalGet)
			writeUleb128(w, idx)

		case ASet, ATee:
			if gidx, ok := wasm3GlobalIndex(p.To.Reg); ok {
				if p.As != ASet {
					return nil, false // global.tee doesn't exist
				}
				writeOpcode(w, AGlobalSet)
				writeUleb128(w, gidx)
				break
			}
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

		case ALocalGet:
			// M3 Phase 3: the real wasm `local.get` opcode, emitted
			// directly by ssaGenValue when it consults the per-value
			// local map (f.Wasm3ValueLocals + the per-function base
			// offset). The operand is a wasm local index, pre-
			// computed at SSA-gen time; no register-name translation.
			if p.From.Type != obj.TYPE_CONST {
				return nil, false
			}
			writeOpcode(w, ALocalGet)
			writeUleb128(w, uint64(p.From.Offset))

		case ALocalSet, ALocalTee:
			// M3 Phase 3 mirror of ALocalGet for the producer side.
			if p.To.Type != obj.TYPE_CONST {
				return nil, false
			}
			// Peephole: ALocalSet idx; ALocalGet idx -> ALocalTee idx.
			// The two-byte saving (one opcode + repeated leb128 index)
			// shows up most in Phi resolution and load-into-register
			// patterns. Only ALocalSet is rewritable; ALocalTee already
			// leaves the value on the stack.
			if p.As == ALocalSet && p.Link != nil && p.Link.As == ALocalGet &&
				p.Link.From.Type == obj.TYPE_CONST && p.Link.From.Offset == p.To.Offset {
				writeOpcode(w, ALocalTee)
				writeUleb128(w, uint64(p.To.Offset))
				p = p.Link // skip the consumed ALocalGet
				break
			}
			writeOpcode(w, p.As)
			writeUleb128(w, uint64(p.To.Offset))

		case AI32Store, AI64Store, AF32Store, AF64Store,
			AI32Store8, AI32Store16, AI64Store8, AI64Store16, AI64Store32:
			// Two flavours: a register spill (OpStoreReg) targets an
			// auto/param slot — encode as `local.set <spillLocal>` so
			// the wasm stack value lands in a wasm local; otherwise the
			// store is a real linear-memory write — the address comes
			// from the wasm stack (pushed by a prior `Get $sym`/load
			// chain), encode as `<store opcode> align offset`.
			if idx, isSpill := spillOf[wasm3SpillSlot(&p.To)]; isSpill {
				writeOpcode(w, ALocalSet)
				writeUleb128(w, idx)
				break
			}
			if p.To.Type != obj.TYPE_CONST || p.To.Offset < 0 {
				return nil, false
			}
			writeOpcode(w, p.As)
			writeUleb128(w, wasm3LoadStoreAlign(p.As))
			writeUleb128(w, uint64(p.To.Offset))

		case AI32Load, AI64Load, AF32Load, AF64Load,
			AI32Load8S, AI32Load8U, AI32Load16S, AI32Load16U,
			AI64Load8S, AI64Load8U, AI64Load16S, AI64Load16U,
			AI64Load32S, AI64Load32U:
			if idx, isSpill := spillOf[wasm3SpillSlot(&p.From)]; isSpill {
				writeOpcode(w, ALocalGet)
				writeUleb128(w, idx)
				break
			}
			if p.From.Type != obj.TYPE_CONST || p.From.Offset < 0 {
				return nil, false
			}
			writeOpcode(w, p.As)
			writeUleb128(w, wasm3LoadStoreAlign(p.As))
			writeUleb128(w, uint64(p.From.Offset))

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

		case obj.ACALL, ACALLNORESUME, ACall:
			// ACALLNORESUME is the wasm-specific "call without a resume
			// point" used to bracket runtime functions that must not
			// trigger goroutine switching. ACall is the lower-level
			// wasm-explicit call (used inside a structured block).
			// wasm3 has no resume points (no goroutine PC trampoline),
			// so all three lower to a plain `call`.
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

		case ACallIndirect:
			// Stage F: indirect call via the funcref table (table 0,
			// declared by writeTableSec3). The wasm encoding is
			//   0x11 <typeidx> <tableidx>
			// where typeidx is a wasm3-internal index into the
			// per-function wasmgc.Table (R_WASMTYPE-relocated by the
			// linker), and tableidx is 0. The compiler-side caller
			// puts the function-table index on the wasm operand
			// stack before this op.
			if p.From.Type != obj.TYPE_CONST {
				return nil, false
			}
			writeOpcode(w, ACallIndirect)
			relocs = append(relocs, obj.Reloc{
				Type: objabi.R_WASMTYPE,
				Off:  int32(w.Len()),
				Siz:  1,
				Add:  p.From.Offset,
			})
			writeUleb128(w, 0) // tableidx

		case ANot:
			writeOpcode(w, AI32Eqz)

		case AGlobalGet:
			// Two emitters call this:
			//
			// (a) Stage G singleton: global.get of a closure-
			//     singleton global. The compiler emits AGlobalGet
			//     via OpWasm3FuncValue codegen with From={TYPE_MEM,
			//     NAME_EXTERN, Sym=funcLSym, Offset=per-package
			//     closureCtx type index}. The linker allocates one
			//     wasm global per unique (Sym, global type index)
			//     pair (initialised via `struct.new $closureCtx
			//     (ref.func $sym) (i64.const 0)`) and patches in
			//     the resolved global index via
			//     R_WASMCLOSURESINGLETON.
			//
			// (b) Captures-in-struct CTXT_REF read: From=
			//     {TYPE_CONST, Offset=Wasm3GlobalIndexCtxRef}. No
			//     relocation; the global index is a literal that
			//     never moves at link time.
			if p.From.Type == obj.TYPE_CONST {
				writeOpcode(w, AGlobalGet)
				writeUleb128(w, uint64(p.From.Offset))
				break
			}
			if p.From.Type != obj.TYPE_MEM ||
				(p.From.Name != obj.NAME_EXTERN && p.From.Name != obj.NAME_STATIC) {
				return nil, false
			}
			writeOpcode(w, AGlobalGet)
			// (c) Boxed package-level variable read (GOARCH=wasm3): a
			//     global.get of the variable's wasm ref-global, marked
			//     Wasm3GlobalRef. The linker resolves R_WASMGLOBAL to the
			//     variable's allocated ref-global index.
			relocType := objabi.R_WASMCLOSURESINGLETON
			if p.Mark&Wasm3GlobalRef != 0 {
				relocType = objabi.R_WASMGLOBAL
			} else if p.Mark&Wasm3DescriptorRef != 0 {
				relocType = objabi.R_WASMDESCRIPTOR
			}
			relocs = append(relocs, obj.Reloc{
				Type: relocType,
				Off:  int32(w.Len()),
				Siz:  1, // variable-sized; the linker writes the global index
				Sym:  p.From.Sym,
				Add:  p.From.Offset,
			})

		case AGlobalSet:
			// (a) Captures-in-struct CTXT_REF write at indirect-call
			//     sites: From={TYPE_CONST, Offset=Wasm3GlobalIndexCtxRef}.
			//     Plain literal global index; no relocation.
			if p.From.Type == obj.TYPE_CONST {
				writeOpcode(w, AGlobalSet)
				writeUleb128(w, uint64(p.From.Offset))
				break
			}
			// (b) Boxed package-level variable write (GOARCH=wasm3):
			//     global.set of the variable's wasm ref-global, marked
			//     Wasm3GlobalRef; R_WASMGLOBAL carries the index.
			if p.From.Type != obj.TYPE_MEM ||
				(p.From.Name != obj.NAME_EXTERN && p.From.Name != obj.NAME_STATIC) ||
				p.Mark&Wasm3GlobalRef == 0 {
				return nil, false
			}
			writeOpcode(w, AGlobalSet)
			relocs = append(relocs, obj.Reloc{
				Type: objabi.R_WASMGLOBAL,
				Off:  int32(w.Len()),
				Siz:  1,
				Sym:  p.From.Sym,
				Add:  p.From.Offset,
			})

		default:
			// Specific operand-carrying ops (wasmgc struct.* / array.* /
			// ref.*) need their operand encoding handled BEFORE the
			// generic "operand-less bail" check. Try the inner switch
			// first; only ops that don't match it fall through to the
			// operand-less encoder, where having operands signals an
			// unhandled case.
			if p.As < AUnreachable {
				// Pseudo-op (ATEXT, AFUNCDATA, etc.) — those have
				// outer-switch cases earlier; if one reaches here it's
				// not encodable. The post-ALast range (exception
				// handling + GC opcodes) IS allowed: writeOpcode
				// handles the 0xFB / 0xD0+ / 0x08+0x0A+0x1F prefixes.
				if os.Getenv("WASM3STUBDBG") != "" {
					fmt.Fprintf(os.Stderr, "wasm3: bail at pseudo-op %s in %s\n", p.As, s.Name)
				}
				return nil, false
			}
			switch p.As {
			case ABlock, ALoop:
				// Inline structured-CF opener emitted by a single SSA
				// op's codegen. Two block-type shapes:
				//
				//   p.From = TYPE_NONE    -> void (block type 0x40)
				//   p.From = TYPE_CONST T -> single-result (ref null T)
				//     encoded as 0x63 (ref-null prefix) 0x62 (exact-heap
				//     prefix) <typeidx-R_WASMTYPE-reloc>
				//
				// The typed-result form is the M4 Phase 4 piece that
				// lets a resume's park-tag handler push the suspended
				// cont at the enclosing block's label without the block
				// needing a separate prelude func type — the enclosing
				// SSA op's codegen sets p.From.Offset to TypeGoCont and
				// the linker remaps it via the per-function wasmgc.Table
				// merge result.
				writeOpcode(w, p.As)
				switch p.From.Type {
				case obj.TYPE_NONE:
					w.WriteByte(0x40) // void block type
				case obj.TYPE_CONST:
					// (ref null \$T) = 0x63 <typeidx>. The exact-heap-
					// type prefix 0x62 was dropped to match wasmfxtime
					// (wasmtime 30 / wasmparser 1.223) — see the
					// typesec.go heaptype helper for the full rationale.
					w.WriteByte(0x63) // ref null prefix
					// The typeidx after 0x63 is in heap-type position,
					// so R_WASMHEAPTYPE (sleb33), not R_WASMTYPE.
					relocs = append(relocs, obj.Reloc{
						Type: objabi.R_WASMHEAPTYPE,
						Off:  int32(w.Len()),
						Siz:  1,
						Add:  p.From.Offset,
					})
				default:
					return nil, false
				}
				stack = append([]wasm3CFFrame{{kind: wasm3CFInline}}, stack...)
				continue

			case ABrIf:
				// Inline conditional branch — depth comes from p.From.Offset.
				// Used inside SSA-op inline loops; the SSA op author is
				// responsible for setting the depth consistent with the
				// frames it opened.
				if p.From.Type != obj.TYPE_CONST || p.From.Offset < 0 {
					return nil, false
				}
				writeOpcode(w, p.As)
				writeUleb128(w, uint64(p.From.Offset))
				continue

			case ABr:
				// Inline unconditional branch. Same shape as ABrIf — the
				// AJMP-driven path that earlier branches reach is in the
				// outer switch (case obj.AJMP); reaching this case means
				// the SSA op author emitted a literal ABr with an
				// explicit depth in p.From.Offset.
				if p.From.Type != obj.TYPE_CONST || p.From.Offset < 0 {
					return nil, false
				}
				writeOpcode(w, p.As)
				writeUleb128(w, uint64(p.From.Offset))
				continue

			case ABrTable, ACall, ACallIndirect:
				// AIf/AElse/AEnd handled above; ABrTable / ACall /
				// ACallIndirect are not currently emittable from SSA ops
				// at this rung.
				return nil, false

			case ARefFunc:
				// ref.func $funcidx — materialises a (ref $funcType)
				// value referencing the named function. The funcidx
				// is patched in at link time via R_WASMREFFUNC, which
				// resolves to the function's module-global index AND
				// marks the function as one that must appear in a
				// passive-declared element segment (wasm 3.0 requires
				// every ref.func target to be in the "declared
				// functions" set or validation fails with "undeclared
				// function reference").
				if p.From.Type != obj.TYPE_MEM ||
					(p.From.Name != obj.NAME_EXTERN && p.From.Name != obj.NAME_STATIC) {
					return nil, false
				}
				writeOpcode(w, p.As)
				relocs = append(relocs, obj.Reloc{
					Type: objabi.R_WASMREFFUNC,
					Off:  int32(w.Len()),
					Siz:  1, // variable-sized; the linker writes the function index
					Sym:  p.From.Sym,
				})
				continue

			case ACallRef, AReturnCallRef:
				// call_ref / return_call_ref $typeidx — invokes the
				// function reference on top of the operand stack
				// against the named function-type signature. The
				// typeidx immediate is a per-package wasmgc type index
				// in p.From.Offset, R_WASMTYPE-relocated to the
				// module-global typeidx by the linker (same shape as
				// StructNew / RefCast / ArrayGet).
				if p.From.Type != obj.TYPE_CONST {
					return nil, false
				}
				writeOpcode(w, p.As)
				relocs = append(relocs, obj.Reloc{
					Type: objabi.R_WASMTYPE,
					Off:  int32(w.Len()),
					Siz:  1, // variable-sized; the linker writes the type index
					Add:  p.From.Offset,
				})
				continue

			case AStructNew, AStructNewDefault:
				// 0xFB-prefixed GC opcodes with a single type-index
				// operand. The per-package type index lives in
				// p.From.Offset; the linker remaps it to a module-
				// global index via the per-function wasmgc.Table's
				// merge result, with the R_WASMTYPE reloc telling the
				// linker which leb128 slot to patch.
				if p.From.Type != obj.TYPE_CONST {
					return nil, false
				}
				writeOpcode(w, p.As)
				relocs = append(relocs, obj.Reloc{
					Type: objabi.R_WASMTYPE,
					Off:  int32(w.Len()),
					Siz:  1, // variable-sized; the linker writes the type index
					Add:  p.From.Offset,
				})
				continue

			case AStructGet, AStructGetS, AStructGetU, AStructSet:
				// struct.get / struct.set: two operands — a type
				// index (R_WASMTYPE-relocated from p.From.Offset)
				// followed by a field index immediate
				// (p.To.Offset). The wasm encoding is
				// 0xFB <subop> <typeidx leb128> <fieldidx leb128>.
				if p.From.Type != obj.TYPE_CONST || p.To.Type != obj.TYPE_CONST {
					return nil, false
				}
				writeOpcode(w, p.As)
				relocs = append(relocs, obj.Reloc{
					Type: objabi.R_WASMTYPE,
					Off:  int32(w.Len()),
					Siz:  1,
					Add:  p.From.Offset,
				})
				writeUleb128(w, uint64(p.To.Offset))
				continue

			case ARefCast, ARefCastNull, ARefTest:
				// ref.cast / ref.cast null / ref.test: heap-type
				// immediate. Plain (typeidx) heap type — no exact
				// prefix; see typesec.go heaptype helper for the full
				// rationale on dropping 0x62 to match wasmfxtime /
				// wasmparser 1.223. The wasmgc subtyping rule lets a
				// (ref (exact $T)) produced by struct.new / array.new
				// be downcast implicitly to (ref $T).
				//
				// Use R_WASMHEAPTYPE — the heap-type position is s33,
				// not uleb. For typeidx >= 64, uleb would write the
				// single byte 0x40 which a sleb-reading validator
				// decodes as -64 (the void block type), producing
				// "invalid heap type" at instantiation.
				if p.From.Type != obj.TYPE_CONST {
					return nil, false
				}
				writeOpcode(w, p.As)
				relocs = append(relocs, obj.Reloc{
					Type: objabi.R_WASMHEAPTYPE,
					Off:  int32(w.Len()),
					Siz:  1,
					Add:  p.From.Offset,
				})
				continue

			case ARefNull:
				// ref.null heaptype — single byte heap-type immediate
				// for abstract heap types, or a typed (typeidx) form
				// for typed refs. No exact-prefix; see typesec.go's
				// heaptype helper for rationale. Heap-type position is
				// s33, so use R_WASMHEAPTYPE.
				if p.From.Type != obj.TYPE_CONST {
					return nil, false
				}
				writeOpcode(w, p.As)
				relocs = append(relocs, obj.Reloc{
					Type: objabi.R_WASMHEAPTYPE,
					Off:  int32(w.Len()),
					Siz:  1,
					Add:  p.From.Offset,
				})
				continue

			case ARefNullAny:
				// ref.null any — abstract anyref null. Encoded as the
				// generic ref.null opcode (0xD0) followed by the
				// single-byte anyref heaptype (0x6E). No typeidx, no
				// relocation. Used for ConstNil of ref-typed Go values
				// (interface, slice, *T...) whose per-value local is
				// anyref (per wasm3ValueType's cutover rules).
				w.WriteByte(0xD0)
				w.WriteByte(0x6E)
				continue

			case AArrayNew, AArrayNewDefault, AArrayGet, AArrayGetS, AArrayGetU, AArraySet, AArrayFill:
				// 0xFB-prefixed array GC opcodes with a single type-
				// index operand. Stack on entry / exit varies per op
				// (array.new: elem+len->ref, array.get: ref+idx->val,
				// array.set: ref+idx+val->void, …) but the *encoding*
				// is identical — opcode + R_WASMTYPE-relocated leb128.
				if p.From.Type != obj.TYPE_CONST {
					return nil, false
				}
				writeOpcode(w, p.As)
				relocs = append(relocs, obj.Reloc{
					Type: objabi.R_WASMTYPE,
					Off:  int32(w.Len()),
					Siz:  1,
					Add:  p.From.Offset,
				})
				continue

			case AArrayLen:
				// array.len has no type-index immediate — it works
				// on any array reference. Just the opcode byte (and
				// the 0xFB prefix writeOpcode handles).
				writeOpcode(w, p.As)
				continue

			case ACurrentMemory, AGrowMemory:
				// memory.size (0x3F 0x00) / memory.grow (0x40 0x00).
				// Single-byte memory-index immediate (always 0 for
				// the wasm 1.0 single-memory model — the multi-memory
				// proposal allows higher indices, not used here).
				// Stack semantics: memory.size → pushes current
				// page count i32; memory.grow → pops delta-pages i32,
				// pushes previous size i32 or -1 on failure.
				writeOpcode(w, p.As)
				w.WriteByte(0x00)
				continue

			case AArrayCopy:
				// array.copy takes two type indices: dst-array type
				// and src-array type. p.From.Offset = dst typeidx,
				// p.To.Offset = src typeidx. Both R_WASMTYPE-relocated.
				if p.From.Type != obj.TYPE_CONST || p.To.Type != obj.TYPE_CONST {
					return nil, false
				}
				writeOpcode(w, p.As)
				relocs = append(relocs, obj.Reloc{
					Type: objabi.R_WASMTYPE,
					Off:  int32(w.Len()),
					Siz:  1,
					Add:  p.From.Offset,
				})
				relocs = append(relocs, obj.Reloc{
					Type: objabi.R_WASMTYPE,
					Off:  int32(w.Len()),
					Siz:  1,
					Add:  p.To.Offset,
				})
				continue

			case AArrayNewFixed:
				// array.new_fixed: 0xFB 0x08 <typeidx> <count>. Pops
				// `count` element values off the stack and builds a fixed-
				// size (array T) from them. p.From.Offset = array typeidx
				// (R_WASMTYPE-relocated, linker-inserted at the reloc off),
				// p.To.Offset = element count (a plain uleb128 immediate
				// written after, so the linker inserts the type index
				// between the opcode and the count). Used to construct a
				// constant (array i8) for a string literal at startup
				// without a data segment (doc/wasm3-design.md; the
				// globals/constants-in-wasmgc init path).
				if p.From.Type != obj.TYPE_CONST || p.To.Type != obj.TYPE_CONST || p.To.Offset < 0 {
					return nil, false
				}
				writeOpcode(w, p.As)
				relocs = append(relocs, obj.Reloc{
					Type: objabi.R_WASMTYPE,
					Off:  int32(w.Len()),
					Siz:  1,
					Add:  p.From.Offset,
				})
				writeUleb128(w, uint64(p.To.Offset))
				continue

			case ASuspend:
				// suspend $tagidx — single tagidx operand, encoded as
				// a ULEB128 immediate after the opcode byte. The wasm3
				// backend currently only emits the M4 park tag (idx 0,
				// see Wasm3TagIndexPark); the wasm-link side has only
				// one tag declared, so the literal byte is always 0x00.
				if p.From.Type != obj.TYPE_CONST {
					return nil, false
				}
				writeOpcode(w, p.As)
				writeUleb128(w, uint64(p.From.Offset))
				continue

			case AContNew:
				// cont.new $typeidx — single typeidx operand. The
				// per-package wasmgc type index is in p.From.Offset,
				// R_WASMTYPE-relocated to the module-global typeidx.
				if p.From.Type != obj.TYPE_CONST {
					return nil, false
				}
				writeOpcode(w, p.As)
				relocs = append(relocs, obj.Reloc{
					Type: objabi.R_WASMTYPE,
					Off:  int32(w.Len()),
					Siz:  1,
					Add:  p.From.Offset,
				})
				continue

			case AResume:
				// resume $typeidx <handler-vec>. Two operand shapes:
				//
				//   p.From = TYPE_CONST typeidx (R_WASMTYPE-relocated)
				//   p.To   = TYPE_CONST 0                    -> empty handler vec
				//   p.To   = TYPE_BRANCH branchTarget (depth) -> 1-handler vec
				//                                                {on $park <depth>}
				//
				// The 1-handler shape lets a single SSA op (the Phase 4
				// RunInContCatchSuspend / Resume primitive) install the
				// goroutine-park-tag handler that catches a wasm.Suspend
				// inside the enclosing block — without needing a multi-
				// operand prog shape that doesn't fit the cmd/internal/obj
				// model. Phase 5+ can extend p.RestArgs for multi-handler
				// shapes (e.g. exception handlers alongside the park tag).
				if p.From.Type != obj.TYPE_CONST {
					return nil, false
				}
				writeOpcode(w, p.As)
				relocs = append(relocs, obj.Reloc{
					Type: objabi.R_WASMTYPE,
					Off:  int32(w.Len()),
					Siz:  1,
					Add:  p.From.Offset,
				})
				switch p.To.Type {
				case obj.TYPE_CONST:
					// Empty handler vec.
					writeUleb128(w, uint64(p.To.Offset))
				case obj.TYPE_BRANCH:
					// One-handler vec: count=1, clause kind, tag=park,
					// label=depth.
					//
					// Clause kind:
					//   0x00 — (on $tag $label): handler receives
					//          [tag-payloads, cont] at $label.
					//   0x01 — (on $tag switch $label): handler receives
					//          [tag-payloads] only; the cont is consumed
					//          by an implicit cont.bind-or-switch.
					//
					// V8 25.9 / December 2025 only implements 0x01
					// (kHandlerSwitch); requesting 0x00 fails with
					// "handler generates 0 operands, target block returns
					// 1" at compile time (the cont is not actually pushed,
					// even though the spec says it should be). We pick the
					// engine-accepted shape — 0x01 — and document the
					// trade-off: the SSA op caller must NOT consume a
					// cont at the handler label, so RunInContCatchSuspend
					// drops the suspended cont implicitly via cont.bind
					// rather than via an explicit stack-drop.
					//
					// When V8 implements 0x00 properly, switch back to
					// the spec-accurate cont-pushed shape; the SSA caller
					// will then have access to the suspended cont.
					writeUleb128(w, 1)
					w.WriteByte(0x01) // (on $tag switch $label) — engine-compatible
					writeUleb128(w, uint64(Wasm3TagIndexPark))
					writeUleb128(w, uint64(p.To.Offset))
				default:
					return nil, false
				}
				continue
			}
			// Operand-less wasm stack instructions only. If an
			// operand-carrying op landed here, it's an unhandled
			// case — bail.
			if p.From.Type != obj.TYPE_NONE || p.To.Type != obj.TYPE_NONE {
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
	if structuredPlan != nil {
		// Apply the final boundary's closes; any remaining stack
		// entries (an AIf left open is the only legitimate case here,
		// but generally none) signal a planner bug.
		if !applyPlanBoundary(len(structuredPlan.PerBoundary) - 1) {
			return nil, false
		}
	}
	if cfg.dispatch {
		// Close the outer `loop $L` and emit `unreachable` to satisfy
		// the validator if execution somehow falls past the loop end
		// without an AJMP back into it (every basic block is expected
		// to either `return` or AJMP back to dispatch, never to fall
		// off the bottom).
		if len(stack) != 1 || stack[0].kind != wasm3CFLoop {
			return nil, false
		}
		writeOpcode(w, AEnd)
		stack = stack[1:]
		writeOpcode(w, AUnreachable)
	}
	if len(stack) != 0 {
		// Mismatched control-flow nesting (open block/if not closed).
		return nil, false
	}

	w.WriteByte(0x0b) // end

	for _, r := range relocs {
		s.AddRel(ctxt, r)
	}
	return w.Bytes(), true
}

// wasm3CFFrame is one entry of the structured-control-flow stack the
// body encoder maintains. It is one of: a wasm `block` opened to give
// an AJMP-target boundary a `br` label (forward-only mode), a `block`
// opened to wrap one basic block in the dispatch-loop scheme, the
// outer `loop` of the dispatch-loop scheme, or an `if` opened by the
// SSA backend's BlockIf lowering. The stack is innermost-first so the
// `br <depth>` lookup is a linear scan from index 0.
type wasm3CFFrame struct {
	kind     wasm3CFKind
	boundary int   // valid if kind == wasm3CFBlock: the boundary at which this block ends
	target   int32 // valid if kind == wasm3CFPlanLoop or wasm3CFPlanBlock: the target block ID
}

type wasm3CFKind uint8

const (
	wasm3CFBlock     wasm3CFKind = iota // wasm `block` opened for a forward AJMP target
	wasm3CFIf                           // wasm `if` opened by an SSA AIf
	wasm3CFLoop                         // wasm `loop` of the dispatch-loop scheme
	wasm3CFDispatch                     // a `block` of the dispatch-loop scheme's br_table nest
	wasm3CFPlanLoop                     // wasm `loop` opened by the relooper plan; target = loop header block ID
	wasm3CFPlanBlock                    // wasm `block` opened by the relooper plan; target = block ID that follows the scope's `end`
	wasm3CFInline                       // wasm `block` or `loop` emitted inline by a single SSA op's codegen (M3.5 linear-memory bridge byte loops); only AEnd at matching depth closes it
)

// wasm3CFG is the result of the encoder's CFG pre-pass.
//
// Boundaries are numbered by the order ARESUMEPOINT progs appear:
// boundary 0 is the function start, boundary 1 is right after the first
// ARESUMEPOINT, and so on. The SSA backend emits one ARESUMEPOINT at
// the end of every basic block, so with N ARESUMEPOINTs there are N
// basic blocks numbered BB_0..BB_{N-1}, with BB_K starting at boundary
// K and ending at boundary K+1.
//
// One of two modes is selected:
//
//   - forward-only: every AJMP is a forward edge (target > source).
//     The body wraps one wasm `block` per distinct forward-target
//     boundary; AJMPs become `br <depth>` exiting the right block.
//     This is compact but cannot express back-edges.
//
//   - dispatch: at least one AJMP is a back-edge. The body wraps the
//     whole code in a `loop $L` plus N nested `block`s ending right
//     before each basic block; a `br_table` at the top of $L
//     dispatches on a fresh i32 local set by each AJMP. Every AJMP —
//     forward or back — becomes `i32.const K; local.set $dispatch;
//     br $L`, restarting the loop at the right basic block.
type wasm3CFG struct {
	dispatch bool // if true, use the dispatch-loop scheme instead of nested blocks

	// forwardTargets, mode == forward-only: the sorted, deduped set of
	// boundary indices some AJMP targets.
	forwardTargets []int

	// numBlocks, mode == dispatch: the basic-block count (== number of
	// ARESUMEPOINT progs). The dispatch wraps numBlocks nested blocks.
	numBlocks int

	// jmpTarget maps each AJMP prog to its target boundary index.
	jmpTarget map[*obj.Prog]int
}

// wasm3AnalyzeCFG scans s's prog stream for ARESUMEPOINT (basic-block
// boundary) and AJMP, classifies each AJMP edge as forward or back, and
// returns the plan the body encoder needs. The plan defaults to the
// compact forward-only nested-blocks scheme if every AJMP is forward,
// and switches to the dispatch-loop scheme the moment a back-edge is
// found.
//
// Returns ok=false on a malformed stream — an AJMP whose branch target
// is not set, or an AJMP whose target is mid-basic-block (which the SSA
// backend should never emit at this stage).
//
// Boundary 0 is the function start; the body never JMPs to it, so the
// useful target range is [1, N] where N is the number of ARESUMEPOINT
// progs. AJMP targets are recorded as the boundary index of the basic
// block that begins at the target prog (i.e. the index of the
// ARESUMEPOINT immediately preceding the target).
func wasm3AnalyzeCFG(s *obj.LSym) (*wasm3CFG, bool) {
	// Pass 1: number ARESUMEPOINTs and map each Pc that begins a basic
	// block (the prog right after an ARESUMEPOINT) to its boundary
	// index. Function entry is boundary 0; nothing JMPs there at this
	// stage so it does not need a Pc entry.
	boundaryOfPc := make(map[int64]int)
	boundaryIdx := 0
	for p := s.Func().Text; p != nil; p = p.Link {
		if p.As == ARESUMEPOINT {
			boundaryIdx++
			if next := p.Link; next != nil {
				boundaryOfPc[next.Pc] = boundaryIdx
			}
		}
	}
	numBoundaries := boundaryIdx

	// Pass 2: classify each AJMP. seenBoundary tracks which basic
	// block we are currently inside; an AJMP is a back-edge if its
	// target boundary is at or before the boundary we are inside.
	cfg := &wasm3CFG{jmpTarget: make(map[*obj.Prog]int)}
	targetSet := make(map[int]bool)
	hasBackEdge := false
	seenBoundary := 0
	for p := s.Func().Text; p != nil; p = p.Link {
		if p.As == ARESUMEPOINT {
			seenBoundary++
			continue
		}
		if p.As != obj.AJMP {
			continue
		}
		target := p.To.Target()
		if target == nil {
			return nil, false
		}
		tBoundary, known := boundaryOfPc[target.Pc]
		if !known {
			return nil, false // jumping into the middle of a basic block
		}
		if tBoundary <= seenBoundary {
			hasBackEdge = true
		}
		cfg.jmpTarget[p] = tBoundary
		targetSet[tBoundary] = true
	}
	if hasBackEdge {
		cfg.dispatch = true
		cfg.numBlocks = numBoundaries
		return cfg, true
	}
	cfg.forwardTargets = make([]int, 0, len(targetSet))
	for k := range targetSet {
		cfg.forwardTargets = append(cfg.forwardTargets, k)
	}
	sort.Ints(cfg.forwardTargets)
	return cfg, true
}

// wasm3LocalDecl is one entry of a wasm function body's local
// declaration vector: count locals of the given wasm value type.
type wasm3LocalDecl struct {
	count uint64
	typ   byte
}

// wasm3ParamCopy is a body-prologue copy of a sub-word integer
// parameter from its i32 parameter local into the i64 SSA-register
// local the body works with, widening it on the way.
type wasm3ParamCopy struct {
	paramLocal uint64 // source: the i32 wasm parameter local
	regLocal   uint64 // dest: the i64 SSA-register local
}

// wasm3SpillKey identifies a register-spill slot: the auto/param frame
// area AddrAuto records, plus its offset. Each distinct slot gets its
// own wasm spill local — see wasm3Locals.
type wasm3SpillKey struct {
	name obj.AddrName
	off  int64
}

// wasm3SpillSlot is the wasm3SpillKey for a spill load/store operand,
// or the zero key if a is not an auto/param slot (a real linear-memory
// access).
func wasm3SpillSlot(a *obj.Addr) wasm3SpillKey {
	if a.Type != obj.TYPE_MEM || (a.Name != obj.NAME_AUTO && a.Name != obj.NAME_PARAM) {
		return wasm3SpillKey{}
	}
	return wasm3SpillKey{name: a.Name, off: a.Offset}
}

// wasm3Locals builds, for s, the register-to-wasm-local-index map, the
// spill-slot-to-wasm-local-index map, the body's local declaration
// vector, and the parameter-widening prologue.
//
// A wasm function's parameters are locals 0..nparams-1 in signature
// order; the register ABI assigns the integer parameters to R0, R1, …
// and the floating parameters to F0, F1, …, reconstructed here from
// the parameter storage types in whichever signature aux s carries —
// an obj.WasmType for an ordinary wasm3 function, or the
// WasmExport/WasmImport aux for a function bearing those pragmas. ok is
// false if s carries none (a function the compiler left as ()->(), or
// a hand-written stub): with no known parameter layout it is left to
// the stub.
//
// The SSA backend works in i64 GP registers and freely reuses a
// parameter's register for unrelated i64 values once the parameter is
// dead. An i64 parameter can therefore alias its parameter local
// directly. A sub-word (i32) parameter local cannot: it would have to
// hold an i64 after reuse. So each i32 parameter that the body
// actually uses gets its own i64 register-local, declared after the
// parameters, plus a prologue copy that widens the parameter into it.
// Every other local-class register (R0-R15, F0-F31) the body
// references likewise becomes a declared i64/f register-local.
//
// A register spill (OpStoreReg/OpLoadReg) targets a wasm local, not a
// linear-memory frame slot — wasm3 has no Go stack frame. Each distinct
// auto/param slot the body spills to or reloads from gets its own
// declared spill local. Registers that are not local-class — SP, g,
// CTXT — are not mapped; a body using one makes encodeWasm3Body fall
// back, since real linear-memory access is a later rung.
func wasm3Locals(s *obj.LSym) (localOf map[int16]uint64, spillOf map[wasm3SpillKey]uint64, decls []wasm3LocalDecl, prologue []wasm3ParamCopy, nextLocal uint64, ok bool) {
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
		return nil, nil, nil, nil, 0, false
	}

	// paramInfo records, per parameter register, the parameter local it
	// holds and whether that local is the narrow (i32) kind.
	type paramInfo struct {
		local  uint64
		narrow bool
	}
	paramOf := make(map[int16]paramInfo, len(params))
	var intParam, floatParam int16
	for i, f := range params {
		var reg int16
		narrow := false
		switch f.Type {
		case obj.WasmF32, obj.WasmF64:
			reg = REG_F0 + floatParam
			floatParam++
		case obj.WasmI32, obj.WasmPtr, obj.WasmBool:
			// All three encode as i32 in the wasm signature (the
			// linker collapses them in fieldsToTypes). Treat them as
			// narrow so the prologue widens them to the i64 SSA
			// register width.
			reg = REG_R0 + intParam
			intParam++
			narrow = true
		default: // WasmI64, WasmRef
			reg = REG_R0 + intParam
			intParam++
		}
		paramOf[reg] = paramInfo{local: uint64(i), narrow: narrow}
	}

	localOf = make(map[int16]uint64)
	next := uint64(len(params)) // register/per-value locals follow the parameter locals

	// M3 Phase 3: declare per-value locals from the wasm3PlaceValues
	// pass *immediately* after the parameter locals so that the
	// SSA-genssa side can compute each per-value local's absolute
	// wasm index as len(params) + Wasm3ValueLocals[v.ID] without
	// needing to know how many register-locals or spill-locals the
	// obj backend will materialize. Register-locals (and later
	// spill-locals) follow these.
	for _, t := range fn.Wasm3LocalTypes {
		decls = append(decls, wasm3LocalDecl{count: 1, typ: t})
		next++
	}

	declare := func(reg int16) {
		if reg < REG_R0 || reg > REG_F31 {
			return // not a local-class register (SP, g, CTXT, …)
		}
		if _, done := localOf[reg]; done {
			return
		}
		if pi, isParam := paramOf[reg]; isParam && !pi.narrow {
			// i64 (or float) parameter: alias the register to its
			// parameter local — reuse for another i64 value is safe.
			localOf[reg] = pi.local
			return
		}
		// A sub-word parameter register, or a non-parameter temporary:
		// a fresh register-local, plus a widening copy if it is a
		// parameter.
		localOf[reg] = next
		decls = append(decls, wasm3LocalDecl{count: 1, typ: byte(regType(reg))})
		if pi, isParam := paramOf[reg]; isParam {
			prologue = append(prologue, wasm3ParamCopy{paramLocal: pi.local, regLocal: next})
		}
		next++
	}

	spillOf = make(map[wasm3SpillKey]uint64)
	declareSpill := func(a *obj.Addr, typ valueType) {
		key := wasm3SpillSlot(a)
		if key == (wasm3SpillKey{}) {
			return // not an auto/param slot
		}
		if _, done := spillOf[key]; done {
			return
		}
		spillOf[key] = next
		decls = append(decls, wasm3LocalDecl{count: 1, typ: byte(typ)})
		next++
	}

	for p := fn.Text; p != nil; p = p.Link {
		switch p.As {
		case AGet:
			declare(p.From.Reg)
		case ASet, ATee:
			declare(p.To.Reg)
		case AI64Store:
			declareSpill(&p.To, i64)
		case AF32Store:
			declareSpill(&p.To, f32)
		case AF64Store:
			declareSpill(&p.To, f64)
		case AI64Load:
			declareSpill(&p.From, i64)
		case AF32Load:
			declareSpill(&p.From, f32)
		case AF64Load:
			declareSpill(&p.From, f64)
		}
	}

	return localOf, spillOf, decls, prologue, next, true
}

// assembleWasm3ExportWrapper emits the body of a //go:wasmexport
// wrapper for GOARCH=wasm3.
//
// The wrapper signature is the wasmexport view, with pointer-shaped
// scalars carried in WasmPtr (i32) — the host's linear-memory address
// width. The wrapped Go function (compiled by wasm3IntField) instead
// carries pointer-shaped scalars in i64, matching the SSA register
// width. The wrapper bridges the two by widening each WasmPtr param
// with `i64.extend_i32_u` before the call and narrowing each WasmPtr
// result with `i32.wrap_i64` after — every other field type lines up
// (WasmI32/WasmI64/WasmBool/WasmF32/WasmF64 are the same on both
// sides).
//
// The body is:
//
//	local declaration count: 0
//	local.get 0
//	[ i64.extend_i32_u ]   ; if param 0 is WasmPtr
//	local.get 1
//	[ i64.extend_i32_u ]   ; if param 1 is WasmPtr
//	...
//	call <wrapped>         ; operand filled in by the R_CALL reloc
//	[ i32.wrap_i64 ]       ; if a result is WasmPtr
//	end
func assembleWasm3ExportWrapper(ctxt *obj.Link, s *obj.LSym, we *obj.WasmExport) {
	w := new(bytes.Buffer)
	writeUleb128(w, 0) // local declaration count
	for i, p := range we.Params {
		writeOpcode(w, ALocalGet)
		writeUleb128(w, uint64(i))
		if p.Type == obj.WasmPtr {
			writeOpcode(w, AI64ExtendI32U)
		}
	}
	writeOpcode(w, ACall)
	callOff := int32(w.Len())
	relocs := []obj.Reloc{{
		Type: objabi.R_CALL,
		Off:  callOff,
		Siz:  1, // variable-sized; the linker writes the function index
		Sym:  we.WrappedSym,
	}}
	for _, r := range we.Results {
		if r.Type == obj.WasmPtr {
			writeOpcode(w, AI32WrapI64)
		}
	}
	w.WriteByte(0x0b) // end
	s.P = w.Bytes()
	for _, r := range relocs {
		s.AddRel(ctxt, r)
	}
}

// wasm3LoadStoreAlign returns the natural alignment immediate for a
// wasm load/store opcode: the operand's byte width as a power of 2.
// Mirrors cmd/internal/obj/wasm.align (the existing wasm backend's
// helper); kept private here so wasm3obj.go is self-contained.
func wasm3LoadStoreAlign(as obj.As) uint64 {
	switch as {
	case AI32Load8S, AI32Load8U, AI64Load8S, AI64Load8U, AI32Store8, AI64Store8:
		return 0
	case AI32Load16S, AI32Load16U, AI64Load16S, AI64Load16U, AI32Store16, AI64Store16:
		return 1
	case AI32Load, AF32Load, AI64Load32S, AI64Load32U, AI32Store, AF32Store, AI64Store32:
		return 2
	case AI64Load, AF64Load, AI64Store, AF64Store:
		return 3
	}
	panic("wasm3LoadStoreAlign: bad op")
}

// assembleWasm3ImportWrapper emits the body of a //go:wasmimport stub
// for GOARCH=wasm3.
//
// The wrapper has the same wasm signature as the host function it
// imports (both produced by paramsToWasmFields). The body is the
// trivial forwarder: push each parameter local in order, then call
// the host import. The R_WASMIMPORT relocation tells the linker to
// patch the call's function index with the import's slot in the
// hostImportMap.
//
// The body is:
//
//	local declaration count: 0
//	local.get 0           ; for each parameter local, in order
//	local.get 1
//	...
//	call <import>         ; operand filled in by the R_WASMIMPORT reloc
//	end
func assembleWasm3ImportWrapper(ctxt *obj.Link, s *obj.LSym, wi *obj.WasmImport) {
	wi.CreateAuxSym()
	w := new(bytes.Buffer)
	writeUleb128(w, 0) // local declaration count
	for i := range wi.Params {
		writeOpcode(w, ALocalGet)
		writeUleb128(w, uint64(i))
	}
	writeOpcode(w, ACall)
	callOff := int32(w.Len())
	w.WriteByte(0x0b) // end (will be at len-1 once the linker grows the call operand)
	s.P = w.Bytes()
	s.AddRel(ctxt, obj.Reloc{
		Type: objabi.R_WASMIMPORT,
		Off:  callOff,
		Siz:  1, // variable-sized; the linker writes the import index
		Sym:  s, // self — the linker uses this to look up the import index
	})
}

