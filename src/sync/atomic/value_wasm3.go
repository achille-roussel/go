// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package atomic

// A Value provides an atomic load and store of a consistently typed value.
// The zero value for a Value returns nil from [Value.Load].
//
// The general implementation (value.go) reinterprets the stored interface
// as two unsafe.Pointer words (efaceWords) and CASes them. On wasm3 an
// interface value is a single WasmGC reference ($go.iface), not two
// linear-memory words, so that representation does not apply. wasm3 M2 is
// single-goroutine (real goroutines/atomics arrive with M4, see
// doc/wasm3-design.md), so these operate on the boxed interface ref
// directly and are not yet atomic.
type Value struct {
	v any
}

// Load returns the value set by the most recent Store.
// It returns nil if there has been no call to Store for this Value.
func (v *Value) Load() (val any) {
	return v.v
}

// Store sets the value of the [Value] v to val.
// Store of nil panics.
func (v *Value) Store(val any) {
	if val == nil {
		panic("sync/atomic: store of nil value into Value")
	}
	v.v = val
}

// Swap stores new into Value and returns the previous value. It returns
// nil if the Value is empty.
func (v *Value) Swap(new any) (old any) {
	if new == nil {
		panic("sync/atomic: swap of nil value into Value")
	}
	old = v.v
	v.v = new
	return old
}

// CompareAndSwap executes the compare-and-swap operation for the [Value].
func (v *Value) CompareAndSwap(old, new any) (swapped bool) {
	if new == nil {
		panic("sync/atomic: compare and swap of nil value into Value")
	}
	if v.v != old {
		return false
	}
	v.v = new
	return true
}
