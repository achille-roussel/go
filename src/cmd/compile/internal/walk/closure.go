// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package walk

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/ssa"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"internal/buildcfg"
	"strconv"
)

// directClosureCall rewrites a direct call of a function literal into
// a normal function call with closure variables passed as arguments.
// This avoids allocation of a closure object.
//
// For illustration, the following call:
//
//	func(a int) {
//		println(byval)
//		byref++
//	}(42)
//
// becomes:
//
//	func(byval int, &byref *int, a int) {
//		println(byval)
//		(*&byref)++
//	}(byval, &byref, 42)
func directClosureCall(n *ir.CallExpr) {
	clo := n.Fun.(*ir.ClosureExpr)
	clofn := clo.Func

	if !clofn.IsClosure() {
		return // leave for walkClosure to handle
	}

	// We are going to insert captured variables before input args.
	var params []*types.Field
	var decls []*ir.Name
	for _, v := range clofn.ClosureVars {
		if !v.Byval() {
			// If v of type T is captured by reference,
			// we introduce function param &v *T
			// and v remains PAUTOHEAP with &v heapaddr
			// (accesses will implicitly deref &v).

			addr := ir.NewNameAt(clofn.Pos(), typecheck.Lookup("&"+v.Sym().Name), types.NewPtr(v.Type()))
			addr.Curfn = clofn
			v.Heapaddr = addr
			v = addr
		}

		v.Class = ir.PPARAM
		decls = append(decls, v)

		fld := types.NewField(src.NoXPos, v.Sym(), v.Type())
		fld.Nname = v
		params = append(params, fld)
	}

	// f is ONAME of the actual function.
	f := clofn.Nname
	typ := f.Type()

	// Create new function type with parameters prepended, and
	// then update type and declarations.
	typ = types.NewSignature(nil, append(params, typ.Params()...), typ.Results())
	f.SetType(typ)
	clofn.Dcl = append(decls, clofn.Dcl...)

	// Rewrite call.
	n.Fun = f
	n.Args.Prepend(closureArgs(clo)...)

	// Update the call expression's type. We need to do this
	// because typecheck gave it the result type of the OCLOSURE
	// node, but we only rewrote the ONAME node's type. Logically,
	// they're the same, but the stack offsets probably changed.
	if typ.NumResults() == 1 {
		n.SetType(typ.Result(0).Type)
	} else {
		n.SetType(typ.ResultsTuple())
	}

	// Add to Closures for enqueueFunc. It's no longer a proper
	// closure, but we may have already skipped over it in the
	// functions list, so this just ensures it's compiled.
	ir.CurFunc.Closures = append(ir.CurFunc.Closures, clofn)
}

