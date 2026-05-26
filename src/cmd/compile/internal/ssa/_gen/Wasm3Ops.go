// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "strings"

// Wasm3Ops.go defines the SSA opcodes for GOARCH=wasm3. For the M2 SSA-layer
// fork it is a copy of WasmOps.go; the divergence to the WebAssembly 3.0
// object model (struct.new, ref-typed values, no write barriers) lands in
// subsequent M2 commits. See doc/wasm3-m2-design.md.

var regNamesWasm3 = []string{
	"R0",
	"R1",
	"R2",
	"R3",
	"R4",
	"R5",
	"R6",
	"R7",
	"R8",
	"R9",
	"R10",
	"R11",
	"R12",
	"R13",
	"R14",
	"R15",

	"F0",
	"F1",
	"F2",
	"F3",
	"F4",
	"F5",
	"F6",
	"F7",
	"F8",
	"F9",
	"F10",
	"F11",
	"F12",
	"F13",
	"F14",
	"F15",

	"F16",
	"F17",
	"F18",
	"F19",
	"F20",
	"F21",
	"F22",
	"F23",
	"F24",
	"F25",
	"F26",
	"F27",
	"F28",
	"F29",
	"F30",
	"F31",

	"SP",
	"g",

	// pseudo-registers
	"SB",
}

