// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasmgc

import "testing"

// groupOf returns the ordinal of the recursion group that contains the
// type at table index typeIdx, or -1 if no group does. The ordinal is
// the group's position in the RecGroups result, which is also its emit
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

// CheckDependencyOrder verifies that every type only depends on types in
// its own group or an earlier one — the invariant the wasm type section
// requires. It is exported so the compiler's wasm3 collector tests can
// reuse it.
func CheckDependencyOrder(t *testing.T, table Table) {
	t.Helper()
	groups := table.RecGroups()
	checkGroupsCoverAll(t, groups, len(table))
	for i := range table {
		gi := groupOf(groups, i)
		for _, dep := range table[i].DependsOn() {
			gd := groupOf(groups, dep)
			if gd > gi {
				t.Errorf("type %d (%s, group %d) depends on type %d (%s, group %d): dependency emitted later",
					i, table[i].Name, gi, dep, table[dep].Name, gd)
			}
		}
	}
}

func TestPreludeRecGroups(t *testing.T) {
	table := Table(PreludeTypes())
	CheckDependencyOrder(t, table)

	groups := table.RecGroups()
	if len(groups) != NumPreludeTypes {
		t.Fatalf("prelude produced %d groups, want %d (all prelude types are non-recursive singletons)", len(groups), NumPreludeTypes)
	}
	for _, idx := range []int{
		TypeGoObject, TypeGoBytes, TypeGoString,
		TypeGoIptrI8, TypeGoIptrI16, TypeGoIptrI32, TypeGoIptrI64,
		TypeGoIptrF32, TypeGoIptrF64, TypeGoIptrRef,
	} {
		if got := groupSize(groups, idx); got != 1 {
			t.Errorf("prelude type %d (%s) is in a group of size %d, want 1", idx, table[idx].Name, got)
		}
	}
	// go.string references both go.object (super) and go.bytes (field),
	// so it must be emitted after both.
	if groupOf(groups, TypeGoString) <= groupOf(groups, TypeGoObject) {
		t.Errorf("go.string emitted before its supertype go.object")
	}
	if groupOf(groups, TypeGoString) <= groupOf(groups, TypeGoBytes) {
		t.Errorf("go.string emitted before its backing type go.bytes")
	}
}

func TestRecGroupsSelfRecursive(t *testing.T) {
	// type Point struct { next *Point }
	point := NumPreludeTypes
	table := Table(append(PreludeTypes(), Type{
		Name:  "go.Point",
		Kind:  KindStruct,
		Super: TypeGoObject,
		Fields: []Field{
			{Storage: RefStorage(point, true), Mutable: true}, // next *Point
		},
	}))
	CheckDependencyOrder(t, table)

	groups := table.RecGroups()
	if got := groupSize(groups, point); got != 1 {
		t.Fatalf("self-recursive Point is in a group of size %d, want 1", got)
	}
	if !table.SelfRecursive(point) {
		t.Errorf("SelfRecursive(Point) = false, want true: it must be emitted inside a rec group")
	}
	if table.SelfRecursive(TypeGoString) {
		t.Errorf("SelfRecursive(go.string) = true, want false")
	}
}

func TestRecGroupsMutuallyRecursive(t *testing.T) {
	// type A struct { b *B }; type B struct { a *A }
	a := NumPreludeTypes
	b := NumPreludeTypes + 1
	table := Table(append(PreludeTypes(),
		Type{
			Name:   "go.A",
			Kind:   KindStruct,
			Super:  TypeGoObject,
			Fields: []Field{{Storage: RefStorage(b, true), Mutable: true}},
		},
		Type{
			Name:   "go.B",
			Kind:   KindStruct,
			Super:  TypeGoObject,
			Fields: []Field{{Storage: RefStorage(a, true), Mutable: true}},
		},
	))
	CheckDependencyOrder(t, table)

	groups := table.RecGroups()
	if groupOf(groups, a) != groupOf(groups, b) {
		t.Fatalf("mutually recursive A and B landed in different groups (%d, %d), want the same rec group",
			groupOf(groups, a), groupOf(groups, b))
	}
	if got := groupSize(groups, a); got != 2 {
		t.Errorf("A/B rec group has size %d, want 2", got)
	}
	// The cycle still subtypes go.object, so the shared group is emitted
	// after go.object's group.
	if groupOf(groups, a) <= groupOf(groups, TypeGoObject) {
		t.Errorf("A/B rec group emitted before its supertype go.object")
	}
}

func TestRecGroupsChainOrdering(t *testing.T) {
	// A non-cyclic chain C -> B -> A: each is its own group, emitted in
	// dependency order A, B, C.
	a := NumPreludeTypes
	b := NumPreludeTypes + 1
	c := NumPreludeTypes + 2
	table := Table(append(PreludeTypes(),
		Type{Name: "go.A", Kind: KindStruct, Super: TypeGoObject},
		Type{
			Name:   "go.B",
			Kind:   KindStruct,
			Super:  TypeGoObject,
			Fields: []Field{{Storage: RefStorage(a, true)}},
		},
		Type{
			Name:   "go.C",
			Kind:   KindStruct,
			Super:  TypeGoObject,
			Fields: []Field{{Storage: RefStorage(b, true)}},
		},
	))
	CheckDependencyOrder(t, table)

	groups := table.RecGroups()
	if !(groupOf(groups, a) < groupOf(groups, b) && groupOf(groups, b) < groupOf(groups, c)) {
		t.Errorf("chain not in dependency order: A=%d B=%d C=%d, want A<B<C",
			groupOf(groups, a), groupOf(groups, b), groupOf(groups, c))
	}
	for _, idx := range []int{a, b, c} {
		if got := groupSize(groups, idx); got != 1 {
			t.Errorf("non-cyclic chain type %d is in a group of size %d, want 1", idx, got)
		}
		if table.SelfRecursive(idx) {
			t.Errorf("SelfRecursive(%d) = true, want false", idx)
		}
	}
}

func TestRecGroupsArrayElement(t *testing.T) {
	// []*Point: an (array (ref null $go.Point)) backing whose element
	// references a self-recursive struct. The array depends on Point;
	// Point depends on itself; they are distinct groups, array after.
	point := NumPreludeTypes
	arr := NumPreludeTypes + 1
	table := Table(append(PreludeTypes(),
		Type{
			Name:   "go.Point",
			Kind:   KindStruct,
			Super:  TypeGoObject,
			Fields: []Field{{Storage: RefStorage(point, true), Mutable: true}},
		},
		Type{
			Name:    "go.array.PtrPoint",
			Kind:    KindArray,
			Super:   -1,
			Elem:    RefStorage(point, true),
			ElemMut: true,
		},
	))
	CheckDependencyOrder(t, table)

	groups := table.RecGroups()
	if groupOf(groups, arr) == groupOf(groups, point) {
		t.Errorf("array and its non-mutually-recursive element type share a group")
	}
	if groupOf(groups, arr) <= groupOf(groups, point) {
		t.Errorf("array backing emitted before its element type go.Point")
	}
}
