// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm3

import (
	"bufio"
	"os"
	"testing"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ssagen"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/obj/wasm"
	"cmd/internal/src"
)

// TestMain initializes just enough of the compiler's global state — the
// wasm3 link architecture, the local package, pointer/register sizes,
// and the predeclared type universe — for the typeCollector tests below
// to build real *types.Type values.
func TestMain(m *testing.M) {
	ssagen.Arch.LinkArch = &wasm.Linkwasm3
	ssagen.Arch.REGSP = wasm.REG_SP
	ssagen.Arch.MAXWIDTH = 1 << 50
	types.MaxWidth = ssagen.Arch.MAXWIDTH
	base.Ctxt = obj.Linknew(ssagen.Arch.LinkArch)
	base.Ctxt.DiagFunc = base.Errorf
	base.Ctxt.DiagFlush = base.FlushErrors
	base.Ctxt.Bso = bufio.NewWriter(os.Stdout)
	types.LocalPkg = types.NewPkg("p", "local")
	types.LocalPkg.Prefix = "p"
	types.PtrSize = ssagen.Arch.LinkArch.PtrSize
	types.RegSize = ssagen.Arch.LinkArch.RegSize
	typecheck.InitUniverse()
	os.Exit(m.Run())
}

// testObj is a minimal types.Object so tests can build named (and
// therefore recursive) types via types.NewNamed.
type testObj struct {
	sym *types.Sym
}

func (o *testObj) Pos() src.XPos     { return src.NoXPos }
func (o *testObj) Sym() *types.Sym   { return o.sym }
func (o *testObj) Type() *types.Type { return nil }

// field builds a named struct field of the given type.
func field(name string, t *types.Type) *types.Field {
	return types.NewField(src.NoXPos, types.LocalPkg.Lookup(name), t)
}

// namedStruct builds a named struct type, allowing its fields to refer
// back to it (directly or through a pointer) for recursion tests.
func namedStruct(name string, fields func(self *types.Type) []*types.Field) *types.Type {
	t := types.NewNamed(&testObj{sym: types.LocalPkg.Lookup(name)})
	t.SetUnderlying(types.NewStruct(fields(t)))
	return t
}

// groupOf returns the ordinal of the recursion group that contains the
// type at table index typeIdx, or -1 if no group does. The ordinal is
// the group's position in the recGroups result, which is also its emit
// order in the module type section.
func groupOf(groups [][]int, typeIdx int) int {
	for g, group := range groups {
		for _, t := range group {
			if t == typeIdx {
				return g
			}
		}
	}
	return -1
}

// groupSize returns the number of types in the group containing typeIdx.
func groupSize(groups [][]int, typeIdx int) int {
	g := groupOf(groups, typeIdx)
	if g < 0 {
		return 0
	}
	return len(groups[g])
}

// checkGroupsCoverAll verifies the groups partition exactly [0, n).
func checkGroupsCoverAll(t *testing.T, groups [][]int, n int) {
	t.Helper()
	seen := make([]bool, n)
	count := 0
	for _, group := range groups {
		for _, idx := range group {
			if idx < 0 || idx >= n {
				t.Fatalf("group member %d out of range [0,%d)", idx, n)
			}
			if seen[idx] {
				t.Fatalf("type %d appears in more than one group", idx)
			}
			seen[idx] = true
			count++
		}
	}
	if count != n {
		t.Fatalf("groups cover %d types, want %d", count, n)
	}
}

// checkDependencyOrder verifies that every type only depends on types in
// its own group or an earlier one — the invariant the wasm type section
// requires.
func checkDependencyOrder(t *testing.T, table wasmTable) {
	t.Helper()
	groups := table.recGroups()
	checkGroupsCoverAll(t, groups, len(table))
	for i := range table {
		gi := groupOf(groups, i)
		for _, dep := range table[i].dependsOn() {
			gd := groupOf(groups, dep)
			if gd > gi {
				t.Errorf("type %d (%s, group %d) depends on type %d (%s, group %d): dependency emitted later",
					i, table[i].name, gi, dep, table[dep].name, gd)
			}
		}
	}
}

