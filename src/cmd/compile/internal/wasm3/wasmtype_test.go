// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm3

import "testing"

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
