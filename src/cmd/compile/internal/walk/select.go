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
		return walkSelectCasesWasm3Multi(cases, dflt)
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
// using a dispatch-after-poll pattern with rotated case order for
// Go-spec-required pseudo-random selection.
//
// Phase 1 (poll): try each case via its per-T non-blocking trampoline,
// in a rotated order chosen by wasm3SelectStart(N). The first ready
// case sets `chosen` to its index; subsequent tries in the rotation
// short-circuit so we never commit two trampolines per round.
//
// Phase 2 (dispatch): an if-chain on `chosen` runs the matching case
// body. Recv-case bodies get prepended assignments from per-case
// (vTmp_i, okTmp_i) temps the poll phase populated.
//
// No-default form wraps phase 1 in a `for chosen<0 { ... ; yield }`
// loop so unsuccessful rounds yield via wasm.SleepMs(1) and re-poll.
// With-default form runs phase 1 once; if `chosen<0` after, the
// default body runs.
//
// Shape (no-default):
//
//	chosen := int32(-1)
//	for chosen < 0 {
//	    switch wasm3SelectStart(N) {
//	    case 0: try0; if chosen>=0 break; try1; if chosen>=0 break; ...
//	    case 1: try1; if chosen>=0 break; try2; ...
//	    ...
//	    }
//	    if chosen < 0 { wasm3SelectYield() }
//	}
//	if chosen == 0 { user_v0 = vTmp0; ...; BODY0; break }
//	if chosen == 1 { user_v1 = vTmp1; ...; BODY1; break }
//	...
//
// With-default: same phase 1 (single iteration, no loop / no yield);
// an extra `if chosen == -1 { DFLT; break }` precedes the dispatch.
func walkSelectCasesWasm3Multi(cases []*ir.CommClause, dflt *ir.CommClause) []ir.Node {
	var init []ir.Node
	pos := base.Pos

	// chosen := int32(-1). The poll phase sets chosen to a case index
	// on success; the dispatch phase reads it.
	int32T := types.Types[types.TINT32]
	chosenVar := typecheck.TempAt(pos, ir.CurFunc, int32T)
	init = append(init, typecheck.Stmt(ir.NewAssignStmt(pos, chosenVar, ir.NewInt(pos, -1))))

	type caseInfo struct {
		idx     int          // index among non-default cases
		cas     *ir.CommClause
		isSend  bool
		chType  *types.Type
		ch      ir.Node
		sendVal ir.Node // OSEND
		userV   ir.Node // OSELRECV2
		userOk  ir.Node // OSELRECV2
		vTmp    ir.Node // populated on recv success
		okTmp   ir.Node // populated on recv success
	}
	var infos []caseInfo
	for _, cas := range cases {
		n := cas.Comm
		if n == nil {
			continue
		}
		ci := caseInfo{idx: len(infos), cas: cas}
		switch n.Op() {
		case ir.OSEND:
			s := n.(*ir.SendStmt)
			ci.isSend = true
			ci.ch = s.Chan
			ci.chType = s.Chan.Type()
			ci.sendVal = s.Value
		case ir.OSELRECV2:
			a := n.(*ir.AssignListStmt)
			recv := a.Rhs[0].(*ir.UnaryExpr)
			ci.ch = recv.X
			ci.chType = recv.X.Type()
			ci.userV = a.Lhs[0]
			ci.userOk = a.Lhs[1]
			elem := ci.chType.Elem()
			ci.vTmp = typecheck.TempAt(pos, ir.CurFunc, elem)
			ci.okTmp = typecheck.TempAt(pos, ir.CurFunc, types.Types[types.TBOOL])
			// Zero vTmp so SSA liveness sees a write before any
			// dispatch-branch read.
			init = append(init, typecheck.Stmt(ir.NewAssignStmt(pos, ci.vTmp, nil)))
		default:
			base.Fatalf("walkSelectCasesWasm3Multi: unexpected case op %v", n.Op())
		}
		infos = append(infos, ci)
	}
	N := len(infos)

	// tryBlock builds the (trampoline-call + set-chosen) IR for one
	// case, with NO body. Returns a slice of statements.
	tryBlock := func(ci caseInfo) []ir.Node {
		var out []ir.Node
		chArg := ir.NewConvExpr(pos, ir.OCONVNOP, types.Types[types.TUNSAFEPTR], ci.ch)
		chArg.SetTypecheck(1)
		if ci.isSend {
			trampoline := reflectdata.SelectNbSendFuncWasm3(ci.chType)
			var cinit ir.Nodes
			call := mkcall1(trampoline.Nname, types.Types[types.TBOOL], &cinit, chArg, ci.sendVal)
			out = append(out, cinit...)
			setChosen := typecheck.Stmt(ir.NewAssignStmt(pos, chosenVar, ir.NewInt(pos, int64(ci.idx))))
			out = append(out, typecheck.Stmt(ir.NewIfStmt(pos, call, []ir.Node{setChosen}, nil)))
		} else {
			trampoline := reflectdata.SelectNbRecvFuncWasm3(ci.chType)
			fn := trampoline.Nname
			selTmp := typecheck.TempAt(pos, ir.CurFunc, types.Types[types.TBOOL])
			var cinit ir.Nodes
			call := mkcall1(fn, fn.Type().ResultsTuple(), &cinit, chArg)
			out = append(out, cinit...)
			out = append(out, typecheck.Stmt(ir.NewAssignListStmt(pos, ir.OAS2,
				[]ir.Node{ci.vTmp, selTmp, ci.okTmp}, []ir.Node{call})))
			setChosen := typecheck.Stmt(ir.NewAssignStmt(pos, chosenVar, ir.NewInt(pos, int64(ci.idx))))
			out = append(out, typecheck.Stmt(ir.NewIfStmt(pos, selTmp, []ir.Node{setChosen}, nil)))
		}
		return out
	}

	// Build the per-rotation switch arms. Arm R tries cases in order
	// R, R+1, ..., (R-1) mod N. After each non-final try, an
	// `if chosen>=0 { break }` short-circuits the rest of this arm
	// (the break breaks the switch, not anything outer).
	var swCases []*ir.CaseClause
	for r := 0; r < N; r++ {
		var arm ir.Nodes
		for off := 0; off < N; off++ {
			k := (r + off) % N
			arm.Append(tryBlock(infos[k])...)
			if off < N-1 {
				ge := ir.NewBinaryExpr(pos, ir.OGE, chosenVar, ir.NewInt(pos, 0))
				brk := ir.NewBranchStmt(pos, ir.OBREAK, nil)
				arm.Append(typecheck.Stmt(ir.NewIfStmt(pos, ge, []ir.Node{brk}, nil)))
			}
		}
		swCases = append(swCases, ir.NewCaseStmt(pos, []ir.Node{ir.NewInt(pos, int64(r))}, arm))
	}

	// startVar := wasm3SelectStart(N)
	startVar := typecheck.TempAt(pos, ir.CurFunc, int32T)
	var startInit ir.Nodes
	startCall := mkcall("wasm3SelectStart", int32T, &startInit, ir.NewInt(pos, int64(N)))
	startInit.Append(typecheck.Stmt(ir.NewAssignStmt(pos, startVar, startCall)))
	swStmt := typecheck.Stmt(ir.NewSwitchStmt(pos, startVar, swCases))

	if dflt == nil {
		// Wrap phase 1 in `for chosen<0 { switch; if chosen<0 yield }`.
		var loopBody ir.Nodes
		loopBody.Append(startInit...)
		loopBody.Append(swStmt)
		lt := ir.NewBinaryExpr(pos, ir.OLT, chosenVar, ir.NewInt(pos, 0))
		var yieldInit ir.Nodes
		yieldCall := typecheck.Stmt(mkcall("wasm3SelectYield", nil, &yieldInit))
		yieldBody := append(append([]ir.Node{}, yieldInit...), yieldCall)
		loopBody.Append(typecheck.Stmt(ir.NewIfStmt(pos, lt, yieldBody, nil)))
		loopCond := ir.NewBinaryExpr(pos, ir.OLT, chosenVar, ir.NewInt(pos, 0))
		init = append(init, typecheck.Stmt(ir.NewForStmt(pos, nil, loopCond, nil, loopBody, false)))
	} else {
		// With-default: single attempt; default fires if nothing ready.
		init = append(init, startInit...)
		init = append(init, swStmt)
	}

	// Dispatch chain: one if-statement per case; each ends with
	// OBREAK so every reachable path through sel.Compiled terminates
	// in a break (the SSA OSELECT handler at ssagen/ssa.go:2398
	// asserts this — without it the fall-through gets marked
	// BlockExit, deleting code after the select).
	//
	// Build each dispatch IfStmt by typechecking its cond and the
	// freshly-synthesised AssignStmts (the userV/userOk bindings) on
	// their own, then assembling the IfStmt and marking it typechecked.
	// We must NOT call typecheck.Stmt on the whole IfStmt: the case
	// body (ci.cas.Body) is already post-walk and may contain INLMARK
	// nodes (left behind by mid-stack inlining of calls in the case
	// body). Re-typechecking through Stmts(IfStmt.Body) walks into the
	// INLMARK, which typecheck1 has no handler for — fatal "typecheck
	// inlmark".
	mkDispatchIf := func(cond ir.Node, body ir.Nodes) ir.Node {
		cond = typecheck.Expr(cond)
		cond = typecheck.DefaultLit(cond, nil)
		ifs := ir.NewIfStmt(pos, cond, body, nil)
		ifs.SetTypecheck(1)
		return ifs
	}
	for _, ci := range infos {
		var body ir.Nodes
		if !ci.isSend {
			if !ir.IsBlank(ci.userV) {
				body.Append(typecheck.Stmt(ir.NewAssignStmt(pos, ci.userV, ci.vTmp)))
			}
			if !ir.IsBlank(ci.userOk) {
				body.Append(typecheck.Stmt(ir.NewAssignStmt(pos, ci.userOk, ci.okTmp)))
			}
		}
		body.Append(ci.cas.Body.Take()...)
		body.Append(ir.NewBranchStmt(pos, ir.OBREAK, nil))
		cond := ir.NewBinaryExpr(pos, ir.OEQ, chosenVar, ir.NewInt(pos, int64(ci.idx)))
		init = append(init, mkDispatchIf(cond, body))
	}
	if dflt != nil {
		var body ir.Nodes
		body.Append(dflt.Body.Take()...)
		body.Append(ir.NewBranchStmt(pos, ir.OBREAK, nil))
		cond := ir.NewBinaryExpr(pos, ir.OLT, chosenVar, ir.NewInt(pos, 0))
		init = append(init, mkDispatchIf(cond, body))
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
