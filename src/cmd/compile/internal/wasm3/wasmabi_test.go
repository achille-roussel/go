// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm3

import (
	"testing"

	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/wasmgc"
)

// sig builds a func type from parameter and result types (no receiver).
func sig(params, results []*types.Type) *types.Type {
	mkFields := func(ts []*types.Type) []*types.Field {
		fs := make([]*types.Field, len(ts))
		for i, t := range ts {
			fs[i] = field("_", t)
		}
		return fs
	}
	return types.NewSignature(nil, mkFields(params), mkFields(results))
}

// wantFields checks an obj.WasmField slice against an expected shape.
// A negative refType entry means "any reference"; a non-negative entry
// means the reference must point at that exact type index.
func wantFields(t *testing.T, label string, got []obj.WasmField, want []obj.WasmField) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d fields, want %d (%+v vs %+v)", label, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i].Type != want[i].Type {
			t.Errorf("%s field %d: type %d, want %d", label, i, got[i].Type, want[i].Type)
			continue
		}
		if want[i].Type == obj.WasmRef && want[i].Offset >= 0 && got[i].Offset != want[i].Offset {
			t.Errorf("%s field %d: ref type index %d, want %d", label, i, got[i].Offset, want[i].Offset)
		}
	}
}

func TestLoweredSignatureScalars(t *testing.T) {
	// func(int64, float64, int32) int32
	ft := sig(
		[]*types.Type{types.Types[types.TINT64], types.Types[types.TFLOAT64], types.Types[types.TINT32]},
		[]*types.Type{types.Types[types.TINT32]},
	)
	c := newTypeCollector()
	got := c.loweredSignature(ft)

	wantFields(t, "params", got.Params, []obj.WasmField{
		{Type: obj.WasmI64}, {Type: obj.WasmF64}, {Type: obj.WasmI32},
	})
	wantFields(t, "results", got.Results, []obj.WasmField{{Type: obj.WasmI32}})
}

func TestLoweredSignatureEmpty(t *testing.T) {
	// func()
	c := newTypeCollector()
	got := c.loweredSignature(sig(nil, nil))
	if got.Params != nil || got.Results != nil {
		t.Fatalf("func() lowered to %+v, want empty params and results", got)
	}
}

func TestLoweredSignatureString(t *testing.T) {
	// func(string) string — a string explodes to {backing, offset, length}.
	ft := sig(
		[]*types.Type{types.Types[types.TSTRING]},
		[]*types.Type{types.Types[types.TSTRING]},
	)
	c := newTypeCollector()
	got := c.loweredSignature(ft)

	want := []obj.WasmField{
		{Type: obj.WasmRef, Offset: wasmgc.TypeGoBytes},
		{Type: obj.WasmI32},
		{Type: obj.WasmI32},
	}
	wantFields(t, "params", got.Params, want)
	wantFields(t, "results", got.Results, want)
}

func TestLoweredSignaturePointer(t *testing.T) {
	// func(*struct{v int64})
	inner := types.NewStruct([]*types.Field{field("v", types.Types[types.TINT64])})
	ft := sig([]*types.Type{types.NewPtr(inner)}, nil)
	c := newTypeCollector()
	got := c.loweredSignature(ft)

	if len(got.Params) != 1 || got.Params[0].Type != obj.WasmRef {
		t.Fatalf("func(*struct) params = %+v, want one reference", got.Params)
	}
	// The reference must point at a struct the collector actually registered.
	idx := got.Params[0].Offset
	if idx < 0 || idx >= int64(len(c.table)) {
		t.Fatalf("pointer param references type index %d, out of range [0,%d)", idx, len(c.table))
	}
	if c.table[idx].Kind != wasmgc.KindStruct {
		t.Errorf("pointer param references a non-struct type %+v", c.table[idx])
	}
}

func TestLoweredSignatureStructValueFlattens(t *testing.T) {
	// func(struct{a int32; b float64}) — a struct value flattens in place.
	st := types.NewStruct([]*types.Field{
		field("a", types.Types[types.TINT32]),
		field("b", types.Types[types.TFLOAT64]),
	})
	c := newTypeCollector()
	got := c.loweredSignature(sig([]*types.Type{st}, nil))

	wantFields(t, "params", got.Params, []obj.WasmField{
		{Type: obj.WasmI32}, {Type: obj.WasmF64},
	})
}

func TestLoweredSignatureCollectsTypes(t *testing.T) {
	// Lowering a signature that mentions a struct must register that
	// struct (and its dependencies) in the collector's table, leaving
	// the table in valid type-section emit order.
	inner := types.NewStruct([]*types.Field{field("v", types.Types[types.TINT64])})
	ft := sig([]*types.Type{types.NewPtr(inner)}, []*types.Type{types.Types[types.TSTRING]})
	c := newTypeCollector()
	before := len(c.table)
	c.loweredSignature(ft)
	if len(c.table) <= before {
		t.Errorf("loweredSignature did not register the referenced struct type")
	}
	checkDependencyOrder(t, c.table)
}

// TestFlatPrimitiveAttachSlice locks in that a function whose param
// list contains a slice gets a 3-i64 lowering from tryFlatPrimitiveAttach
// — the regabi-matching shape (ptr, len, cap) the SSA call site pushes.
// Before this fallback existed the function would have no WasmType
// aux and the linker would default to ()->(), breaking the caller's
// wasm-stack balance at validation time.
func TestFlatPrimitiveAttachSlice(t *testing.T) {
	ft := sig(
		[]*types.Type{types.NewSlice(types.Types[types.TINT32])},
		[]*types.Type{types.Types[types.TINT32]},
	)
	got, ok := tryFlatPrimitiveAttach(ft)
	if !ok {
		t.Fatalf("tryFlatPrimitiveAttach rejected func([]int32) int32")
	}
	wantFields(t, "params", got.Params, []obj.WasmField{
		{Type: obj.WasmI64}, // backing ptr
		{Type: obj.WasmI64}, // len
		{Type: obj.WasmI64}, // cap
	})
	wantFields(t, "results", got.Results, []obj.WasmField{
		{Type: obj.WasmI32},
	})
}

