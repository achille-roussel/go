// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package walk

import (
	"internal/buildcfg"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
)

// This file recognizes min/max reduction loops of the form
//
//	for _, v := range x { m = min(m, v) }
//
// over slices of integer and floating-point elements (with max handled
// symmetrically and the arguments of min/max accepted in either order)
// and rewrites them into calls to vectorized runtime kernels (see
// runtime/minmax_reduce_simd_amd64.go):
//
//	m = runtime.minInt32(x, m)
//
// This is the body of slices.Min and slices.Max, which the rewrite
// sees both in their shape instantiations and inlined into callers.

// minmaxEnabled reports whether the current configuration has the
// vectorized reduction kernels, so that reduction loops should be
// matched at all.
func minmaxEnabled() bool {
	if !buildcfg.Experiment.SIMD || buildcfg.GOARCH != "amd64" {
		return false
	}
	if base.Flag.N != 0 || base.Flag.Cfg.Instrumenting {
		// Instrumented builds must keep the per-element loads that the
		// race detector and sanitizers observe, like the existing map
		// and array clear idioms.
		return false
	}
	// The runtime's own kernels (runtime/minmax_reduce*.go) end in the
	// same reduction loops that are being recognized here. This check is
	// the sole guard against rewriting them into calls to themselves.
	return types.LocalPkg.Path != "runtime"
}

// minmaxKernel returns the name and element type of the runtime kernel
// that folds a slice with element type elem into an accumulator using
// op (OMIN or OMAX), or "" if the element type has no kernel.
func minmaxKernel(op ir.Op, elem *types.Type) (string, *types.Type) {
	var suffix string
	var kind types.Kind
	switch elem.Kind() {
	case types.TUINT8:
		suffix, kind = "Uint8", types.TUINT8
	case types.TINT16:
		suffix, kind = "Int16", types.TINT16
	default:
		return "", nil
	}
	if op == ir.OMIN {
		return "min" + suffix, types.Types[kind]
	}
	return "max" + suffix, types.Types[kind]
}

// minmaxAssign matches stmt as "m = min(m, other)" or "m = max(m, other)"
// where m is a simple non-address-taken stack variable, and returns m,
// the operation, and the other argument.
func minmaxAssign(stmt ir.Node) (*ir.Name, ir.Op, ir.Node) {
	as, ok := stmt.(*ir.AssignStmt)
	if !ok || as.Op() != ir.OAS {
		return nil, ir.OXXX, nil
	}
	m, ok := as.X.(*ir.Name)
	if !ok || !isSimpleStackVar(m) || m.Addrtaken() {
		return nil, ir.OXXX, nil
	}
	call, ok := as.Y.(*ir.CallExpr)
	if !ok || (call.Op() != ir.OMIN && call.Op() != ir.OMAX) || len(call.Args) != 2 {
		return nil, ir.OXXX, nil
	}
	var other ir.Node
	switch {
	case call.Args[0] == m:
		other = call.Args[1]
	case call.Args[1] == m:
		other = call.Args[0]
	default:
		return nil, ir.OXXX, nil
	}
	return m, call.Op(), other
}

// isSimpleStackVar reports whether n is a non-blank local variable or
// parameter that lives on the stack. Unlike Name.OnStack, it does not
// panic when n is not a variable (e.g. the blank identifier).
func isSimpleStackVar(n *ir.Name) bool {
	switch n.Class {
	case ir.PAUTO, ir.PPARAM, ir.PPARAMOUT:
		return !ir.IsBlank(n) && n.OnStack()
	}
	return false
}

// rangeMinMax matches nrange as "for _, v := range x { m = min(m, v) }"
// over a slice x and rewrites it to "m = runtime.<kernel>(x, m)". The
// kernel folds an empty slice to m, so no guard is needed. v1, v2 and a
// are walkRange's normalized key, value and range expression.
func rangeMinMax(nrange *ir.RangeStmt, v1, v2 ir.Node, a ir.Node) ir.Node {
	if !minmaxEnabled() {
		return nil
	}
	if nrange.Label != nil || len(nrange.Body) != 1 {
		return nil
	}
	t := a.Type()
	if !t.IsSlice() {
		return nil
	}
	// Only "for _, v := range x": key blank, value a simple variable.
	if v1 == nil || v2 == nil || !ir.IsBlank(v1) {
		return nil
	}
	v, ok := v2.(*ir.Name)
	if !ok || !isSimpleStackVar(v) || v.Addrtaken() {
		return nil
	}
	m, op, other := minmaxAssign(nrange.Body[0])
	if m == nil || other != ir.Node(v) || m == v {
		return nil
	}
	kernel, kt := minmaxKernel(op, t.Elem())
	if kernel == "" {
		return nil
	}

	rangeInit := ir.TakeInit(nrange)

	var init ir.Nodes
	call := mkcall(kernel, kt, &init, typecheck.ConvNop(a, types.NewSlice(kt)), typecheck.ConvNop(m, kt))
	as := typecheck.Stmt(ir.NewAssignStmt(base.Pos, m, typecheck.ConvNop(call, m.Type())))

	res := walkStmt(as)
	if len(init) > 0 || len(rangeInit) > 0 {
		rangeInit.Append(init...)
		rangeInit.Append(res)
		return ir.NewBlockStmt(base.Pos, rangeInit)
	}
	return res
}
