// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm3

package runtime

import "unsafe"

func interequal(p, q unsafe.Pointer) bool {
	x := *(*iface)(p)
	y := *(*iface)(q)
	return x.tab == y.tab && ifaceeq(x.tab, x.data, y.data)
}
func nilinterequal(p, q unsafe.Pointer) bool {
	x := *(*eface)(p)
	y := *(*eface)(q)
	return x._type == y._type && efaceeq(x._type, x.data, y.data)
}
func efaceeq(t *_type, x, y unsafe.Pointer) bool {
	if t == nil {
		return true
	}
	eq := t.Equal
	if eq == nil {
		panic(errorString("comparing uncomparable type " + toRType(t).string()))
	}
	if t.IsDirectIface() {
		// Direct interface types are ptr, chan, map, func, and single-element structs/arrays thereof.
		// Maps and funcs are not comparable, so they can't reach here.
		// Ptrs, chans, and single-element items can be compared directly using ==.
		return x == y
	}
	return eq(x, y)
}
func ifaceeq(tab *itab, x, y unsafe.Pointer) bool {
	if tab == nil {
		return true
	}
	t := tab.Type
	eq := t.Equal
	if eq == nil {
		panic(errorString("comparing uncomparable type " + toRType(t).string()))
	}
	if t.IsDirectIface() {
		// See comment in efaceeq.
		return x == y
	}
	return eq(x, y)
}
