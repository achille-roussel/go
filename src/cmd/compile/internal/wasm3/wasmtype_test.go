// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm3

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ssagen"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/obj/wasm"
	"cmd/internal/src"
	"cmd/internal/wasmgc"
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
// type at table index typeIdx, or -1 if no group does.
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

// checkDependencyOrder verifies that every collected type only depends
// on types in its own recursion group or an earlier one — the invariant
// the wasm type section requires. It mirrors the same check in the
// cmd/internal/wasmgc tests, applied here to tables the collector built.
func checkDependencyOrder(t *testing.T, table wasmgc.Table) {
	t.Helper()
	groups := table.RecGroups()
	seen := make([]bool, len(table))
	count := 0
	for _, group := range groups {
		for _, idx := range group {
			if idx < 0 || idx >= len(table) {
				t.Fatalf("group member %d out of range [0,%d)", idx, len(table))
			}
			if seen[idx] {
				t.Fatalf("type %d appears in more than one group", idx)
			}
			seen[idx] = true
			count++
		}
	}
	if count != len(table) {
		t.Fatalf("groups cover %d types, want %d", count, len(table))
	}
	for i := range table {
		gi := groupOf(groups, i)
		for _, dep := range table[i].DependsOn() {
			if gd := groupOf(groups, dep); gd > gi {
				t.Errorf("type %d (%s, group %d) depends on type %d (%s, group %d): dependency emitted later",
					i, table[i].Name, gi, dep, table[dep].Name, gd)
			}
		}
	}
}

// wrapModule wraps a type-section payload in an otherwise empty but
// valid WebAssembly module so the encoded table can be validated.
func wrapModule(payload []byte) []byte {
	mod := []byte{
		0x00, 0x61, 0x73, 0x6d, // \0asm
		0x01, 0x00, 0x00, 0x00, // version 1
	}
	mod = append(mod, wasmgc.SectionType)
	mod = wasmgc.AppendUleb(mod, uint64(len(payload)))
	return append(mod, payload...)
}