func walkClosure(clo *ir.ClosureExpr, init *ir.Nodes) ir.Node {
	clofn := clo.Func

	// If not a closure, don't bother wrapping.
	if !clofn.IsClosure() {
		if base.Debug.Closure > 0 {
			base.WarnfAt(clo.Pos(), "closure converted to global")
		}
		return clofn.Nname
	}

	// The closure is not trivial or directly called, so it's going to stay a closure.
	ir.ClosureDebugRuntimeCheck(clo)
	clofn.SetNeedctxt(true)

	// The closure expression may be walked more than once if it appeared in composite
	// literal initialization (e.g, see issue #49029).
	//
	// Don't add the closure function to compilation queue more than once, since when
	// compiling a function twice would lead to an ICE.
	if !clofn.Walked() {
		clofn.SetWalked(true)
		ir.CurFunc.Closures = append(ir.CurFunc.Closures, clofn)
	}

	typ := typecheck.ClosureType(clo)

	clos := ir.NewCompLitExpr(base.Pos, ir.OCOMPLIT, typ, nil)
	clos.SetEsc(clo.Esc())
	clos.List = append([]ir.Node{ir.NewUnaryExpr(base.Pos, ir.OCFUNC, clofn.Nname)}, closureArgs(clo)...)
	for i, value := range clos.List {
		clos.List[i] = ir.NewStructKeyExpr(base.Pos, typ.Field(i), value)
	}

	addr := typecheck.NodAddr(clos)
	addr.SetEsc(clo.Esc())

	// non-escaping temp to use, if any.
	// Stage G: skip on wasm3 — the Prealloc temp is an SP-relative
	// auto slot in the linear-memory frame, which wasm3 doesn't
	// model (it has no Go stack frame; autos go to per-function wasm
	// locals or to the bump heap). Forcing escape sends the closure
	// captures struct through runtime.newobject onto the bump heap,
	// where wasm3WrapClosure can wrap the i64 pointer it returns
	// into the wasmgc closureCtx.
	if x := clo.Prealloc; x != nil && buildcfg.GOARCH != "wasm3" {
		if !types.Identical(typ, x.Type()) {
			panic("closure type does not match order's assigned type")
		}
		addr.Prealloc = x
		clo.Prealloc = nil
	}

	if buildcfg.GOARCH == "wasm3" {
		// doc/wasm3-m3-captures-in-struct.md: closures matching the
		// scalar-by-value predicate go through
		// wasm3MakeClosureInlineN intrinsics, which lower to
		// OpWasm3MakeClosureRefInline — captures land directly in
		// the wasmgc closureCtx subtype with no linear-memory
		// captures-struct allocation. The body-side prologue at
		// ssagen/ssa.go uses the *matching* predicate so the
		// closureCtx shape is consistent on both sides. Today only
		// the 1-capture form is wired; multi-capture closures fall
		// through to the legacy wasm3WrapClosure path.
		if n := len(clofn.ClosureVars); n >= 1 && n <= 4 && wasm3AllScalarByVal(clofn.ClosureVars) {
			fnName := "wasm3MakeClosureInline" + strconv.Itoa(n)
			fn := typecheck.LookupRuntime(fnName)
			closureType := reflectdata.TypePtrAt(base.Pos, clo.Type())
			fnSym := ir.NewUnaryExpr(base.Pos, ir.OCFUNC, clofn.Nname)
			fnSym.SetType(types.Types[types.TUINTPTR])
			fnSym = typecheck.Expr(fnSym).(*ir.UnaryExpr)
			args := make([]ir.Node, 0, 2+n)
			args = append(args, closureType, fnSym)
			for _, cv := range clofn.ClosureVars {
				args = append(args, wasm3CaptureAsUintptr(cv.Outer))
			}
			call := typecheck.Call(base.Pos, fn, args, false).(*ir.CallExpr)
			call.SetType(clo.Type())
			return walkExpr(call, init)
		}

		// Stage G (legacy): wrap the linear-memory captures struct
		// in a wasmgc `(ref $go.closure.<sig>)`. The compiler
		// intrinsifies runtime.wasm3WrapClosure at the SSA layer
		// into OpWasm3MakeClosureRef, which emits `ref.func $sym;
		// getValue captures; struct.new $closureCtx` and produces
		// an anyref-typed value. ConvNop the unsafe.Pointer return
		// to the user's closure func type at the wasm signature
		// boundary; SSA sees both as i64-shaped wires at this
		// stage, but the wasm3 backend's per-value local for the
		// result is anyref (per wasm3ValueType
		// OpWasm3MakeClosureRef case).
		fn := typecheck.LookupRuntime("wasm3WrapClosure")
		closureType := reflectdata.TypePtrAt(base.Pos, clo.Type())
		fnSym := ir.NewUnaryExpr(base.Pos, ir.OCFUNC, clofn.Nname)
		fnSym.SetType(types.Types[types.TUINTPTR])
		fnSym = typecheck.Expr(fnSym).(*ir.UnaryExpr)
		captures := typecheck.ConvNop(addr, types.Types[types.TUNSAFEPTR])
		call := typecheck.Call(base.Pos, fn, []ir.Node{closureType, fnSym, captures}, false).(*ir.CallExpr)
		// Force the call expression's type to the closure func type
		// so the SSA intrinsic (which fires on this OCALLFUNC) can
		// read n.Type() to recover the func type for the
		// OpWasm3MakeClosureRef's v.Type. The actual runtime decl
		// returns unsafe.Pointer; this override is a lie that holds
		// because the intrinsic short-circuits the call before any
		// runtime body or call ABI is observed.
		call.SetType(clo.Type())
		return walkExpr(call, init)
	}

	// Force type conversion from *struct to the func type.
	cfn := typecheck.ConvNop(addr, clo.Type())

	return walkExpr(cfn, init)
}