func TestPreludeRecGroups(t *testing.T) {
	table := wasmTable(preludeTypes())
	checkDependencyOrder(t, table)

	groups := table.recGroups()
	if len(groups) != numPreludeTypes {
		t.Fatalf("prelude produced %d groups, want %d (all prelude types are non-recursive singletons)", len(groups), numPreludeTypes)
	}
	for _, idx := range []int{typeGoObject, typeGoBytes, typeGoString} {
		if got := groupSize(groups, idx); got != 1 {
			t.Errorf("prelude type %d (%s) is in a group of size %d, want 1", idx, table[idx].name, got)
		}
	}
	// go.string references both go.object (super) and go.bytes (field),
	// so it must be emitted after both.
	if groupOf(groups, typeGoString) <= groupOf(groups, typeGoObject) {
		t.Errorf("go.string emitted before its supertype go.object")
	}
	if groupOf(groups, typeGoString) <= groupOf(groups, typeGoBytes) {
		t.Errorf("go.string emitted before its backing type go.bytes")
	}
}

func TestRecGroupsSelfRecursive(t *testing.T) {
	// type Point struct { next *Point }
	point := numPreludeTypes
	table := wasmTable(append(preludeTypes(), wasmType{
		name:  "go.Point",
		kind:  wasmStructType,
		super: typeGoObject,
		fields: []wasmField{
			{storage: ref(point, true), mutable: true}, // next *Point
		},
	}))
	checkDependencyOrder(t, table)

	groups := table.recGroups()
	if got := groupSize(groups, point); got != 1 {
		t.Fatalf("self-recursive Point is in a group of size %d, want 1", got)
	}
	if !table.selfRecursive(point) {
		t.Errorf("selfRecursive(Point) = false, want true: it must be emitted inside a rec group")
	}
	if table.selfRecursive(typeGoString) {
		t.Errorf("selfRecursive(go.string) = true, want false")
	}
}

func TestRecGroupsMutuallyRecursive(t *testing.T) {
	// type A struct { b *B }; type B struct { a *A }
	a := numPreludeTypes
	b := numPreludeTypes + 1
	table := wasmTable(append(preludeTypes(),
		wasmType{
			name:   "go.A",
			kind:   wasmStructType,
			super:  typeGoObject,
			fields: []wasmField{{storage: ref(b, true), mutable: true}},
		},
		wasmType{
			name:   "go.B",
			kind:   wasmStructType,
			super:  typeGoObject,
			fields: []wasmField{{storage: ref(a, true), mutable: true}},
		},
	))
	checkDependencyOrder(t, table)

	groups := table.recGroups()
	if groupOf(groups, a) != groupOf(groups, b) {
		t.Fatalf("mutually recursive A and B landed in different groups (%d, %d), want the same rec group",
			groupOf(groups, a), groupOf(groups, b))
	}
	if got := groupSize(groups, a); got != 2 {
		t.Errorf("A/B rec group has size %d, want 2", got)
	}
	// The cycle still subtypes go.object, so the shared group is emitted
	// after go.object's group.
	if groupOf(groups, a) <= groupOf(groups, typeGoObject) {
		t.Errorf("A/B rec group emitted before its supertype go.object")
	}
}

