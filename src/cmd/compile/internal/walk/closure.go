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
	"cmd/internal/obj/wasm"
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
		if n := len(clofn.ClosureVars); n >= 1 && n <= 8 && wasm3AllScalarByVal(clofn.ClosureVars) {
			// Pick an intrinsic by the shape group of all
			// captures. All same-precision floats go through
			// the F32 / F64 multi-arity intrinsics; all integers
			// or pointers go through the uintptr-arg InlineN.
			// Mixed shapes (e.g. one int + one float) have no
			// intrinsic and fall through to legacy.
			shape := wasm3CaptureShape(clofn.ClosureVars)
			if shape != wasm3ShapeMixed {
				fnName, ok := wasm3InlineIntrinsicName(n, shape)
				if !ok {
					goto wasm3LegacyClosure
				}
				fn := typecheck.LookupRuntime(fnName)
				closureType := reflectdata.TypePtrAt(base.Pos, clo.Type())
				fnSym := ir.NewUnaryExpr(base.Pos, ir.OCFUNC, clofn.Nname)
				fnSym.SetType(types.Types[types.TUINTPTR])
				fnSym = typecheck.Expr(fnSym).(*ir.UnaryExpr)
				args := make([]ir.Node, 0, 2+n)
				args = append(args, closureType, fnSym)
				for _, cv := range clofn.ClosureVars {
					args = append(args, wasm3CaptureArg(cv.Outer, shape))
				}
				call := typecheck.Call(base.Pos, fn, args, false).(*ir.CallExpr)
				call.SetType(clo.Type())
				return walkExpr(call, init)
			}
		}
		// Single-composite-capture closure. The composite-captures
		// extension of captures-in-struct: rather than store the
		// capture as N i64 fields in a runtime.newobject linear-
		// memory block (which would i64.store the anyref backing
		// pointer — invalid wasm), put each component into the
		// wasmgc closureCtx subtype with its proper wasm type
		// (anyref for the backing, i64 for len / cap). The body's
		// GetClosureField loop reads them back via per-component
		// struct.gets + Slice/StringMake. Predicate: exactly one
		// ClosureVar, by-value, not addr-taken, slice- or string-
		// typed (or small-int-array via element-decomposition).
		if len(clofn.ClosureVars) == 1 {
			cv := clofn.ClosureVars[0]
			// Small integer-array captures decompose into N
			// scalar captures and reuse the existing
			// wasm3MakeClosureInlineN intrinsic family (N up to
			// 8). The body-side prologue rematerialises the
			// array from these N captures via OpWasm3StackArray
			// + per-element assign. This sidesteps the still-
			// open array-by-value call ABI: each element
			// crosses the call boundary as an i64, the whole
			// array never has to.
			if cv.Byval() && !cv.Addrtaken() && cv.Type().IsArray() &&
				cv.Type().Elem().IsInteger() &&
				cv.Type().NumElem() >= 1 && cv.Type().NumElem() <= 8 {
				n := int(cv.Type().NumElem())
				// Publish side channel so the body-side prologue
				// gets the per-closure-ctx shape from the [N]T
				// ClosureVar (matching the N i64 fields the
				// caller is about to push).
				wasm.Wasm3ClosureBodyCaptures.Store(clofn.Nname.Linksym(), &wasm.Wasm3ClosureBodyInfo{
					FuncType: clofn.Type(),
					Captures: []*types.Type{cv.Type()},
				})
				fnName, ok := wasm3InlineIntrinsicName(n, wasm3ShapeUintptr)
				if !ok {
					goto wasm3LegacyClosure
				}
				fn := typecheck.LookupRuntime(fnName)
				closureType := reflectdata.TypePtrAt(base.Pos, clo.Type())
				fnSym := ir.NewUnaryExpr(base.Pos, ir.OCFUNC, clofn.Nname)
				fnSym.SetType(types.Types[types.TUINTPTR])
				fnSym = typecheck.Expr(fnSym).(*ir.UnaryExpr)
				args := make([]ir.Node, 0, 2+n)
				args = append(args, closureType, fnSym)
				for i := 0; i < n; i++ {
					idxNode := ir.NewInt(base.Pos, int64(i))
					idxNode.SetType(types.Types[types.TINT])
					ix := ir.NewIndexExpr(base.Pos, cv.Outer, idxNode)
					ix.SetBounded(true)
					args = append(args, wasm3CaptureAsUintptr(typecheck.Expr(ix)))
				}
				call := typecheck.Call(base.Pos, fn, args, false).(*ir.CallExpr)
				call.SetType(clo.Type())
				return walkExpr(call, init)
			}
			if cv.Byval() && !cv.Addrtaken() && ssa.CanSSA(cv.Type()) {
				var intrinsicName string
				var typeArgs []*types.Type
				switch {
				case cv.Type().IsSlice():
					intrinsicName = "wasm3MakeClosureInlineSlice1"
					typeArgs = []*types.Type{cv.Type().Elem()}
				case cv.Type().IsString():
					intrinsicName = "wasm3MakeClosureInlineString1"
					typeArgs = nil
				}
				if intrinsicName != "" {
					// Publish closure body info to the side channel up-
					// front. Both the caller-side OpWasm3MakeClosureRefInline
					// codegen and the body-side OpWasm3LoweredCastClosureRef /
					// OpWasm3GetClosureField codegens read this to derive
					// the matching per-closure-ctx struct shape (Go-level
					// types so composite captures lower consistently on
					// both sides). The body-side prologue at ssagen also
					// publishes — they overwrite each other with the same
					// content, so racing is harmless.
					captureTypes := []*types.Type{cv.Type()}
					wasm.Wasm3ClosureBodyCaptures.Store(clofn.Nname.Linksym(), &wasm.Wasm3ClosureBodyInfo{
						FuncType: clofn.Type(),
						Captures: captureTypes,
					})
					fn := typecheck.LookupRuntime(intrinsicName, typeArgs...)
					closureType := reflectdata.TypePtrAt(base.Pos, clo.Type())
					fnSym := ir.NewUnaryExpr(base.Pos, ir.OCFUNC, clofn.Nname)
					fnSym.SetType(types.Types[types.TUINTPTR])
					fnSym = typecheck.Expr(fnSym).(*ir.UnaryExpr)
					call := typecheck.Call(base.Pos, fn, []ir.Node{closureType, fnSym, cv.Outer}, false).(*ir.CallExpr)
					call.SetType(clo.Type())
					return walkExpr(call, init)
				}
			}
		}

	wasm3LegacyClosure:

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
// Integers, pointers (TPTR, TUNSAFEPTR), and floats qualify.
// Pointer captures are stored as i64 (the linear-memory address-
// as-uintptr that wasm3CaptureAsUintptr produces). Float captures
// are pushed natively (the call site uses wasm3MakeClosureInline1F*
// intrinsics) — only in the 1-capture case, since per-shape multi-
// arity intrinsics aren't wired yet. Multi-capture float-bearing
// closures fall back to the legacy wasm3WrapClosure path.
func wasm3ScalarByValClosureVar(v *ir.Name) bool {
	if !v.Byval() || v.Addrtaken() {
		return false
	}
	if !ssa.CanSSA(v.Type()) {
		return false
	}
	t := v.Type()
	return t.IsInteger() || t.IsPtr() || t.IsUnsafePtr() || t.IsFloat()
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

// wasm3MethodValueFitsInline reports whether a method value with
// receiver `rcvr` can be routed through the captures-in-struct
// path. Receivers that lower to a single i64 storage (integer or
// pointer) qualify; composite (string, struct, slice) receivers
// fall back to the legacy heap-captures path so the multi-field
// closureCtx wiring isn't needed yet.
func wasm3MethodValueFitsInline(rcvr ir.Node) bool {
	t := rcvr.Type()
	return t.IsInteger() || t.IsPtr() || t.IsUnsafePtr()
}

// wasm3CaptureShape identifies which intrinsic family a closure's
// captures collectively map to. Mixed shapes (e.g. one int + one
// float) have no single-typed N-ary intrinsic in runtime.go and
// fall back to the legacy wasm3WrapClosure heap path.
type wasm3CapShape int

const (
	wasm3ShapeUintptr wasm3CapShape = iota // all int/ptr — uintptr-arg InlineN
	wasm3ShapeF32                          // all float32 — InlineN F32
	wasm3ShapeF64                          // all float64 — InlineN F64
	wasm3ShapeMixed                        // multi-shape, no intrinsic
)

func wasm3CaptureShape(vars []*ir.Name) wasm3CapShape {
	if len(vars) == 0 {
		return wasm3ShapeMixed
	}
	first := wasm3SlotShape(vars[0].Type())
	for _, v := range vars[1:] {
		if wasm3SlotShape(v.Type()) != first {
			return wasm3ShapeMixed
		}
	}
	return first
}

// wasm3SlotShape returns the per-capture shape for a Go type.
// Integers, pointers, and unsafe.Pointer all collapse to
// wasm3ShapeUintptr because walkClosure passes them as uintptr
// through wasm3CaptureAsUintptr. Floats split by precision.
// Anything else returns wasm3ShapeMixed as a sentinel — the
// caller's predicate has already rejected such captures.
func wasm3SlotShape(t *types.Type) wasm3CapShape {
	switch {
	case t.IsInteger(), t.IsPtr(), t.IsUnsafePtr():
		return wasm3ShapeUintptr
	case t.IsFloat() && t.Size() == 4:
		return wasm3ShapeF32
	case t.IsFloat() && t.Size() == 8:
		return wasm3ShapeF64
	}
	return wasm3ShapeMixed
}

// wasm3InlineIntrinsicName returns the runtime decl for an N-ary
// shape-homogeneous captures-in-struct intrinsic, or false if no
// such intrinsic exists today (e.g. the F32 multi-arity matrix
// stops at 4).
func wasm3InlineIntrinsicName(n int, shape wasm3CapShape) (string, bool) {
	base := "wasm3MakeClosureInline" + strconv.Itoa(n)
	switch shape {
	case wasm3ShapeUintptr:
		return base, true
	case wasm3ShapeF64:
		return base + "F64", true
	case wasm3ShapeF32:
		if n > 4 {
			// Only Inline{1..4}F32 are declared in runtime.go;
			// higher arities fall back. F32 captures in real
			// code are rare enough that 4 covers the bulk.
			return "", false
		}
		return base + "F32", true
	}
	return "", false
}

// wasm3CaptureArg converts a capture expression to the IR node
// that walkClosure pushes as the corresponding arg to the
// intrinsic. uintptr-shape captures go through
// wasm3CaptureAsUintptr; float-shape captures pass through as-is
// (the intrinsic's parameter is natively float-typed).
func wasm3CaptureArg(captured ir.Node, shape wasm3CapShape) ir.Node {
	if shape == wasm3ShapeUintptr {
		return wasm3CaptureAsUintptr(captured)
	}
	return captured
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
		// doc/wasm3-m3-captures-in-struct.md: the method value's
		// single capture is the receiver `n.X`. If it's pointer-
		// shaped (the common case — every method value on a value
		// or pointer receiver) we take the captures-in-struct path,
		// passing the receiver address directly to
		// wasm3MakeClosureInline1. The -fm wrapper body, also
		// compiled on wasm3, has exactly one ClosureVar (the
		// receiver), so its body prologue auto-uses the matching
		// GetClosureField path via wasm3ClosureUsesCapturesInStruct.
		// No linear-memory `{F, R}` struct, no heap alloc, no
		// CTXT-i64 indirection — one wasmgc struct.new per method-
		// value evaluation.
		wrapper := methodValueWrapper(n)
		if wasm3MethodValueFitsInline(n.X) {
			fn := typecheck.LookupRuntime("wasm3MakeClosureInline1")
			closureType := reflectdata.TypePtrAt(base.Pos, n.Type())
			fnSym := ir.NewUnaryExpr(base.Pos, ir.OCFUNC, wrapper)
			fnSym.SetType(types.Types[types.TUINTPTR])
			fnSym = typecheck.Expr(fnSym).(*ir.UnaryExpr)
			cap0 := wasm3CaptureAsUintptr(n.X)
			call := typecheck.Call(base.Pos, fn, []ir.Node{closureType, fnSym, cap0}, false).(*ir.CallExpr)
			call.SetType(n.Type())
			return walkExpr(call, init)
		}

		// Stage G (legacy): mirror walkClosure's wasm3 path. The
		// method value `&{F, R}` is allocated on the bump heap via
		// ONEW (forced by the Prealloc bypass above), then wrapped
		// in a wasmgc closureCtx by wasm3WrapClosure. CTXT at the
		// call site is the i64 captures-ptr to the {F, R} struct,
		// which the -fm wrapper dereferences off offset 8 to
		// recover the receiver.
		fn := typecheck.LookupRuntime("wasm3WrapClosure")
		closureType := reflectdata.TypePtrAt(base.Pos, n.Type())
		fnSym := ir.NewUnaryExpr(base.Pos, ir.OCFUNC, wrapper)
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
