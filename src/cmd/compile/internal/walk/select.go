// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package walk

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"internal/buildcfg"
)

func walkSelect(sel *ir.SelectStmt) {
	lno := ir.SetPos(sel)
	if sel.Walked() {
		base.Fatalf("double walkSelect")
	}
	sel.SetWalked(true)

	init := ir.TakeInit(sel)

	init = append(init, walkSelectCases(sel.Cases)...)
	sel.Cases = nil

	sel.Compiled = init
	walkStmtList(sel.Compiled)

	base.Pos = lno
}

func walkSelectCases(cases []*ir.CommClause) []ir.Node {
	ncas := len(cases)
	sellineno := base.Pos

	// optimization: zero-case select
	if ncas == 0 {
		return []ir.Node{mkcallstmt("block")}
	}

	// optimization: one-case select: single op.
	if ncas == 1 {
		cas := cases[0]
		ir.SetPos(cas)
		l := cas.Init()
		if cas.Comm != nil { // not default:
			n := cas.Comm
			l = append(l, ir.TakeInit(n)...)
			switch n.Op() {
			default:
				base.Fatalf("select %v", n.Op())

			case ir.OSEND:
				// already ok

			case ir.OSELRECV2:
				r := n.(*ir.AssignListStmt)
				if ir.IsBlank(r.Lhs[0]) && ir.IsBlank(r.Lhs[1]) {
					n = r.Rhs[0]
					break
				}
				r.SetOp(ir.OAS2RECV)
			}

			l = append(l, n)
		}

		l = append(l, cas.Body...)
		l = append(l, ir.NewBranchStmt(base.Pos, ir.OBREAK, nil))
		return l
	}

	// convert case value arguments to addresses.
	// this rewrite is used by both the general code and the next optimization.
	//
	// Skipped on GOARCH=wasm3: there is no Go stack in linear memory, so
	// a scalar &local lowers to an SP-relative TYPE_ADDR the obj backend
	// can't encode (only string/slice/iface have boxed-local cells today,
	// see doc/wasm3-addressable-boxed-locals.md). The wasm3 select
	// trampolines instead take/return values by value.
	var dflt *ir.CommClause
	for _, cas := range cases {
		ir.SetPos(cas)
		n := cas.Comm
		if n == nil {
			dflt = cas
			continue
		}
		if buildcfg.GOARCH == "wasm3" {
			continue
		}
		switch n.Op() {
		case ir.OSEND:
			n := n.(*ir.SendStmt)
			n.Value = typecheck.NodAddr(n.Value)
			n.Value = typecheck.Expr(n.Value)

		case ir.OSELRECV2:
			n := n.(*ir.AssignListStmt)
			if !ir.IsBlank(n.Lhs[0]) {
				n.Lhs[0] = typecheck.NodAddr(n.Lhs[0])
				n.Lhs[0] = typecheck.Expr(n.Lhs[0])
			}
		}
	}

	// optimization: two-case select but one is default: single non-blocking op.
	if ncas == 2 && dflt != nil {
		cas := cases[0]
		if cas == dflt {
			cas = cases[1]
		}

		n := cas.Comm
		ir.SetPos(n)
		r := ir.NewIfStmt(base.Pos, nil, nil, nil)
		r.SetInit(cas.Init())
		var cond ir.Node
		switch n.Op() {
		default:
			base.Fatalf("select %v", n.Op())

		case ir.OSEND:
			// if selectnbsend(c, v) { body } else { default body }
			n := n.(*ir.SendStmt)
			ch := n.Chan
			if buildcfg.GOARCH == "wasm3" {
				// Per-T wasm3 trampoline takes (c unsafe.Pointer, v T)
				// by value (n.Value was NOT NodAddr'd on wasm3 — see
				// the earlier-pass NodAddr skip).
				trampoline := reflectdata.SelectNbSendFuncWasm3(ch.Type())
				chArg := ir.NewConvExpr(base.Pos, ir.OCONVNOP, types.Types[types.TUNSAFEPTR], ch)
				chArg.SetTypecheck(1)
				cond = mkcall1(trampoline.Nname, types.Types[types.TBOOL], r.PtrInit(), chArg, n.Value)
				break
			}
			cond = mkcall1(chanfn("selectnbsend", 2, ch.Type()), types.Types[types.TBOOL], r.PtrInit(), ch, n.Value)

		case ir.OSELRECV2:
			n := n.(*ir.AssignListStmt)
			recv := n.Rhs[0].(*ir.UnaryExpr)
			ch := recv.X
			elem := n.Lhs[0]
			if buildcfg.GOARCH == "wasm3" {
				// Per-T wasm3 trampoline returns (v T, selected,
				// received) by value. Distribute v into the user's
				// LHS in-line; cond/received feed the if/else after.
				trampoline := reflectdata.SelectNbRecvFuncWasm3(ch.Type())
				fn := trampoline.Nname
				chArg := ir.NewConvExpr(base.Pos, ir.OCONVNOP, types.Types[types.TUNSAFEPTR], ch)
				chArg.SetTypecheck(1)
				call := mkcall1(fn, fn.Type().ResultsTuple(), r.PtrInit(), chArg)
				cond = typecheck.TempAt(base.Pos, ir.CurFunc, types.Types[types.TBOOL])
				vTmp := typecheck.TempAt(base.Pos, ir.CurFunc, ch.Type().Elem())
				okTmp := n.Lhs[1]
				if ir.IsBlank(okTmp) {
					okTmp = typecheck.TempAt(base.Pos, ir.CurFunc, types.Types[types.TBOOL])
				}
				as := ir.NewAssignListStmt(r.Pos(), ir.OAS2, []ir.Node{vTmp, cond, okTmp}, []ir.Node{call})
				r.PtrInit().Append(typecheck.Stmt(as))
				if !ir.IsBlank(elem) {
					// Prepend `elem = vTmp` to the case body so the
					// user's variable receives the value before BODY
					// runs.
					assignV := typecheck.Stmt(ir.NewAssignStmt(base.Pos, elem, vTmp))
					cas.Body = append([]ir.Node{assignV}, cas.Body...)
				}
				break
			}
			if ir.IsBlank(elem) {
				elem = typecheck.NodNil()
			}
			cond = typecheck.TempAt(base.Pos, ir.CurFunc, types.Types[types.TBOOL])
			fn := chanfn("selectnbrecv", 2, ch.Type())
			call := mkcall1(fn, fn.Type().ResultsTuple(), r.PtrInit(), elem, ch)
			as := ir.NewAssignListStmt(r.Pos(), ir.OAS2, []ir.Node{cond, n.Lhs[1]}, []ir.Node{call})
			r.PtrInit().Append(typecheck.Stmt(as))
		}

		r.Cond = typecheck.Expr(cond)
		r.Body = cas.Body
		r.Else = append(dflt.Init(), dflt.Body...)
		return []ir.Node{r, ir.NewBranchStmt(base.Pos, ir.OBREAK, nil)}
	}

	if buildcfg.GOARCH == "wasm3" {
		// Multi-case select on wasm3 is non-trivial — see
		// doc/wasm3-select-status.md (and the abandoned attempts in
		// walkSelectCasesWasm3Multi, kept below for reference). Two
		// design directions both have substantial blockers:
		//
		//   1. Inline-poll: walk-emitted polling loop. The CFG
		//      produced (for-loop with N if-then-break branches, each
		//      with a trampoline call) is valid Go IR but the wasm3
		//      obj backend's CFG analyser (wasm3AnalyzeCFG) bails on
		//      the RESUMEPOINT shape and main.main stubs to
		//      `unreachable`.
		//
		//   2. Synthesised nested 2-case-default SelectStmts: build a
		//      fresh SelectStmt per case and walk each through the
		//      proven 2-case-default emission path. Compiles cleanly
		//      for handwritten equivalents in the user's source, but
		//      the synthesised SelectStmts trip the walker's typecheck
		//      ("unknown op RESULT" via the case body's expressions)
		//      because the cases were already typechecked by the
		//      outer walkSelect before they reach us.
		//
		// Both directions need real compiler-internals work to land.
		// Until then, emit a runtime trap so multi-case selects build
		// (rather than panicking the compiler at SSA-liveness or walk
		// time) and the user gets a clear "not yet supported" signal
		// at run time. 2-case-with-default selects (the common
		// non-blocking poll idiom and the `close(c)` interaction) are
		// handled by the path above and work end-to-end.
		var init ir.Nodes
		call := mkcall("panicwrap", nil, &init)
		init.Append(typecheck.Stmt(call))
		return init
	}
	if dflt != nil {
		ncas--
	}
	casorder := make([]*ir.CommClause, ncas)
	nsends, nrecvs := 0, 0

	var init []ir.Node

	// generate sel-struct
	base.Pos = sellineno
	selv := typecheck.TempAt(base.Pos, ir.CurFunc, types.NewArray(scasetype(), int64(ncas)))
	init = append(init, typecheck.Stmt(ir.NewAssignStmt(base.Pos, selv, nil)))

	// No initialization for order; runtime.selectgo is responsible for that.
	order := typecheck.TempAt(base.Pos, ir.CurFunc, types.NewArray(types.Types[types.TUINT16], 2*int64(ncas)))

	var pc0, pcs ir.Node
	if base.Flag.Race {
		pcs = typecheck.TempAt(base.Pos, ir.CurFunc, types.NewArray(types.Types[types.TUINTPTR], int64(ncas)))
		pc0 = typecheck.Expr(typecheck.NodAddr(ir.NewIndexExpr(base.Pos, pcs, ir.NewInt(base.Pos, 0))))
	} else {
		pc0 = typecheck.NodNil()
	}

	// register cases
	for _, cas := range cases {
		ir.SetPos(cas)

		init = append(init, ir.TakeInit(cas)...)

		n := cas.Comm
		if n == nil { // default:
			continue
		}

		var i int
		var c, elem ir.Node
		switch n.Op() {
		default:
			base.Fatalf("select %v", n.Op())
		case ir.OSEND:
			n := n.(*ir.SendStmt)
			i = nsends
			nsends++
			c = n.Chan
			elem = n.Value
		case ir.OSELRECV2:
			n := n.(*ir.AssignListStmt)
			nrecvs++
			i = ncas - nrecvs
			recv := n.Rhs[0].(*ir.UnaryExpr)
			c = recv.X
			elem = n.Lhs[0]
		}

		casorder[i] = cas

		setField := func(f string, val ir.Node) {
			r := ir.NewAssignStmt(base.Pos, ir.NewSelectorExpr(base.Pos, ir.ODOT, ir.NewIndexExpr(base.Pos, selv, ir.NewInt(base.Pos, int64(i))), typecheck.Lookup(f)), val)
			init = append(init, typecheck.Stmt(r))
		}

		c = typecheck.ConvNop(c, types.Types[types.TUNSAFEPTR])
		setField("c", c)
		if !ir.IsBlank(elem) {
			elem = typecheck.ConvNop(elem, types.Types[types.TUNSAFEPTR])
			setField("elem", elem)
		}

		// TODO(mdempsky): There should be a cleaner way to
		// handle this.
		if base.Flag.Race {
			r := mkcallstmt("selectsetpc", typecheck.NodAddr(ir.NewIndexExpr(base.Pos, pcs, ir.NewInt(base.Pos, int64(i)))))
			init = append(init, r)
		}
	}
	if nsends+nrecvs != ncas {
		base.Fatalf("walkSelectCases: miscount: %v + %v != %v", nsends, nrecvs, ncas)
	}

	// run the select
	base.Pos = sellineno
	chosen := typecheck.TempAt(base.Pos, ir.CurFunc, types.Types[types.TINT])
	recvOK := typecheck.TempAt(base.Pos, ir.CurFunc, types.Types[types.TBOOL])
	r := ir.NewAssignListStmt(base.Pos, ir.OAS2, nil, nil)
	r.Lhs = []ir.Node{chosen, recvOK}
	fn := typecheck.LookupRuntime("selectgo")
	var fnInit ir.Nodes
	r.Rhs = []ir.Node{mkcall1(fn, fn.Type().ResultsTuple(), &fnInit, bytePtrToIndex(selv, 0), bytePtrToIndex(order, 0), pc0, ir.NewInt(base.Pos, int64(nsends)), ir.NewInt(base.Pos, int64(nrecvs)), ir.NewBool(base.Pos, dflt == nil))}
	init = append(init, fnInit...)
	init = append(init, typecheck.Stmt(r))

	// selv, order, and pcs (if race) are no longer alive after selectgo.

	// dispatch cases
	dispatch := func(cond ir.Node, cas *ir.CommClause) {
		var list ir.Nodes

		if n := cas.Comm; n != nil && n.Op() == ir.OSELRECV2 {
			n := n.(*ir.AssignListStmt)
			if !ir.IsBlank(n.Lhs[1]) {
				x := ir.NewAssignStmt(base.Pos, n.Lhs[1], recvOK)
				list.Append(typecheck.Stmt(x))
			}
		}

		list.Append(cas.Body.Take()...)
		list.Append(ir.NewBranchStmt(base.Pos, ir.OBREAK, nil))

		var r ir.Node
		if cond != nil {
			cond = typecheck.Expr(cond)
			cond = typecheck.DefaultLit(cond, nil)
			r = ir.NewIfStmt(base.Pos, cond, list, nil)
		} else {
			r = ir.NewBlockStmt(base.Pos, list)
		}

		init = append(init, r)
	}

	if dflt != nil {
		ir.SetPos(dflt)
		dispatch(ir.NewBinaryExpr(base.Pos, ir.OLT, chosen, ir.NewInt(base.Pos, 0)), dflt)
	}
	for i, cas := range casorder {
		ir.SetPos(cas)
		if i == len(casorder)-1 {
			dispatch(nil, cas)
			break
		}
		dispatch(ir.NewBinaryExpr(base.Pos, ir.OEQ, chosen, ir.NewInt(base.Pos, int64(i))), cas)
	}

	return init
}