// TestFlatPrimitiveAttachPointerStruct covers the trace-locker shape
// (a struct of {*m, uintptr}) that the typeCollector path used to
// produce a ref-typed signature for. collectorMatchesRegabi now
// rejects struct fields that are pointers; the flat fallback lowers
// every leaf to its register-shaped wasm type, yielding two i64
// results which matches what the SSA call site pushes.
func TestFlatPrimitiveAttachPointerStruct(t *testing.T) {
	// `struct{ *struct{ v int64 }; uintptr }`
	inner := types.NewStruct([]*types.Field{field("v", types.Types[types.TINT64])})
	st := types.NewStruct([]*types.Field{
		field("mp", types.NewPtr(inner)),
		field("gen", types.Types[types.TUINTPTR]),
	})
	ft := sig(nil, []*types.Type{st})
	got, ok := tryFlatPrimitiveAttach(ft)
	if !ok {
		t.Fatalf("tryFlatPrimitiveAttach rejected struct{*T; uintptr} result")
	}
	if len(got.Params) != 0 {
		t.Errorf("params = %+v, want empty", got.Params)
	}
	wantFields(t, "results", got.Results, []obj.WasmField{
		{Type: obj.WasmI64}, // *inner — i64-register-shaped pointer
		{Type: obj.WasmI64}, // uintptr
	})
}

func TestCollectSignature(t *testing.T) {
	// func(*struct{v int64}, int32) string
	inner := types.NewStruct([]*types.Field{field("v", types.Types[types.TINT64])})
	ft := sig(
		[]*types.Type{types.NewPtr(inner), types.Types[types.TINT32]},
		[]*types.Type{types.Types[types.TSTRING]},
	)
	c := newTypeCollector()
	idx := c.collectSignature(ft)

	got := c.table[idx]
	if got.Kind != wasmgc.KindFunc {
		t.Fatalf("collectSignature produced a %d-kind entry, want KindFunc", got.Kind)
	}
	// params: one reference (the *struct) + one i32; result: a string,
	// which flattens to {backing ref, offset i32, length i32}.
	if len(got.Params) != 2 || !got.Params[0].IsRef() || got.Params[1] != wasmgc.PrimStorage(wasmgc.I32) {
		t.Errorf("params = %+v, want {ref, i32}", got.Params)
	}
	if len(got.Results) != 3 || !got.Results[0].IsRef() {
		t.Errorf("results = %+v, want a flattened string {ref, i32, i32}", got.Results)
	}
	// The referenced struct must be a registered struct subtyping go.object.
	pointee := c.table[got.Params[0].RefType]
	if pointee.Kind != wasmgc.KindStruct || pointee.Super != wasmgc.TypeGoObject {
		t.Errorf("param ref points at %+v, want a struct subtyping go.object", pointee)
	}
	checkDependencyOrder(t, c.table)

	// Memoized: collecting the same func type again returns the same
	// index without growing the table.
	n := len(c.table)
	if again := c.collectSignature(ft); again != idx || len(c.table) != n {
		t.Errorf("re-collecting the signature grew/changed the table (idx %d->%d, len %d->%d)", idx, again, n, len(c.table))
	}

	// The whole table, func type included, must encode and validate.
	validateModule(t, "collected-signature", wrapModule(c.table.EncodeTypeSection()))
}

func TestCollectClosureCtx(t *testing.T) {
	// func(int, int) int — a simple two-int-in / one-int-out signature.
	ft := sig(
		[]*types.Type{types.Types[types.TINT], types.Types[types.TINT]},
		[]*types.Type{types.Types[types.TINT]},
	)
	c := newTypeCollector()
	cidx := c.collectClosureCtx(ft)

	// The closure-context entry must be a struct subtyping go.object
	// with exactly one field: a (ref $funcType) pointing at the
	// matching collectSignature entry.
	got := c.table[cidx]
	if got.Kind != wasmgc.KindStruct {
		t.Fatalf("collectClosureCtx produced a %d-kind entry, want KindStruct", got.Kind)
	}
	if got.Super != wasmgc.TypeGoObject {
		t.Errorf("super = %d, want TypeGoObject (%d)", got.Super, wasmgc.TypeGoObject)
	}
	if len(got.Fields) != 2 {
		t.Fatalf("fields = %d, want 2", len(got.Fields))
	}
	if !got.Fields[0].Storage.IsRef() {
		t.Errorf("first field = %+v, want a ref", got.Fields[0])
	}
	funcIdx := got.Fields[0].Storage.RefType
	if c.table[funcIdx].Kind != wasmgc.KindFunc {
		t.Errorf("first field points at table[%d] kind=%d, want KindFunc",
			funcIdx, c.table[funcIdx].Kind)
	}
	if got.Fields[1].Storage.IsRef() {
		t.Errorf("second field = %+v, want a primitive i64 (captures pointer)", got.Fields[1])
	}

	// Memoized: collecting the same func type again returns the same
	// index without growing the table.
	n := len(c.table)
	if again := c.collectClosureCtx(ft); again != cidx || len(c.table) != n {
		t.Errorf("re-collecting the closure ctx grew/changed the table (idx %d->%d, len %d->%d)", cidx, again, n, len(c.table))
	}

	// The whole table, closure ctx + func type included, must encode
	// and validate.
	validateModule(t, "collected-closure-ctx", wrapModule(c.table.EncodeTypeSection()))
}