func TestRecGroupsChainOrdering(t *testing.T) {
	// A non-cyclic chain C -> B -> A: each is its own group, emitted in
	// dependency order A, B, C.
	a := numPreludeTypes
	b := numPreludeTypes + 1
	c := numPreludeTypes + 2
	table := wasmTable(append(preludeTypes(),
		wasmType{name: "go.A", kind: wasmStructType, super: typeGoObject},
		wasmType{
			name:   "go.B",
			kind:   wasmStructType,
			super:  typeGoObject,
			fields: []wasmField{{storage: ref(a, true)}},
		},
		wasmType{
			name:   "go.C",
			kind:   wasmStructType,
			super:  typeGoObject,
			fields: []wasmField{{storage: ref(b, true)}},
		},
	))
	checkDependencyOrder(t, table)

	groups := table.recGroups()
	if !(groupOf(groups, a) < groupOf(groups, b) && groupOf(groups, b) < groupOf(groups, c)) {
		t.Errorf("chain not in dependency order: A=%d B=%d C=%d, want A<B<C",
			groupOf(groups, a), groupOf(groups, b), groupOf(groups, c))
	}
	for _, idx := range []int{a, b, c} {
		if got := groupSize(groups, idx); got != 1 {
			t.Errorf("non-cyclic chain type %d is in a group of size %d, want 1", idx, got)
		}
		if table.selfRecursive(idx) {
			t.Errorf("selfRecursive(%d) = true, want false", idx)
		}
	}
}

func TestRecGroupsArrayElement(t *testing.T) {
	// []*Point: an (array (ref null $go.Point)) backing whose element
	// references a self-recursive struct. The array depends on Point;
	// Point depends on itself; they are distinct groups, array after.
	point := numPreludeTypes
	arr := numPreludeTypes + 1
	table := wasmTable(append(preludeTypes(),
		wasmType{
			name:   "go.Point",
			kind:   wasmStructType,
			super:  typeGoObject,
			fields: []wasmField{{storage: ref(point, true), mutable: true}},
		},
		wasmType{
			name:    "go.array.PtrPoint",
			kind:    wasmArrayType,
			super:   -1,
			elem:    ref(point, true),
			elemMut: true,
		},
	))
	checkDependencyOrder(t, table)

	groups := table.recGroups()
	if groupOf(groups, arr) == groupOf(groups, point) {
		t.Errorf("array and its non-mutually-recursive element type share a group")
	}
	if groupOf(groups, arr) <= groupOf(groups, point) {
		t.Errorf("array backing emitted before its element type go.Point")
	}
}

func TestCollectScalarStruct(t *testing.T) {
	// struct { a int64; b float64; c int32 }
	st := types.NewStruct([]*types.Field{
		field("a", types.Types[types.TINT64]),
		field("b", types.Types[types.TFLOAT64]),
		field("c", types.Types[types.TINT32]),
	})
	c := newTypeCollector()
	idx := c.collectStruct(st)

	if idx != numPreludeTypes {
		t.Fatalf("first collected struct got index %d, want %d (just past the prelude)", idx, numPreludeTypes)
	}
	got := c.table[idx]
	if got.kind != wasmStructType || got.super != typeGoObject {
		t.Fatalf("collected struct: kind=%d super=%d, want struct subtyping go.object", got.kind, got.super)
	}
	want := []wasmField{
		{storage: prim(wasmI64), mutable: true},
		{storage: prim(wasmF64), mutable: true},
		{storage: prim(wasmI32), mutable: true},
	}
	if len(got.fields) != len(want) {
		t.Fatalf("collected struct has %d fields, want %d", len(got.fields), len(want))
	}
	for i := range want {
		if got.fields[i] != want[i] {
			t.Errorf("field %d = %+v, want %+v", i, got.fields[i], want[i])
		}
	}
	checkDependencyOrder(t, c.table)
}

func TestCollectMemoization(t *testing.T) {
	st := types.NewStruct([]*types.Field{field("a", types.Types[types.TINT64])})
	c := newTypeCollector()
	first := c.collectStruct(st)
	n := len(c.table)
	second := c.collectStruct(st)
	if first != second {
		t.Errorf("collectStruct of the same type returned %d then %d", first, second)
	}
	if len(c.table) != n {
		t.Errorf("collectStruct of an already-collected type grew the table from %d to %d", n, len(c.table))
	}
}