func init() {
	// Make map from reg names to reg integers.
	if len(regNamesWasm3) > 64 {
		panic("too many registers")
	}
	num := map[string]int{}
	for i, name := range regNamesWasm3 {
		num[name] = i
	}
	buildReg := func(s string) regMask {
		m := regMask{}
		for _, r := range strings.Split(s, " ") {
			if n, ok := num[r]; ok {
				m = m.addReg(uint(n))
				continue
			}
			panic("register " + r + " not found")
		}
		return m
	}

	var (
		gp     = buildReg("R0 R1 R2 R3 R4 R5 R6 R7 R8 R9 R10 R11 R12 R13 R14 R15")
		fp32   = buildReg("F0 F1 F2 F3 F4 F5 F6 F7 F8 F9 F10 F11 F12 F13 F14 F15")
		fp64   = buildReg("F16 F17 F18 F19 F20 F21 F22 F23 F24 F25 F26 F27 F28 F29 F30 F31")
		gpsp   = gp.union(buildReg("SP"))
		gpspsb = gpsp.union(buildReg("SB"))
		// The "registers", which are actually local variables, can get clobbered
		// if we're switching goroutines, because it unwinds the WebAssembly stack.
		callerSave = gp.union(fp32).union(fp64).union(buildReg("g"))
		// M2 cutover, Stage C.2: unlike GOARCH=wasm, a wasm3 call does not
		// clobber the caller's registers. wasm3 functions are native
		// typed wasm functions, so the SSA "registers" are wasm locals,
		// which a `call` instruction leaves untouched — and M2 is
		// single-goroutine, so there is no goroutine switch unwinding the
		// WebAssembly stack. A value therefore survives a call in its
		// register, with no spill to a linear-memory frame.
		callClobbers = regMask{}
	)

	// Common regInfo
	var (
		gp01      = regInfo{inputs: nil, outputs: []regMask{gp}}
		gp11      = regInfo{inputs: []regMask{gpsp}, outputs: []regMask{gp}}
		gp21      = regInfo{inputs: []regMask{gpsp, gpsp}, outputs: []regMask{gp}}
		gp31      = regInfo{inputs: []regMask{gpsp, gpsp, gpsp}, outputs: []regMask{gp}}
		fp32_01   = regInfo{inputs: nil, outputs: []regMask{fp32}}
		fp32_11   = regInfo{inputs: []regMask{fp32}, outputs: []regMask{fp32}}
		fp32_21   = regInfo{inputs: []regMask{fp32, fp32}, outputs: []regMask{fp32}}
		fp32_21gp = regInfo{inputs: []regMask{fp32, fp32}, outputs: []regMask{gp}}
		fp64_01   = regInfo{inputs: nil, outputs: []regMask{fp64}}
		fp64_11   = regInfo{inputs: []regMask{fp64}, outputs: []regMask{fp64}}
		fp64_21   = regInfo{inputs: []regMask{fp64, fp64}, outputs: []regMask{fp64}}
		fp64_21gp = regInfo{inputs: []regMask{fp64, fp64}, outputs: []regMask{gp}}
		gpload    = regInfo{inputs: []regMask{gpspsb, regMask{}}, outputs: []regMask{gp}}
		gpstore   = regInfo{inputs: []regMask{gpspsb, gpsp, regMask{}}}
		fp32load  = regInfo{inputs: []regMask{gpspsb, regMask{}}, outputs: []regMask{fp32}}
		fp32store = regInfo{inputs: []regMask{gpspsb, fp32, regMask{}}}
		fp64load  = regInfo{inputs: []regMask{gpspsb, regMask{}}, outputs: []regMask{fp64}}
		fp64store = regInfo{inputs: []regMask{gpspsb, fp64, regMask{}}}
	)

	var Wasm3Ops = []opData{
		// M2 cutover, Stage C.2: the call ops are variadic (argLength
		// -1), like the register-ABI arches' CALL* ops. expand_calls
		// appends each register-resident argument to the call value as
		// an explicit arg, with mem as the last arg; a fixed argLength
		// would corrupt the value. The leading fixed inputs (codeptr,
		// closure) keep their positions.
		{name: "LoweredStaticCall", argLength: -1, reg: regInfo{clobbers: callClobbers}, aux: "CallOff", call: true},                                           // call static function aux.(*obj.LSym). last arg=mem, auxint=argsize, returns mem
		{name: "LoweredTailCall", argLength: -1, reg: regInfo{clobbers: callClobbers}, aux: "CallOff", call: true, tailCall: true},                             // tail call static function aux.(*obj.LSym). last arg=mem, auxint=argsize, returns mem
		{name: "LoweredTailCallInter", argLength: -1, reg: regInfo{inputs: []regMask{gp}, clobbers: callClobbers}, aux: "CallOff", call: true, tailCall: true}, // tail call fn by pointer. arg0=codeptr, last arg=mem, auxint=argsize, returns mem
		{name: "LoweredClosureCall", argLength: -1, reg: regInfo{inputs: []regMask{gp, gp, regMask{}}, clobbers: callClobbers}, aux: "CallOff", call: true},    // call function via closure. arg0=codeptr, arg1=closure, last arg=mem, auxint=argsize, returns mem
		{name: "LoweredInterCall", argLength: -1, reg: regInfo{inputs: []regMask{gp}, clobbers: callClobbers}, aux: "CallOff", call: true},                     // call fn by pointer. arg0=codeptr, last arg=mem, auxint=argsize, returns mem

		{name: "LoweredAddr", argLength: 1, reg: gp11, aux: "SymOff", rematerializeable: true, symEffect: "Addr"}, // returns base+aux+auxint, arg0=base
		{name: "LoweredMove", argLength: 3, reg: regInfo{inputs: []regMask{gp, gp}}, aux: "Int64"},                // large move. arg0=dst, arg1=src, arg2=mem, auxint=len, returns mem
		{name: "LoweredZero", argLength: 2, reg: regInfo{inputs: []regMask{gp}}, aux: "Int64"},                    // large zeroing. arg0=start, arg1=mem, auxint=len, returns mem

		{name: "LoweredGetClosurePtr", reg: gp01},                                                                          // returns wasm.REG_CTXT, the closure pointer
		{name: "LoweredGetCallerPC", reg: gp01, rematerializeable: true},                                                   // returns the PC of the caller of the current function
		{name: "LoweredGetCallerSP", argLength: 1, reg: gp01, rematerializeable: true},                                     // returns the SP of the caller of the current function. arg0=mem.
		{name: "LoweredNilCheck", argLength: 2, reg: regInfo{inputs: []regMask{gp}}, nilCheck: true, faultOnNilArg0: true}, // panic if arg0 is nil. arg1=mem
		{name: "LoweredWB", argLength: 1, reg: regInfo{clobbers: callerSave, outputs: []regMask{gp}}, aux: "Int64"},        // invokes runtime.gcWriteBarrier{auxint}. arg0=mem, auxint=# of buffer entries needed. Returns a pointer to a write barrier buffer.

		// LoweredConvert converts between pointers and integers.
		// We have a special op for this so as to not confuse GCCallOff
		// (particularly stack maps). It takes a memory arg so it
		// gets correctly ordered with respect to GC safepoints.
		// arg0=ptr/int arg1=mem, output=int/ptr
		//
		// TODO(neelance): LoweredConvert should not be necessary any more, since OpConvert does not need to be lowered any more (CL 108496).
		{name: "LoweredConvert", argLength: 2, reg: regInfo{inputs: []regMask{gp}, outputs: []regMask{gp}}},

		// The following are native WebAssembly instructions, see https://webassembly.github.io/spec/core/syntax/instructions.html

		{name: "Select", asm: "Select", argLength: 3, reg: gp31}, // returns arg0 if arg2 != 0, otherwise returns arg1

		{name: "I64Load8U", asm: "I64Load8U", argLength: 2, reg: gpload, aux: "Int64", typ: "UInt8"},    // read unsigned 8-bit integer from address arg0+aux, arg1=mem
		{name: "I64Load8S", asm: "I64Load8S", argLength: 2, reg: gpload, aux: "Int64", typ: "Int8"},     // read signed 8-bit integer from address arg0+aux, arg1=mem
		{name: "I64Load16U", asm: "I64Load16U", argLength: 2, reg: gpload, aux: "Int64", typ: "UInt16"}, // read unsigned 16-bit integer from address arg0+aux, arg1=mem
		{name: "I64Load16S", asm: "I64Load16S", argLength: 2, reg: gpload, aux: "Int64", typ: "Int16"},  // read signed 16-bit integer from address arg0+aux, arg1=mem
		{name: "I64Load32U", asm: "I64Load32U", argLength: 2, reg: gpload, aux: "Int64", typ: "UInt32"}, // read unsigned 32-bit integer from address arg0+aux, arg1=mem
		{name: "I64Load32S", asm: "I64Load32S", argLength: 2, reg: gpload, aux: "Int64", typ: "Int32"},  // read signed 32-bit integer from address arg0+aux, arg1=mem
		{name: "I64Load", asm: "I64Load", argLength: 2, reg: gpload, aux: "Int64", typ: "UInt64"},       // read 64-bit integer from address arg0+aux, arg1=mem
		{name: "I64Store8", asm: "I64Store8", argLength: 3, reg: gpstore, aux: "Int64", typ: "Mem"},     // store 8-bit integer arg1 at address arg0+aux, arg2=mem, returns mem
		{name: "I64Store16", asm: "I64Store16", argLength: 3, reg: gpstore, aux: "Int64", typ: "Mem"},   // store 16-bit integer arg1 at address arg0+aux, arg2=mem, returns mem
		{name: "I64Store32", asm: "I64Store32", argLength: 3, reg: gpstore, aux: "Int64", typ: "Mem"},   // store 32-bit integer arg1 at address arg0+aux, arg2=mem, returns mem
		{name: "I64Store", asm: "I64Store", argLength: 3, reg: gpstore, aux: "Int64", typ: "Mem"},       // store 64-bit integer arg1 at address arg0+aux, arg2=mem, returns mem

		{name: "F32Load", asm: "F32Load", argLength: 2, reg: fp32load, aux: "Int64", typ: "Float32"}, // read 32-bit float from address arg0+aux, arg1=mem
		{name: "F64Load", asm: "F64Load", argLength: 2, reg: fp64load, aux: "Int64", typ: "Float64"}, // read 64-bit float from address arg0+aux, arg1=mem
		{name: "F32Store", asm: "F32Store", argLength: 3, reg: fp32store, aux: "Int64", typ: "Mem"},  // store 32-bit float arg1 at address arg0+aux, arg2=mem, returns mem
		{name: "F64Store", asm: "F64Store", argLength: 3, reg: fp64store, aux: "Int64", typ: "Mem"},  // store 64-bit float arg1 at address arg0+aux, arg2=mem, returns mem

		{name: "I64Const", reg: gp01, aux: "Int64", rematerializeable: true, typ: "Int64"},        // returns the constant integer aux
		{name: "F32Const", reg: fp32_01, aux: "Float32", rematerializeable: true, typ: "Float32"}, // returns the constant float aux
		{name: "F64Const", reg: fp64_01, aux: "Float64", rematerializeable: true, typ: "Float64"}, // returns the constant float aux

		{name: "I64Eqz", asm: "I64Eqz", argLength: 1, reg: gp11, typ: "Bool"}, // arg0 == 0
		{name: "I64Eq", asm: "I64Eq", argLength: 2, reg: gp21, typ: "Bool"},   // arg0 == arg1
		{name: "I64Ne", asm: "I64Ne", argLength: 2, reg: gp21, typ: "Bool"},   // arg0 != arg1
		{name: "I64LtS", asm: "I64LtS", argLength: 2, reg: gp21, typ: "Bool"}, // arg0 < arg1 (signed)
		{name: "I64LtU", asm: "I64LtU", argLength: 2, reg: gp21, typ: "Bool"}, // arg0 < arg1 (unsigned)
		{name: "I64GtS", asm: "I64GtS", argLength: 2, reg: gp21, typ: "Bool"}, // arg0 > arg1 (signed)
		{name: "I64GtU", asm: "I64GtU", argLength: 2, reg: gp21, typ: "Bool"}, // arg0 > arg1 (unsigned)
		{name: "I64LeS", asm: "I64LeS", argLength: 2, reg: gp21, typ: "Bool"}, // arg0 <= arg1 (signed)
		{name: "I64LeU", asm: "I64LeU", argLength: 2, reg: gp21, typ: "Bool"}, // arg0 <= arg1 (unsigned)
		{name: "I64GeS", asm: "I64GeS", argLength: 2, reg: gp21, typ: "Bool"}, // arg0 >= arg1 (signed)
		{name: "I64GeU", asm: "I64GeU", argLength: 2, reg: gp21, typ: "Bool"}, // arg0 >= arg1 (unsigned)

		{name: "F32Eq", asm: "F32Eq", argLength: 2, reg: fp32_21gp, typ: "Bool"}, // arg0 == arg1
		{name: "F32Ne", asm: "F32Ne", argLength: 2, reg: fp32_21gp, typ: "Bool"}, // arg0 != arg1
		{name: "F32Lt", asm: "F32Lt", argLength: 2, reg: fp32_21gp, typ: "Bool"}, // arg0 < arg1
		{name: "F32Gt", asm: "F32Gt", argLength: 2, reg: fp32_21gp, typ: "Bool"}, // arg0 > arg1
		{name: "F32Le", asm: "F32Le", argLength: 2, reg: fp32_21gp, typ: "Bool"}, // arg0 <= arg1
		{name: "F32Ge", asm: "F32Ge", argLength: 2, reg: fp32_21gp, typ: "Bool"}, // arg0 >= arg1

		{name: "F64Eq", asm: "F64Eq", argLength: 2, reg: fp64_21gp, typ: "Bool"}, // arg0 == arg1
		{name: "F64Ne", asm: "F64Ne", argLength: 2, reg: fp64_21gp, typ: "Bool"}, // arg0 != arg1
		{name: "F64Lt", asm: "F64Lt", argLength: 2, reg: fp64_21gp, typ: "Bool"}, // arg0 < arg1
		{name: "F64Gt", asm: "F64Gt", argLength: 2, reg: fp64_21gp, typ: "Bool"}, // arg0 > arg1
		{name: "F64Le", asm: "F64Le", argLength: 2, reg: fp64_21gp, typ: "Bool"}, // arg0 <= arg1
		{name: "F64Ge", asm: "F64Ge", argLength: 2, reg: fp64_21gp, typ: "Bool"}, // arg0 >= arg1

		{name: "I64Add", asm: "I64Add", argLength: 2, reg: gp21, typ: "Int64"},                         // arg0 + arg1
		{name: "I64AddConst", asm: "I64Add", argLength: 1, reg: gp11, aux: "Int64", typ: "Int64"},      // arg0 + aux
		{name: "I64Sub", asm: "I64Sub", argLength: 2, reg: gp21, typ: "Int64"},                         // arg0 - arg1
		{name: "I64Mul", asm: "I64Mul", argLength: 2, reg: gp21, typ: "Int64"},                         // arg0 * arg1
		{name: "I64DivS", asm: "I64DivS", argLength: 2, reg: gp21, typ: "Int64", hasSideEffects: true}, // arg0 / arg1 (signed)
		{name: "I64DivU", asm: "I64DivU", argLength: 2, reg: gp21, typ: "Int64", hasSideEffects: true}, // arg0 / arg1 (unsigned)
		{name: "I64RemS", asm: "I64RemS", argLength: 2, reg: gp21, typ: "Int64", hasSideEffects: true}, // arg0 % arg1 (signed)
		{name: "I64RemU", asm: "I64RemU", argLength: 2, reg: gp21, typ: "Int64", hasSideEffects: true}, // arg0 % arg1 (unsigned)
		{name: "I64And", asm: "I64And", argLength: 2, reg: gp21, typ: "Int64"},                         // arg0 & arg1
		{name: "I64Or", asm: "I64Or", argLength: 2, reg: gp21, typ: "Int64"},                           // arg0 | arg1
		{name: "I64Xor", asm: "I64Xor", argLength: 2, reg: gp21, typ: "Int64"},                         // arg0 ^ arg1
		{name: "I64Shl", asm: "I64Shl", argLength: 2, reg: gp21, typ: "Int64"},                         // arg0 << (arg1 % 64)
		{name: "I64ShrS", asm: "I64ShrS", argLength: 2, reg: gp21, typ: "Int64"},                       // arg0 >> (arg1 % 64) (signed)
		{name: "I64ShrU", asm: "I64ShrU", argLength: 2, reg: gp21, typ: "Int64"},                       // arg0 >> (arg1 % 64) (unsigned)

		{name: "F32Neg", asm: "F32Neg", argLength: 1, reg: fp32_11, typ: "Float32"}, // -arg0
		{name: "F32Add", asm: "F32Add", argLength: 2, reg: fp32_21, typ: "Float32"}, // arg0 + arg1
		{name: "F32Sub", asm: "F32Sub", argLength: 2, reg: fp32_21, typ: "Float32"}, // arg0 - arg1
		{name: "F32Mul", asm: "F32Mul", argLength: 2, reg: fp32_21, typ: "Float32"}, // arg0 * arg1
		{name: "F32Div", asm: "F32Div", argLength: 2, reg: fp32_21, typ: "Float32"}, // arg0 / arg1

		{name: "F64Neg", asm: "F64Neg", argLength: 1, reg: fp64_11, typ: "Float64"}, // -arg0
		{name: "F64Add", asm: "F64Add", argLength: 2, reg: fp64_21, typ: "Float64"}, // arg0 + arg1
		{name: "F64Sub", asm: "F64Sub", argLength: 2, reg: fp64_21, typ: "Float64"}, // arg0 - arg1
		{name: "F64Mul", asm: "F64Mul", argLength: 2, reg: fp64_21, typ: "Float64"}, // arg0 * arg1
		{name: "F64Div", asm: "F64Div", argLength: 2, reg: fp64_21, typ: "Float64"}, // arg0 / arg1

		{name: "I64TruncSatF64S", asm: "I64TruncSatF64S", argLength: 1, reg: regInfo{inputs: []regMask{fp64}, outputs: []regMask{gp}}, typ: "Int64"}, // truncates the float arg0 to a signed integer (saturating)
		{name: "I64TruncSatF64U", asm: "I64TruncSatF64U", argLength: 1, reg: regInfo{inputs: []regMask{fp64}, outputs: []regMask{gp}}, typ: "Int64"}, // truncates the float arg0 to an unsigned integer (saturating)
		{name: "I64TruncSatF32S", asm: "I64TruncSatF32S", argLength: 1, reg: regInfo{inputs: []regMask{fp32}, outputs: []regMask{gp}}, typ: "Int64"}, // truncates the float arg0 to a signed integer (saturating)
		{name: "I64TruncSatF32U", asm: "I64TruncSatF32U", argLength: 1, reg: regInfo{inputs: []regMask{fp32}, outputs: []regMask{gp}}, typ: "Int64"}, // truncates the float arg0 to an unsigned integer (saturating)
		{name: "F32ConvertI64S", asm: "F32ConvertI64S", argLength: 1, reg: regInfo{inputs: []regMask{gp}, outputs: []regMask{fp32}}, typ: "Float32"}, // converts the signed integer arg0 to a float
		{name: "F32ConvertI64U", asm: "F32ConvertI64U", argLength: 1, reg: regInfo{inputs: []regMask{gp}, outputs: []regMask{fp32}}, typ: "Float32"}, // converts the unsigned integer arg0 to a float
		{name: "F64ConvertI64S", asm: "F64ConvertI64S", argLength: 1, reg: regInfo{inputs: []regMask{gp}, outputs: []regMask{fp64}}, typ: "Float64"}, // converts the signed integer arg0 to a float
		{name: "F64ConvertI64U", asm: "F64ConvertI64U", argLength: 1, reg: regInfo{inputs: []regMask{gp}, outputs: []regMask{fp64}}, typ: "Float64"}, // converts the unsigned integer arg0 to a float
		{name: "F32DemoteF64", asm: "F32DemoteF64", argLength: 1, reg: regInfo{inputs: []regMask{fp64}, outputs: []regMask{fp32}}, typ: "Float32"},
		{name: "F64PromoteF32", asm: "F64PromoteF32", argLength: 1, reg: regInfo{inputs: []regMask{fp32}, outputs: []regMask{fp64}}, typ: "Float64"},

		{name: "I64Extend8S", asm: "I64Extend8S", argLength: 1, reg: gp11, typ: "Int64"},   // sign-extend arg0 from 8 to 64 bit
		{name: "I64Extend16S", asm: "I64Extend16S", argLength: 1, reg: gp11, typ: "Int64"}, // sign-extend arg0 from 16 to 64 bit
		{name: "I64Extend32S", asm: "I64Extend32S", argLength: 1, reg: gp11, typ: "Int64"}, // sign-extend arg0 from 32 to 64 bit

		{name: "F32Sqrt", asm: "F32Sqrt", argLength: 1, reg: fp32_11, typ: "Float32"},         // sqrt(arg0)
		{name: "F32Trunc", asm: "F32Trunc", argLength: 1, reg: fp32_11, typ: "Float32"},       // trunc(arg0)
		{name: "F32Ceil", asm: "F32Ceil", argLength: 1, reg: fp32_11, typ: "Float32"},         // ceil(arg0)
		{name: "F32Floor", asm: "F32Floor", argLength: 1, reg: fp32_11, typ: "Float32"},       // floor(arg0)
		{name: "F32Nearest", asm: "F32Nearest", argLength: 1, reg: fp32_11, typ: "Float32"},   // round(arg0)
		{name: "F32Abs", asm: "F32Abs", argLength: 1, reg: fp32_11, typ: "Float32"},           // abs(arg0)
		{name: "F32Copysign", asm: "F32Copysign", argLength: 2, reg: fp32_21, typ: "Float32"}, // copysign(arg0, arg1)

		{name: "F64Sqrt", asm: "F64Sqrt", argLength: 1, reg: fp64_11, typ: "Float64"},         // sqrt(arg0)
		{name: "F64Trunc", asm: "F64Trunc", argLength: 1, reg: fp64_11, typ: "Float64"},       // trunc(arg0)
		{name: "F64Ceil", asm: "F64Ceil", argLength: 1, reg: fp64_11, typ: "Float64"},         // ceil(arg0)
		{name: "F64Floor", asm: "F64Floor", argLength: 1, reg: fp64_11, typ: "Float64"},       // floor(arg0)
		{name: "F64Nearest", asm: "F64Nearest", argLength: 1, reg: fp64_11, typ: "Float64"},   // round(arg0)
		{name: "F64Abs", asm: "F64Abs", argLength: 1, reg: fp64_11, typ: "Float64"},           // abs(arg0)
		{name: "F64Copysign", asm: "F64Copysign", argLength: 2, reg: fp64_21, typ: "Float64"}, // copysign(arg0, arg1)

		{name: "I64Ctz", asm: "I64Ctz", argLength: 1, reg: gp11, typ: "Int64"},       // ctz(arg0)
		{name: "I64Clz", asm: "I64Clz", argLength: 1, reg: gp11, typ: "Int64"},       // clz(arg0)
		{name: "I32Rotl", asm: "I32Rotl", argLength: 2, reg: gp21, typ: "Int32"},     // rotl(arg0, arg1)
		{name: "I64Rotl", asm: "I64Rotl", argLength: 2, reg: gp21, typ: "Int64"},     // rotl(arg0, arg1)
		{name: "I64Popcnt", asm: "I64Popcnt", argLength: 1, reg: gp11, typ: "Int64"}, // popcnt(arg0)

		// WebAssembly 3.0 garbage-collection ops. These implement the
		// design-doc §6 object model: Go heap objects become host-GC
		// structs and arrays instead of linear-memory blocks. Aux carries
		// the *types.Type whose wasm type index the obj backend resolves;
		// for the field accessors AuxInt carries the field index. They are
		// defined here for the M2 cutover but are not yet produced by any
		// rule in Wasm3.rules — the lowering that emits them lands in a
		// later M2 commit. See doc/wasm3-m2-design.md §3, §5.
		{name: "StructNew", argLength: -1, reg: gp01, aux: "Typ"},                                             // struct.new $Aux; allocates a struct of wasm type Aux with fields taken from args
		{name: "StructNewDefault", argLength: 0, reg: gp01, aux: "Typ"},                                       // struct.new_default $Aux; allocates a zeroed struct of wasm type Aux
		{name: "StructGet", argLength: 1, reg: gp11, aux: "TypInt"},                                              // struct.get $Aux AuxInt; reads field AuxInt of struct arg0
		{name: "StructSet", argLength: 2, reg: regInfo{inputs: []regMask{gp, gp}}, aux: "TypInt", typ: "Mem"},    // struct.set $Aux AuxInt; arg0=struct, arg1=value

		// Field access by Go byte offset (doc/wasm3-slice-boxing.md field-access ABI).
		// aux = the Go struct *types.Type, auxint = the field's BYTE OFFSET; codegen
		// resolves the offset to a WasmGC field index via the collector layout and
		// emits struct.get/struct.set. This is how Load/Store of a struct field lower
		// to WasmGC instead of linear I64Load/I64Store. arg0=struct ref.
		{name: "FieldGet", argLength: 2, reg: gp11, aux: "TypInt"},                                            // struct.get; arg0=struct, arg1=mem (order vs FieldSet)
		{name: "FieldSet", argLength: 3, reg: regInfo{inputs: []regMask{gp, gp}}, aux: "TypInt", typ: "Mem"}, // struct.set; arg0=struct, arg1=value, arg2=mem

		// Box-cell deref (doc/wasm3-addressable-boxed-locals). A pointer to a
		// boxed type (*string/*slice/*interface) is a reference to a one-field
		// go.box.<T> cell holding the boxed ref (collectBox; pointerStorage in
		// wasmtype.go uses the same cell). BoxLoad/BoxStore deref it via
		// ref.cast (ref go.box.T) + struct.get/struct.set field 0. aux = the
		// boxed (pointee) Go *types.Type T, registered through collectBox by
		// wasm3RegisterStruct. arg0 = the cell ref.
		{name: "BoxLoad", argLength: 2, reg: gp11, aux: "Typ"},                                            // struct.get field 0; arg0=cell, arg1=mem
		{name: "BoxStore", argLength: 3, reg: regInfo{inputs: []regMask{gp, gp}}, aux: "Typ", typ: "Mem"}, // struct.set field 0; arg0=cell, arg1=value, arg2=mem
		{name: "BoxNewDefault", argLength: 0, reg: gp01, aux: "Typ"},                                      // struct.new_default $go.box.T; allocates a zeroed one-field cell for boxed type Aux

		// Boxed slice header accessors (doc/wasm3-slice-boxing.md). A slice is a
		// single (ref $go.slice.<T>) struct; these read its fields at fixed
		// indices. aux=slice *types.Type (resolved to the header type via
		// wasm3RegisterSliceStruct). SliceData reads field 0 (backing array ref,
		// anyref); SliceLength field 2; SliceCapacity field 3.
		{name: "SliceData", argLength: 1, reg: gp11, aux: "Typ", typ: "BytePtr"},   // struct.get $go.slice.<T> 0
		{name: "SliceLength", argLength: 1, reg: gp11, aux: "Typ", typ: "Int64"},   // struct.get $go.slice.<T> 2
		{name: "SliceCapacity", argLength: 1, reg: gp11, aux: "Typ", typ: "Int64"}, // struct.get $go.slice.<T> 3

		// Boxed string ($go.string) component reads (doc/wasm3-slice-boxing.md).
		// StringData reads field 0 (backing $go.bytes ref, anyref); StringLength
		// field 2 (i64). Dedicated ops (not StructGet) so wasm3ValueType can
		// classify the ref vs i64 result. arg0=string ref.
		{name: "StringData", argLength: 1, reg: gp11, typ: "BytePtr"},   // struct.get $go.string 0
		{name: "StringLength", argLength: 1, reg: gp11, typ: "Int64"},   // struct.get $go.string 2

		// Boxed interface ($go.iface = {itab anyref, data anyref}) component
		// reads (doc/wasm3-slice-boxing.md). IfaceItab reads field 0 (the
		// type-descriptor/itab ref); IfaceData field 1 (the data ref). Both
		// anyref results — dedicated ops so wasm3ValueType classifies them
		// as anyref. arg0 = interface ref.
		{name: "IfaceItab", argLength: 1, reg: gp11, typ: "BytePtr"}, // struct.get $go.iface 0
		{name: "IfaceData", argLength: 1, reg: gp11, typ: "BytePtr"}, // struct.get $go.iface 1
		// IfaceMake builds an interface value: struct.new $go.iface
		// {itab, data}. arg0 = itab (a type descriptor — its opaque
		// identity ref via global.get/R_WASMDESCRIPTOR when it is a
		// descriptor symbol address, else an already-ref value), arg1 =
		// data (a ref). Result anyref.
		{name: "IfaceMake", argLength: 2, reg: gp21, typ: "BytePtr"}, // struct.new $go.iface

		{name: "ArrayNew", argLength: 2, reg: gp21, aux: "Typ"},                                               // array.new $Aux; arg0=element value, arg1=length
		{name: "ArrayNewDefault", argLength: 1, reg: gp11, aux: "Typ"},                                        // array.new_default $Aux; arg0=length
		{name: "ArrayGet", argLength: 3, reg: regInfo{inputs: []regMask{gp, gp}, outputs: []regMask{gp}}, aux: "Typ"}, // array.get $Aux; arg0=array, arg1=index, arg2=mem (ordering only — array elements are mutable, so reads must order against ArraySet writes)
		{name: "ArraySet", argLength: 4, reg: regInfo{inputs: []regMask{gp, gp, gp}}, aux: "Typ", typ: "Mem"}, // array.set $Aux; arg0=array, arg1=index, arg2=value, arg3=mem; returns mem
		{name: "ArrayLen", argLength: 1, reg: gp11, typ: "Int64"},                                             // array.len; arg0=array
		{name: "RefNull", argLength: 0, reg: gp01, aux: "Typ", rematerializeable: true},                       // ref.null $Aux
		{name: "RefIsNull", argLength: 1, reg: gp11, typ: "Bool"},                                             // ref.is_null; arg0=ref
		{name: "RefCast", argLength: 1, reg: gp11, aux: "Typ"},                                                // ref.cast (ref $Aux); arg0=ref
		{name: "RefTest", argLength: 1, reg: gp11, aux: "Typ", typ: "Bool"},                                   // ref.test (ref $Aux); arg0=ref

		// M3 Stage D: stack-allocated `var buf [N]T` autos lower to a
		// wasmgc `(ref (array T))` allocated at function entry, replacing
		// the SP-relative address SSA would otherwise produce via
		// OpLocalAddr. arg0 is the entry memory; v.Aux is the *ir.Name
		// of the auto (the Name's type is *[N]T). Returned value is
		// typed as a pointer in SSA but stored in an anyref local at
		// the wasm level. Later Wasm3.rules rewrite (Load (OffPtr [off]
		// (StackArray ...)) _) into Wasm3ArrayGet ops on this ref.
		{name: "StackArray", argLength: 1, reg: gp01, aux: "Sym", symEffect: "Addr"},

		// OpWasm3StackStruct is the struct analogue of StackArray: a
		// stack-allocated `var s T` (T a struct) lowers to a wasmgc
		// (ref $go.struct.T) allocated once at function entry, replacing
		// OpLocalAddr so field access and value-copy go through the boxed
		// ref. arg0 is the entry memory; v.Aux is the *ir.Name (its type
		// is *T). The zero value is built with struct.new_default (scalars
		// 0, refs null) then array fields are filled with a fresh
		// array.new_default (a nil slice/string/map/ptr field stays null,
		// which is the correct zero value).
		{name: "StackStruct", argLength: 1, reg: gp01, aux: "Sym", symEffect: "Addr"},

		// M3 Stage E phase 2: replacement for the bump-heap
		// runtime.makeslice call. Allocates a wasmgc `(ref (array T))`
		// backing of length cap via `array.new_default`. The result is
		// the slice's data pointer at the SSA level — Go-typed as
		// unsafe.Pointer (so the SSA layer treats it like a normal
		// pointer flowing into OpSliceMake), wasm-typed as anyref so the
		// per-value local holds the actual ref. v.Aux carries the
		// *types.Type of the slice's element (T); the obj backend
		// resolves it to a wasmgc array-backing type index via
		// wasm3RegisterArrayBacking. arg0=len, arg1=cap, arg2=mem.
		// Indexing on a Wasm3MakeSlice-derived OpSlicePtr lowers via
		// Wasm3.rules to ArrayGet / ArraySet on the ref.
		{name: "MakeSlice", argLength: 3, reg: gp21, aux: "Typ", typ: "BytePtr"},

		// M3 per-type maps: `make(map[K]V[, hint])` allocates a fresh
		// $go.map.<K,V> WasmGC struct with cap=0, used=0, keys=null,
		// values=null — lazy backing materialisation on first insert.
		// v.Aux carries the *types.Type of the map (so the obj backend
		// resolves it to a wasm $go.map.<K,V> type index via
		// wasm3RegisterMapStruct). The size hint is currently ignored
		// — the linear-seek impl grows on demand — so the op takes no
		// args (mirroring OpWasm3StructNewDefault). Result is the
		// freshly-allocated map ref, wasm-typed as anyref so the per-
		// value local holds the actual ref.
		{name: "MakeMap", argLength: 0, reg: gp01, aux: "Typ", typ: "BytePtr"},

		// M3 per-type maps: `clear(m)` resets a $go.map.<K,V> back to
		// empty — used=0, cap=0, keys=null, values=null. Nulling the
		// backings releases all key/value references for host-GC; the
		// next insert reallocates. arg0=map ref (anyref), arg1=mem.
		// v.Aux carries the map's *types.Type for wasm3RegisterMapStruct.
		{name: "MapClear", argLength: 2, reg: regInfo{inputs: []regMask{gp}}, aux: "Typ", typ: "Mem"},

		// M3 Stage E phase 3: sub-slicing `s[lo:hi:cap]` on a wasmgc-
		// backed slice. The wasm3 backend cannot do pointer arithmetic
		// on the backing ref, so the standard `rptr = ptr + lo*stride`
		// path that ssagen.slice() emits would generate an invalid
		// `i64.add anyref i64`. This op instead allocates a fresh
		// backing of `cap` elements and array.copies `len` elements
		// from orig_backing[lo..lo+len] into new[0..len]. The result
		// is the new backing ref, sized correctly for both indexing
		// and append-within-cap (the trailing cap-len slots stay zero
		// from array.new_default).
		//
		// arg0=orig_backing (anyref), arg1=lo (i32), arg2=len (i32),
		// arg3=cap (i32), arg4=mem. v.Aux is the slice's *types.Type
		// — wasm3RegisterArrayAux resolves the elem backing index.
		//
		// SEMANTIC DIFFERENCE: writes to the sub-slice do not
		// propagate to the parent, because the backings are
		// physically distinct. The design-doc's (ref backing, off,
		// len, cap) header would share backing and adjust offset
		// instead; that's a deeper change blocked on an SSA-level
		// slice representation rework. Documented in
		// doc/wasm3-m3-notes.md "Stage E phase 3 — sub-slicing".
		{name: "SubSlice", argLength: 5, reg: regInfo{inputs: []regMask{gp, gp, gp, gp}, outputs: []regMask{gp}}, aux: "Typ", typ: "BytePtr"},

		// M3 Stage E phase 4: copy() builtin lowered via array.copy
		// instead of runtime.memmove. arg0=dst (anyref), arg1=src
		// (anyref), arg2=n (i64 element count), arg3=mem. v.Aux is
		// the slice's *types.Type so wasm3RegisterArrayAux resolves
		// the elem backing index. Result is Mem (void op).
		//
		// walkCopy on wasm3 emits `runtime.wasm3SliceCopy(et, dst,
		// src, n)` instead of `runtime.memmove(dst, src, n_bytes)`;
		// the SSA intrinsic for wasm3SliceCopy lifts to this op.
		// Renamed-runtime-symbol avoids the inlining-clone-key
		// pitfall the direct-memmove-intrinsic attempt hit.
		{name: "ArrayCopy", argLength: 4, reg: regInfo{inputs: []regMask{gp, gp, gp}}, aux: "Typ", typ: "Mem"},

		// M3 Stage G: materialise a function value as `(ref
		// $go.closure.<sig>)`. v.Aux is the function's own *obj.LSym
		// (the symbol the `ref.func` opcode references, not the
		// closure-data linksym StaticData FuncLinksym produces). v.Type
		// is the Go *func(...) type so the closure-context typeidx is
		// derivable at codegen time via wasm3RegisterClosureCtxAux.
		// Codegen emits `ref.func $sym; i64.const 0; struct.new
		// $closureCtx`; the second field is the captures pointer,
		// nil for bare top-level functions. Result lands in an
		// anyref-typed per-value local.
		{name: "FuncValue", argLength: 0, reg: gp01, aux: "Sym", symEffect: "Addr", rematerializeable: true, typ: "BytePtr"},

		// Pointer-representation cutover (doc/wasm3-pointer-cutover):
		// read/write a package-level variable of a boxed (WasmGC-ref)
		// type, which lives in a wasm mutable ref-global rather than
		// linear-memory static data. v.Aux is the variable's *obj.LSym.
		// GlobalGet reads it (global.get); GlobalSet writes it
		// (global.set, arg0=value, arg1=mem). Codegen emits the
		// instruction carrying an R_WASMGLOBAL reloc the linker resolves
		// to the variable's allocated wasm global index. The result of
		// GlobalGet lands in an anyref per-value local.
		{name: "GlobalGet", argLength: 0, reg: gp01, aux: "Sym", symEffect: "Read"},
		{name: "GlobalSet", argLength: 2, reg: regInfo{inputs: []regMask{gp}}, aux: "Sym", symEffect: "Write", typ: "Mem"},

		// M3 Stage G closures: materialise a closure value as `(ref
		// $go.closure.<sig>)` wrapping a linear-memory captures
		// struct. v.Aux is the synthetic function's *obj.LSym; arg0
		// is the i64 captures pointer (the address of the &struct{F,
		// X0, ...} layout walkClosure constructs and SSA pre-
		// populates via field stores). v.Type is the user's *func or
		// func type, used to derive the closureCtx index. Codegen
		// emits `ref.func $sym; getValue captures; struct.new
		// $closureCtx`. The 2-field $closureCtx (funcref + captures-
		// ptr) lets the indirect-call site stash the captures-ptr in
		// the wasm3 CTXT global so the body's CTXT-relative load
		// path keeps working.
		{name: "MakeClosureRef", argLength: 1, reg: gp11, aux: "Sym", symEffect: "Addr", typ: "BytePtr"},

		// M3 Stage G captures-in-closureCtx (doc/wasm3-m3-captures-
		// in-struct.md): materialise a closure as a per-closure
		// `(ref $go.closure.<funcsym>)` subtype, with the captures
		// stored *inline* in the wasmgc struct rather than on the
		// linear-memory bump heap. v.Aux is the closure body's
		// *obj.LSym (also keys the per-closure wasmgc type); v.Type
		// is the user's *func/func type for closureCtx-index
		// derivation. Args are the captured values themselves, in
		// ClosureVars order — one anyref/i64/etc. per capture, no
		// captures-ptr indirection. Codegen emits `ref.func $sym;
		// <captures...>; struct.new $closureCtx_<funcsym>`. Replaces
		// the MakeClosureRef + heap-alloc + runtime.wasm3WrapClosure
		// pattern for closures whose bodies have been migrated to
		// the new prologue (LoweredGetClosureRef + GetClosureField);
		// MakeClosureRef stays for the still-CTXT-i64 method-value
		// path.
		{name: "MakeClosureRefInline", argLength: -1, reg: regInfo{outputs: []regMask{gp}}, aux: "Sym", symEffect: "Addr", typ: "BytePtr"},

		// LoweredGetClosureRef: read the CTXT_REF anyref global
		// (module global 2), the closure-ref the indirect-call site
		// stored before call_ref. Used by closure-body prologues
		// migrated to the captures-in-struct scheme to recover the
		// concrete closureCtx value for subsequent ref.cast +
		// struct.get on captures. rematerializeable so the prologue
		// emits exactly one read and downstream uses see it via the
		// per-value local.
		{name: "LoweredGetClosureRef", reg: gp01, rematerializeable: true, typ: "BytePtr"},

		// LoweredCastClosureRef: cast a plain anyref (typically from
		// LoweredGetClosureRef) down to a concrete `(ref $go.closure.
		// <funcsym>)`. v.Aux is the closure body's *obj.LSym, used by
		// the obj-encoder to look up the per-closure closureCtx type
		// index for the ref.cast operand. arg0 is the anyref value.
		// Output is an anyref-shaped local that subsequent
		// GetClosureField ops read from.
		{name: "LoweredCastClosureRef", argLength: 1, reg: gp11, aux: "Sym", symEffect: "Addr", typ: "BytePtr"},

		// GetClosureField: read a capture from a typed closure-ref
		// produced by LoweredCastClosureRef. v.Aux is the closure
		// body's *obj.LSym (for type-index lookup); v.AuxInt is the
		// 0-based field index *within the closure's capture list*
		// (field 0 in the wasmgc struct is the funcref, so the
		// encoder adds 1 when emitting `struct.get $closureCtx
		// <AuxInt+1>`). arg0 is the typed closure-ref. v.Type drives
		// the per-value local shape — scalar captures land in an
		// i64 local, reference captures in an anyref local, matching
		// the wasm3ValueType dispatch.
		{name: "GetClosureField", argLength: 1, reg: gp11, aux: "SymOff", symEffect: "Addr"},

		// Fat-pointer (interior-pointer) primitives. See
		// doc/wasm3-fat-pointers-design.md.
		//
		// A fat pointer is a `(container ref, offset i32)` pair,
		// materialised as a wasmgc `(struct (ref any) i32)` whose
		// wrapper type is one of the prelude `$go.iptr.<class>` entries
		// (TypeGoIptrI8 .. TypeGoIptrRef). The container ref is anyref
		// at storage time; uses ref.cast back to the concrete container
		// type before reading or writing via struct.get/set or
		// array.get/set.
		//
		// Aux on each op is the *types.Type of the pointee (a Go scalar
		// or composite). The obj backend reads it to pick the right
		// wrapper type (TypeGoIptr* index) and the right wasm op for
		// load/store width. Multi-field pointees (string, slice,
		// interface) lower their load to a tuple of `array.get` /
		// `struct.get` reads and rely on OpSelectN downstream for
		// decomposition — same shape OpStringMake already uses as a
		// tuple producer.

		// OpWasm3InteriorPtr materialises a fat pointer. arg0 is the
		// container ref (anyref-typed at the SSA level); arg1 is the
		// i32 offset. Result is the fat-pointer wasmgc ref.
		// Emits: `<arg0>; <arg1>; struct.new $go.iptr.<class>`.
		// Result lands in an anyref per-value local.
		{name: "InteriorPtr", argLength: 2, reg: gp21, aux: "Typ", typ: "BytePtr"},

		// OpWasm3ArrayElemRef materialises &arr[i] for an array/slice whose
		// element is a struct (a boxed ref in the (array (ref box.E))
		// backing). arg0 is the container ref (anyref), arg1 is the i64
		// element index. Result is the element's boxed ref, which IS the
		// *E interior pointer in the boxed model — so a following field
		// access folds to FieldGet on it. Emits: ref.cast (ref array);
		// i32.wrap idx; array.get -> element ref. v.Aux is the array/slice
		// Go *types.Type (its elem keys the backing). Distinct from
		// OpWasm3InteriorPtr (scalar element, deref'd via LoadInterior) and
		// OpWasm3ArrayGet (returns a scalar element value, not a ref).
		{name: "ArrayElemRef", argLength: 2, reg: gp21, aux: "Typ", typ: "BytePtr"},

		// OpWasm3Clone deep-copies a boxed composite value for Go value
		// semantics (b := a / s.field = arr must copy, not alias). arg0 is
		// the source ref; v.Aux is the Go composite type. Result is a fresh
		// independent ref. Array: array.new_default + array.copy (scalar/
		// ref elements; validated in doc/wasm3-array-valuecopy-derisk.wat).
		// Struct: struct.new with each field cloned (scalars copied,
		// composite fields recursively cloned). Pure (no mem).
		{name: "Clone", argLength: 1, reg: gp11, aux: "Typ", typ: "BytePtr"},

		// OpWasm3ArrayCopyInto copies the elements of one array into
		// another in place (b := a where b is a pre-allocated local array
		// ref / StackArray, which cannot be reassigned). arg0 = dst array
		// ref, arg1 = src array ref, arg2 = mem. v.Aux is the Go array
		// type. Emits array.copy $arr $arr (dst,0,src,0,len). Returns mem.
		{name: "ArrayCopyInto", argLength: 3, reg: regInfo{inputs: []regMask{gp, gp}}, aux: "Typ", typ: "Mem"},

		// OpWasm3LoadInterior reads through a fat pointer. arg0 is the
		// fat-pointer ref. v.Aux is the pointee Go type so the backend
		// derives the wrapper type and the read width. v.Type drives
		// the per-value local shape (anyref for ref pointees, i64 for
		// scalar pointees). For multi-component pointees (string,
		// interface, slice) the op is tuple-producing — OpSelectN
		// consumers extract individual fields.
		//
		// Emits: ref.cast (ref $go.iptr.<class>); struct.get
		// $go.iptr.<class> 0; ref.cast (ref $containerType);
		// struct.get $go.iptr.<class> 1; { array.get_u | struct.get }
		// $containerType.
		{name: "LoadInterior", argLength: 2, reg: gp11, aux: "Typ"},

		// OpWasm3StoreInterior writes through a fat pointer. arg0 is
		// the fat-pointer ref; arg1..argN are the value(s) to write
		// (one for scalar pointees, multiple for multi-component);
		// argN+1 is mem. v.Aux is the pointee Go type. Returns mem.
		//
		// Emits the dual of LoadInterior: ref.cast + struct.get 0/1
		// + array.set/struct.set on the recovered container.
		{name: "StoreInterior", argLength: -1, reg: regInfo{inputs: []regMask{gp, gp, gp}}, aux: "Typ", typ: "Mem"},

		// Accessor-pair interior pointer for a struct field (the i64
		// pointee class), the boundary-crossing fat pointer of
		// doc/wasm3-pointer-cutover (de-risked in
		// doc/wasm3-fat-pointer-derisk.wat). Unlike the array-based
		// InteriorPtr above, $go.ptr.i64 carries get/set FUNCREFs so a
		// generic callee can dereference it via call_ref without the
		// container's static type.

		// OpWasm3MakeFieldPtr materialises $go.ptr.i64 for &container.field.
		// arg0 is the container ref (anyref); v.Aux is the container's
		// struct *types.Type and v.AuxInt the field byte offset (TypInt).
		// Codegen: <arg0>; i32.const 0 (offset unused for a struct field —
		// the field index is static in the accessors); ref.func
		// $get_T_field; ref.func $set_T_field; struct.new $go.ptr.i64.
		// The accessors are generated (in walk) by
		// reflectdata.WasmGCFieldGetter/Setter. Result lands in an anyref
		// per-value local.
		{name: "MakeFieldPtr", argLength: 1, reg: gp11, aux: "TypInt", typ: "BytePtr"},

		// OpWasm3PtrLoad reads through a $go.ptr.i64 (call_ref the getter).
		// arg0 is the fat-pointer ref; result is i64. Codegen reads the
		// base/offset/get fields and call_ref's $go.getter.i64.
		{name: "PtrLoad", argLength: 1, reg: gp11},

		// OpWasm3PtrStore writes through a $go.ptr.i64 (call_ref the
		// setter). arg0 = fat-pointer ref, arg1 = i64 value, arg2 = mem.
		// Returns mem.
		{name: "PtrStore", argLength: 3, reg: regInfo{inputs: []regMask{gp, gp}}, typ: "Mem"},
	}

	archs = append(archs, arch{
		name:     "Wasm3",
		pkg:      "cmd/internal/obj/wasm",
		genfile:  "../../wasm3/ssa.go",
		ops:      Wasm3Ops,
		blocks:   nil,
		regnames: regNamesWasm3,
		// M2 cutover, Stage C.2: unlike GOARCH=wasm — which has no
		// register parameters and passes everything through the Go
		// stack in linear memory — wasm3 functions are native typed
		// wasm functions whose parameters and results are wasm function
		// params/results. The SSA "registers" R0-R15 / F0-F31 are the
		// wasm locals the obj backend emits, so they double as the
		// parameter registers; arguments beyond them spill to the
		// frame. See doc/wasm3-m2-cutover-notes.md §5.
		ParamIntRegNames:   "R0 R1 R2 R3 R4 R5 R6 R7 R8 R9 R10 R11 R12 R13 R14 R15",
		ParamFloatRegNames: "F0 F1 F2 F3 F4 F5 F6 F7 F8 F9 F10 F11 F12 F13 F14 F15 F16 F17 F18 F19 F20 F21 F22 F23 F24 F25 F26 F27 F28 F29 F30 F31",
		gpregmask:          gp,
		fpregmask:          fp32.union(fp64),
		fp32regmask:        fp32,
		fp64regmask:        fp64,
		framepointerreg:    -1, // not used
		linkreg:            -1, // not used
	})
}