// wasm3ScalarByValClosureVar mirrors the
// wasm3ClosureUsesCapturesInStruct predicate from ssagen: a single
// ClosureVar fits the captures-in-struct path only if it's a small
// by-value scalar with no addr-taken aliasing.
//
// Integer captures only for now. Pointer captures lower to a
// `(ref $T)` field in the per-closure closureCtx struct, but the
// body's code that *uses* the captured pointer still assumes
// linear-memory i64 pointers (it emits `i32.wrap + load offset`
// for field access, not `struct.get` on a typed ref) — running
// such a body trips wasm validation with "expected i64, found
// anyref". Until the body-side pointer-deref lowering is extended
// to recognise wasmgc-shaped pointer locals, only integer captures
// stay on the new path; pointers fall back to the legacy
// wasm3WrapClosure heap-captures route.
func wasm3ScalarByValClosureVar(v *ir.Name) bool {
	if !v.Byval() || v.Addrtaken() {
		return false
	}
	if !ssa.CanSSA(v.Type()) {
		return false
	}
	return v.Type().IsInteger()
}

// wasm3AllScalarByVal is wasm3ScalarByValClosureVar lifted over a
// ClosureVars list.
func wasm3AllScalarByVal(vars []*ir.Name) bool {
	for _, v := range vars {
		if !wasm3ScalarByValClosureVar(v) {
			return false
		}
	}
	return true
}

// wasm3CaptureAsUintptr converts an arbitrary integer- or pointer-
// shaped capture expression to a uintptr value suitable for the
// wasm3MakeClosureInline1 runtime intrinsic. Integers go through a
// single typecheck.Conv; pointer-shaped captures (TPTR, TUNSAFEPTR)
// go through unsafe.Pointer first to mirror Go's
// `uintptr(unsafe.Pointer(p))` idiom (a direct *T -> uintptr Conv
// fails the typechecker's no-sign-mismatch rule).
func wasm3CaptureAsUintptr(captured ir.Node) ir.Node {
	t := captured.Type()
	if t.IsInteger() {
		return typecheck.Conv(captured, types.Types[types.TUINTPTR])
	}
	via := typecheck.ConvNop(captured, types.Types[types.TUNSAFEPTR])
	return typecheck.Conv(via, types.Types[types.TUINTPTR])
}

// closureArgs returns a slice of expressions that can be used to
// initialize the given closure's free variables. These correspond
// one-to-one with the variables in clo.Func.ClosureVars, and will be
// either an ONAME node (if the variable is captured by value) or an
// OADDR-of-ONAME node (if not).
func closureArgs(clo *ir.ClosureExpr) []ir.Node {
	fn := clo.Func

	args := make([]ir.Node, len(fn.ClosureVars))
	for i, v := range fn.ClosureVars {
		var outer ir.Node
		outer = v.Outer
		if !v.Byval() {
			outer = typecheck.NodAddrAt(fn.Pos(), outer)
		}
		args[i] = typecheck.Expr(outer)
	}
	return args
}