// validateModule runs `wasm-tools validate --features gc` on mod,
// skipping the test if wasm-tools is not installed.
func validateModule(t *testing.T, name string, mod []byte) {
	t.Helper()
	tool, err := exec.LookPath("wasm-tools")
	if err != nil {
		t.Skip("wasm-tools not found in PATH; skipping module validation")
	}
	path := filepath.Join(t.TempDir(), name+".wasm")
	if err := os.WriteFile(path, mod, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(tool, "validate", "--features", "gc", path).CombinedOutput(); err != nil {
		t.Fatalf("wasm-tools validate failed: %v\n%s\nmodule bytes: % x", err, out, mod)
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

	if idx != wasmgc.NumPreludeTypes {
		t.Fatalf("first collected struct got index %d, want %d (just past the prelude)", idx, wasmgc.NumPreludeTypes)
	}
	got := c.table[idx]
	if got.Kind != wasmgc.KindStruct || got.Super != wasmgc.TypeGoObject {
		t.Fatalf("collected struct: kind=%d super=%d, want struct subtyping go.object", got.Kind, got.Super)
	}
	want := []wasmgc.Field{
		{Storage: wasmgc.PrimStorage(wasmgc.I64), Mutable: true},
		{Storage: wasmgc.PrimStorage(wasmgc.F64), Mutable: true},
		{Storage: wasmgc.PrimStorage(wasmgc.I32), Mutable: true},
	}
	if len(got.Fields) != len(want) {
		t.Fatalf("collected struct has %d fields, want %d", len(got.Fields), len(want))
	}
	for i := range want {
		if got.Fields[i] != want[i] {
			t.Errorf("field %d = %+v, want %+v", i, got.Fields[i], want[i])
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

	want := []wasmgc.Field{
		{Storage: wasmgc.RefStorage(wasmgc.TypeGoBytes, false), Mutable: true},
		{Storage: wasmgc.PrimStorage(wasmgc.I32), Mutable: true},
		{Storage: wasmgc.PrimStorage(wasmgc.I32), Mutable: true},
	}
	got := c.table[idx].Fields
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

	if len(c.table[idx].Fields) != 1 {
		t.Fatalf("struct{p *inner} has %d fields, want 1", len(c.table[idx].Fields))
	}
	f := c.table[idx].Fields[0]
	if !f.Storage.IsRef() || !f.Storage.RefNull {
		t.Fatalf("pointer field storage = %+v, want a nullable reference", f.Storage)
	}
	pointee := c.table[f.Storage.RefType]
	if pointee.Kind != wasmgc.KindStruct || pointee.Super != wasmgc.TypeGoObject {
		t.Errorf("pointee type = %+v, want a struct subtyping go.object", pointee)
	}
	checkDependencyOrder(t, c.table)
}

func TestCollectPointerToScalarBoxes(t *testing.T) {
	// struct { p *int64 } — a pointer to a scalar references a boxed wrapper.
	st := types.NewStruct([]*types.Field{field("p", types.NewPtr(types.Types[types.TINT64]))})
	c := newTypeCollector()
	idx := c.collectStruct(st)

	f := c.table[idx].Fields[0]
	if !f.Storage.IsRef() {
		t.Fatalf("*int64 field storage = %+v, want a reference", f.Storage)
	}
	box := c.table[f.Storage.RefType]
	if box.Kind != wasmgc.KindStruct || len(box.Fields) != 1 || box.Fields[0].Storage != wasmgc.PrimStorage(wasmgc.I64) {
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
	fields := c.table[idx].Fields
	if len(fields) != 3 {
		t.Fatalf("Point lowered to %d fields, want 3", len(fields))
	}
	if fields[2].Storage.RefType != idx {
		t.Errorf("Point.next references type %d, want self (%d)", fields[2].Storage.RefType, idx)
	}
	if !c.table.SelfRecursive(idx) {
		t.Errorf("SelfRecursive(Point) = false, want true")
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

	groups := c.table.RecGroups()
	if groupOf(groups, ai) != groupOf(groups, bi) {
		t.Errorf("mutually recursive A (%d) and B (%d) landed in different rec groups", ai, bi)
	}
}

// TestCollectArrayField locks in the M3 Stage D representation:
// a Go `[N]T` array field inside a struct lowers to a single
// `(ref (array T))` field, regardless of N. The pre-Stage-D
// representation unrolled into N*len(elem) flat fields, which
// blew past wasmparser's ~10000-field engine limit for struct
// types containing large buffers (the go-test wasip1 binary
// blocker). One representation for all arrays — see
// doc/wasm3-m3-notes.md.
func TestCollectArrayField(t *testing.T) {
	for _, n := range []int64{1, 4, 256, 65504} {
		t.Run(typeName(types.Types[types.TINT32]), func(t *testing.T) {
			st := types.NewStruct([]*types.Field{
				field("buf", types.NewArray(types.Types[types.TINT32], n)),
				field("tag", types.Types[types.TINT64]),
			})
			c := newTypeCollector()
			idx := c.collectStruct(st)
			got := c.table[idx]
			if got.Kind != wasmgc.KindStruct {
				t.Fatalf("collected struct kind=%d, want struct", got.Kind)
			}
			if len(got.Fields) != 2 {
				t.Fatalf("collected struct for [%d]int32: got %d fields, want 2 (one ref to array + one i64 tag)", n, len(got.Fields))
			}
			refField := got.Fields[0]
			if !refField.Storage.IsRef() {
				t.Errorf("buf field storage = %+v, want a ref storage", refField.Storage)
			}
			if backing := c.table[refField.Storage.RefType]; backing.Kind != wasmgc.KindArray {
				t.Errorf("buf field refers to type[%d] kind=%d, want array", refField.Storage.RefType, backing.Kind)
			}
			if got.Fields[1].Storage != (wasmgc.PrimStorage(wasmgc.I64)) {
				t.Errorf("tag field storage = %+v, want i64", got.Fields[1].Storage)
			}
			checkDependencyOrder(t, c.table)
		})
	}
}

// TestCollectArrayBackingPacked locks in that array backings for
// sub-i32 element kinds use packed wasmgc storage (i8 for
// byte/int8/bool, i16 for int16/uint16) rather than rounding up
// to i32. This is what makes the wasmgc <-> linear-memory bridge
// work for byte arrays: a `[N]byte` lays out as `(array (mut i8))`,
// the wasm3 backend emits `array.get_u` / `array.set` on i32, and
// the read elements can be stored into linear-memory scratch with
// `i32.store8` for fd_write to consume.
func TestCollectArrayBackingPacked(t *testing.T) {
	cases := []struct {
		kind    types.Kind
		want    wasmgc.Prim
		wantStr string
	}{
		{types.TUINT8, wasmgc.I8, "i8"},
		{types.TINT8, wasmgc.I8, "i8"},
		{types.TBOOL, wasmgc.I8, "i8"},
		{types.TINT16, wasmgc.I16, "i16"},
		{types.TUINT16, wasmgc.I16, "i16"},
		{types.TINT32, wasmgc.I32, "i32"},
		{types.TUINT32, wasmgc.I32, "i32"},
		{types.TINT64, wasmgc.I64, "i64"},
		{types.TFLOAT64, wasmgc.F64, "f64"},
	}
	for _, tc := range cases {
		t.Run(tc.wantStr, func(t *testing.T) {
			c := newTypeCollector()
			idx := c.collectBacking(types.Types[tc.kind])
			got := c.table[idx]
			if got.Kind != wasmgc.KindArray {
				t.Fatalf("backing for %v: got kind=%d, want array", tc.kind, got.Kind)
			}
			if got.Elem.IsRef() {
				t.Fatalf("backing for %v: got ref storage, want primitive %s", tc.kind, tc.wantStr)
			}
			if got.Elem.Prim != tc.want {
				t.Errorf("backing for %v: got prim=%d, want %s (%d)", tc.kind, got.Elem.Prim, tc.wantStr, tc.want)
			}
		})
	}
}

// TestCollectSliceStruct locks in the boxed slice header
// (doc/wasm3-slice-boxing.md): a Go []T lowers to a 4-field struct
// (data (ref go.array.T), off i32, len i32, cap i32) subtyping
// go.object, with the backing array collected first so the data field
// can reference it. All fields are mutable so the backend may rewrite
// a header in place.
func TestCollectSliceStruct(t *testing.T) {
	sl := types.NewSlice(types.Types[types.TINT32])
	c := newTypeCollector()
	idx := c.collectSliceStruct(sl)

	got := c.table[idx]
	if got.Kind != wasmgc.KindStruct || got.Super != wasmgc.TypeGoObject {
		t.Fatalf("slice header: kind=%d super=%d, want struct subtyping go.object", got.Kind, got.Super)
	}
	if len(got.Fields) != 4 {
		t.Fatalf("slice header has %d fields, want 4 (data, off, len, cap)", len(got.Fields))
	}
	data := got.Fields[0]
	if !data.Storage.IsRef() || !data.Mutable {
		t.Fatalf("data field = %+v, want a mutable ref", data)
	}
	if backing := c.table[data.Storage.RefType]; backing.Kind != wasmgc.KindArray {
		t.Errorf("data field refers to type[%d] kind=%d, want array", data.Storage.RefType, backing.Kind)
	}
	for i, name := range []string{"off", "len", "cap"} {
		f := got.Fields[1+i]
		if f.Storage != wasmgc.PrimStorage(wasmgc.I32) || !f.Mutable {
			t.Errorf("%s field = %+v, want mutable i32", name, f)
		}
	}
	checkDependencyOrder(t, c.table)

	// Memoized: re-collecting the same slice type returns the same index
	// and does not grow the table.
	n := len(c.table)
	if again := c.collectSliceStruct(sl); again != idx || len(c.table) != n {
		t.Errorf("re-collecting []int32 grew/changed the table (idx %d->%d, len %d->%d)", idx, again, n, len(c.table))
	}
}

// TestCollectSliceStructByteBacking confirms []byte's boxed header
// reuses a packed (array (mut i8)) backing — the same go.bytes shape
// strings use — so the slice ref can flow to the host boundary over a
// byte-tight array.
func TestCollectSliceStructByteBacking(t *testing.T) {
	sl := types.NewSlice(types.Types[types.TUINT8])
	c := newTypeCollector()
	idx := c.collectSliceStruct(sl)
	data := c.table[idx].Fields[0]
	backing := c.table[data.Storage.RefType]
	if backing.Kind != wasmgc.KindArray {
		t.Fatalf("[]byte backing kind=%d, want array", backing.Kind)
	}
	if backing.Elem.IsRef() || backing.Elem.Prim != wasmgc.I8 {
		t.Errorf("[]byte backing elem = %+v, want packed i8", backing.Elem)
	}
	checkDependencyOrder(t, c.table)
}

func TestEncodedCollectedTypesValidate(t *testing.T) {
	// A program-like mix: a recursive struct, a struct with a string
	// and a slice, mutually recursive structs, and a pointer to a
	// scalar. The whole collected table must encode to a valid GC type
	// section.
	point := namedStruct("Point", func(self *types.Type) []*types.Field {
		return []*types.Field{
			field("x", types.Types[types.TFLOAT64]),
			field("next", types.NewPtr(self)),
		}
	})
	var b *types.Type
	a := namedStruct("A", func(self *types.Type) []*types.Field {
		b = namedStruct("B", func(*types.Type) []*types.Field {
			return []*types.Field{field("a", types.NewPtr(self))}
		})
		return []*types.Field{field("b", types.NewPtr(b))}
	})
	mixed := types.NewStruct([]*types.Field{
		field("name", types.Types[types.TSTRING]),
		field("nums", types.NewSlice(types.Types[types.TINT64])),
		field("count", types.NewPtr(types.Types[types.TINT32])),
		field("origin", types.NewPtr(point)),
	})

	c := newTypeCollector()
	c.collectStruct(point)
	c.collectStruct(a)
	c.collectStruct(b)
	c.collectStruct(mixed)

	checkDependencyOrder(t, c.table)
	validateModule(t, "collected", wrapModule(c.table.EncodeTypeSection()))
}