// walkSelectCasesWasm3Multi lowers a multi-case select on GOARCH=wasm3
// by emitting a polling for-loop that tries each case non-blocking
// via the per-T trampolines, falls back to a wasm.SleepMs(1) yield
// when nothing is ready, and dispatches the matched case body inside
// the loop (so a user's `break` inside a case body breaks the
// for-loop = the select). Default case becomes the no-ready fallback.
//
// Cases are tried top-to-bottom; the first ready one wins. No
// randomisation (real Go select randomises to prevent starvation).
//
// Shape (3 cases, no default):
//
//	for {
//	    if wasm3SelectNbSend.T0(c0, e0) { BODY0; break }
//	    v1, sel1, ok1 := wasm3SelectNbRecv.T1(c1)
//	    if sel1 { user_v1 = v1; user_ok1 = ok1; BODY1; break }
//	    if wasm3SelectNbSend.T2(c2, e2) { BODY2; break }
//	    wasm.SleepMs(1)
//	}
//
// With default:
//
//	for {
//	    ... try each case ...
//	    DEFAULT_BODY; break
//	}
func walkSelectCasesWasm3Multi(cases []*ir.CommClause, dflt *ir.CommClause) []ir.Node {
	var init []ir.Node

	// `done` is the "we picked a case" flag — false initially, set
	// true at the end of any case body that fires. Each per-case
	// nested SelectStmt is gated by `if !done { … }`, so once any
	// case succeeds the rest short-circuit.
	doneVar := typecheck.TempAt(base.Pos, ir.CurFunc, types.Types[types.TBOOL])
	init = append(init, typecheck.Stmt(ir.NewAssignStmt(base.Pos, doneVar, nil)))

	// Build sequential per-case attempts. Each one is a synthesised
	// 2-case-default SelectStmt (which goes through walkSelectCases'
	// proven 2-case-default emission path) wrapped in `if !done`.
	var perIter ir.Nodes
	for _, cas := range cases {
		n := cas.Comm
		if n == nil {
			continue // default handled after the per-case sweep
		}
		// Augment the case body with `done = true` so subsequent
		// gates short-circuit. cas.Body.Take() empties cas.Body;
		// the caller (walkSelect) does not re-read it after we
		// return.
		bodyWithDone := append([]ir.Node(cas.Body.Take()),
			typecheck.Stmt(ir.NewAssignStmt(base.Pos, doneVar, ir.NewBool(base.Pos, true))))

		// Build: select { case <original-comm>: bodyWithDone; default: }
		synthCase := ir.NewCommStmt(base.Pos, n, bodyWithDone)
		dfltCase := ir.NewCommStmt(base.Pos, nil, nil)
		synthSel := ir.NewSelectStmt(base.Pos, []*ir.CommClause{synthCase, dfltCase})

		// Walk the synthetic select. Its Compiled is the lowered
		// IR shape walkSelectCases' 2-case-default branch produces.
		walkSelect(synthSel)

		// if !done { synthSel.Compiled }
		notDone := ir.NewUnaryExpr(base.Pos, ir.ONOT, doneVar)
		perIter.Append(typecheck.Stmt(ir.NewIfStmt(base.Pos, notDone, synthSel.Compiled, nil)))
	}

	if dflt != nil {
		init = append(init, perIter...)
		// Final fallback: if no case fired, run the user's default.
		dfltBody := append([]ir.Node(dflt.Body.Take()),
			typecheck.Stmt(ir.NewAssignStmt(base.Pos, doneVar, ir.NewBool(base.Pos, true))))
		notDone := ir.NewUnaryExpr(base.Pos, ir.ONOT, doneVar)
		init = append(init, typecheck.Stmt(ir.NewIfStmt(base.Pos, notDone, dfltBody, nil)))
	} else {
		// No default — must block until some case fires. Loop with
		// a yield in the no-progress branch so a goroutine parked
		// on one of the chans gets a chance to run between polls.
		yieldFn := typecheck.LookupRuntime("wasm3SelectYield")
		var callInit ir.Nodes
		yieldCall := mkcall1(yieldFn, nil, &callInit)
		perIter.Append(callInit...)
		notDoneYield := ir.NewUnaryExpr(base.Pos, ir.ONOT, doneVar)
		perIter.Append(typecheck.Stmt(ir.NewIfStmt(base.Pos, notDoneYield,
			[]ir.Node{typecheck.Stmt(yieldCall)}, nil)))
		notDoneLoop := ir.NewUnaryExpr(base.Pos, ir.ONOT, doneVar)
		loop := ir.NewForStmt(base.Pos, nil, notDoneLoop, nil, perIter, false)
		init = append(init, typecheck.Stmt(loop))
	}
	return init
}

// bytePtrToIndex returns a Node representing "(*byte)(&n[i])".
func bytePtrToIndex(n ir.Node, i int64) ir.Node {
	s := typecheck.NodAddr(ir.NewIndexExpr(base.Pos, n, ir.NewInt(base.Pos, i)))
	t := types.NewPtr(types.Types[types.TUINT8])
	return typecheck.ConvNop(s, t)
}

var scase *types.Type

// Keep in sync with src/runtime/select.go.
func scasetype() *types.Type {
	if scase == nil {
		n := ir.NewDeclNameAt(src.NoXPos, ir.OTYPE, ir.Pkgs.Runtime.Lookup("scase"))
		scase = types.NewNamed(n)
		n.SetType(scase)
		n.SetTypecheck(1)

		scase.SetUnderlying(types.NewStruct([]*types.Field{
			types.NewField(base.Pos, typecheck.Lookup("c"), types.Types[types.TUNSAFEPTR]),
			types.NewField(base.Pos, typecheck.Lookup("elem"), types.Types[types.TUNSAFEPTR]),
		}))
	}
	return scase
}