func TestCollectStringField(t *testing.T) {
	// struct { s string } — the string flattens to {backing, offset, length}.
	st := types.NewStruct([]*types.Field{field("s", types.Types[types.TSTRING])})
	c := newTypeCollector()
	idx := c.collectStruct(st)

	want := []wasmField{
		{storage: ref(typeGoBytes, false), mutable: true},
		{storage: prim(wasmI32), mutable: true},
		{storage: prim(wasmI32), mutable: true},
	}
	got := c.table[idx].fields
	if len(got) != len(want) {
		t.Fatalf("struct{s string} lowered to %d fields, want %d (string flattens)", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("field %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCollectPointerToStruct(t *testing.T) {
	inner := types.NewStruct([]*types.Field{field("v", types.Types[types.TINT64])})
	outer := types.NewStruct([]*types.Field{field("p", types.NewPtr(inner))})
	c := newTypeCollector()
	idx := c.collectStruct(outer)

	if len(c.table[idx].fields) != 1 {
		t.Fatalf("struct{p *inner} has %d fields, want 1", len(c.table[idx].fields))
	}
	f := c.table[idx].fields[0]
	if !f.storage.isRef() || !f.storage.refNull {
		t.Fatalf("pointer field storage = %+v, want a nullable reference", f.storage)
	}
	pointee := c.table[f.storage.refType]
	if pointee.kind != wasmStructType || pointee.super != typeGoObject {
		t.Errorf("pointee type = %+v, want a struct subtyping go.object", pointee)
	}
	checkDependencyOrder(t, c.table)
}

func TestCollectPointerToScalarBoxes(t *testing.T) {
	// struct { p *int64 } — a pointer to a scalar references a boxed wrapper.
	st := types.NewStruct([]*types.Field{field("p", types.NewPtr(types.Types[types.TINT64]))})
	c := newTypeCollector()
	idx := c.collectStruct(st)

	f := c.table[idx].fields[0]
	if !f.storage.isRef() {
		t.Fatalf("*int64 field storage = %+v, want a reference", f.storage)
	}
	box := c.table[f.storage.refType]
	if box.kind != wasmStructType || len(box.fields) != 1 || box.fields[0].storage != prim(wasmI64) {
		t.Errorf("boxed scalar = %+v, want a one-i64-field struct", box)
	}
	checkDependencyOrder(t, c.table)
}

func TestCollectRecursiveStruct(t *testing.T) {
	// type Point struct { x, y float64; next *Point }
	point := namedStruct("Point", func(self *types.Type) []*types.Field {
		return []*types.Field{
			field("x", types.Types[types.TFLOAT64]),
			field("y", types.Types[types.TFLOAT64]),
			field("next", types.NewPtr(self)),
		}
	})
	c := newTypeCollector()
	idx := c.collectStruct(point)

	// The recursive collect must terminate and the next field must
	// reference the struct's own index.
	fields := c.table[idx].fields
	if len(fields) != 3 {
		t.Fatalf("Point lowered to %d fields, want 3", len(fields))
	}
	if fields[2].storage.refType != idx {
		t.Errorf("Point.next references type %d, want self (%d)", fields[2].storage.refType, idx)
	}
	if !c.table.selfRecursive(idx) {
		t.Errorf("selfRecursive(Point) = false, want true")
	}
	checkDependencyOrder(t, c.table)

	// Collecting Point again is memoized: no new entry.
	n := len(c.table)
	if again := c.collectStruct(point); again != idx || len(c.table) != n {
		t.Errorf("re-collecting Point grew/changed the table")
	}
}

func TestCollectMutuallyRecursiveStructs(t *testing.T) {
	// type A struct { b *B }; type B struct { a *A }
	var b *types.Type
	a := namedStruct("A", func(self *types.Type) []*types.Field {
		b = namedStruct("B", func(bSelf *types.Type) []*types.Field {
			return []*types.Field{field("a", types.NewPtr(self))}
		})
		return []*types.Field{field("b", types.NewPtr(b))}
	})
	c := newTypeCollector()
	ai := c.collectStruct(a)
	bi := c.collectStruct(b)
	checkDependencyOrder(t, c.table)

	groups := c.table.recGroups()
	if groupOf(groups, ai) != groupOf(groups, bi) {
		t.Errorf("mutually recursive A (%d) and B (%d) landed in different rec groups", ai, bi)
	}
}