func walkMethodValue(n *ir.SelectorExpr, init *ir.Nodes) ir.Node {
	// Create closure in the form of a composite literal.
	// For x.M with receiver (x) type T, the generated code looks like:
	//
	//	clos = &struct{F uintptr; R T}{T.M·f, x}
	//
	// Like walkClosure above.

	if n.X.Type().IsInterface() {
		// Trigger panic for method on nil interface now.
		// Otherwise it happens in the wrapper and is confusing.
		n.X = cheapExpr(n.X, init)
		n.X = walkExpr(n.X, nil)

		tab := ir.NewUnaryExpr(base.Pos, ir.OITAB, n.X)
		check := ir.NewUnaryExpr(base.Pos, ir.OCHECKNIL, tab)
		init.Append(typecheck.Stmt(check))
	}

	typ := typecheck.MethodValueType(n)

	clos := ir.NewCompLitExpr(base.Pos, ir.OCOMPLIT, typ, nil)
	clos.SetEsc(n.Esc())
	clos.List = []ir.Node{ir.NewUnaryExpr(base.Pos, ir.OCFUNC, methodValueWrapper(n)), n.X}

	addr := typecheck.NodAddr(clos)
	addr.SetEsc(n.Esc())
	// Stage G: skip on wasm3 — same reasoning as walkClosure: the
	// Prealloc would land in the linear-memory SP frame, which the
	// wasm3 ABI can't pass as the anyref-shaped funcvalue/closureCtx
	// arg downstream callers now expect. Forcing the {F,R} struct
	// through runtime.newobject (Esc=EscHeap) puts it on the bump
	// heap; wasm3WrapClosure then wraps the i64 captures-ptr in the
	// wasmgc `(ref $closureCtx)`. The method wrapper (-fm) reads R
	// off CTXT+8 via an ordinary linear-memory load, which is just a
	// (Get CTXT; I64Load $8) the encoder already handles.
	if buildcfg.GOARCH == "wasm3" {
		addr.SetEsc(ir.EscHeap)
	} else if x := n.Prealloc; x != nil {
		if !types.Identical(typ, x.Type()) {
			panic("partial call type does not match order's assigned type")
		}
		addr.Prealloc = x
		n.Prealloc = nil
	}

	if buildcfg.GOARCH == "wasm3" {
		// Stage G: mirror walkClosure's wasm3 path. The method value
		// `&{F, R}` is allocated on the bump heap via ONEW (forced by
		// the Prealloc bypass above), then wrapped in a wasmgc
		// closureCtx by wasm3WrapClosure. CTXT at the call site is
		// the i64 captures-ptr to the {F, R} struct, which the -fm
		// wrapper dereferences off offset 8 to recover the receiver.
		fn := typecheck.LookupRuntime("wasm3WrapClosure")
		closureType := reflectdata.TypePtrAt(base.Pos, n.Type())
		fnSym := ir.NewUnaryExpr(base.Pos, ir.OCFUNC, methodValueWrapper(n))
		fnSym.SetType(types.Types[types.TUINTPTR])
		fnSym = typecheck.Expr(fnSym).(*ir.UnaryExpr)
		captures := typecheck.ConvNop(addr, types.Types[types.TUNSAFEPTR])
		call := typecheck.Call(base.Pos, fn, []ir.Node{closureType, fnSym, captures}, false).(*ir.CallExpr)
		call.SetType(n.Type())
		return walkExpr(call, init)
	}

	// Force type conversion from *struct to the func type.
	cfn := typecheck.ConvNop(addr, n.Type())

	return walkExpr(cfn, init)
}

// methodValueWrapper returns the ONAME node representing the
// wrapper function (*-fm) needed for the given method value. If the
// wrapper function hasn't already been created yet, it's created and
// added to typecheck.Target.Decls.
func methodValueWrapper(dot *ir.SelectorExpr) *ir.Name {
	if dot.Op() != ir.OMETHVALUE {
		base.Fatalf("methodValueWrapper: unexpected %v (%v)", dot, dot.Op())
	}

	meth := dot.Sel
	rcvrtype := dot.X.Type()
	sym := ir.MethodSymSuffix(rcvrtype, meth, "-fm")

	if sym.Uniq() {
		return sym.Def.(*ir.Name)
	}
	sym.SetUniq(true)

	base.FatalfAt(dot.Pos(), "missing wrapper for %v", meth)
	panic("unreachable")
}
