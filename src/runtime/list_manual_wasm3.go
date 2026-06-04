// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import (
	"unsafe"
)

// wasm3 mirror of list_manual.go.
//
// The non-wasm3 file stores list head / prev / next as uintptr
// for write-barrier bypass — the same idiom guintptr/muintptr/
// puintptr use, and the same one [[wasm3-scheduler-intptr-rung1-done]]
// adapted. On wasm3 the bypass is moot (host WasmGC tracks every
// ref slot) and impossible (anyref has no scalar form).
//
// Adapt the fields to unsafe.Pointer and replace all
// uintptr↔unsafe.Pointer round-trips with direct pointer
// assignment. Struct layout changes from the !wasm3 file
// (unsafe.Pointer is a single anyref slot, uintptr is i64) —
// callers in runtime2.go's m / sched structs see a different
// layout on wasm3, same convention as guintptr/muintptr.

type listHeadManual struct {
	obj unsafe.Pointer

	initialized bool
	nodeOffset  uintptr
}

func (head *listHeadManual) init(off uintptr) {
	head.initialized = true
	head.nodeOffset = off
}

type listNodeManual struct {
	prev unsafe.Pointer
	next unsafe.Pointer
}

func (head *listHeadManual) getNode(p unsafe.Pointer) *listNodeManual {
	if !head.initialized {
		throw("runtime: uninitialized listHead")
	}

	if p == nil {
		return nil
	}
	// On wasm3 the intrusive-list pattern unsafe.Add(p, nodeOffset)
	// produces malformed wasm: the SSA-gen lowers OUNSAFEADD to a
	// pointer+uintptr arithmetic that turns into an i64.add with an
	// anyref operand, which V8 rejects at validation. Trap until the
	// few call sites (lockedExtraM / extraM / etc.) are reshaped to
	// either embed the node directly as a field or use a wasm3-aware
	// container helper. Most paths reaching listHeadManual on wasm3
	// are gated out by the Rung-29 mirror leak cleanup, so this trap
	// fires only when an unmigrated call sneaks through.
	throw("runtime.listHeadManual.getNode not supported on wasm3 (unsafe.Add ptr-arith path)")
	return nil
}

func (head *listHeadManual) empty() bool {
	return head.obj == nil
}

func (head *listHeadManual) head() unsafe.Pointer {
	return head.obj
}

func (head *listHeadManual) push(p unsafe.Pointer) {
	pNode := head.getNode(p)
	pNode.next = head.obj

	if head.obj != nil {
		headNode := head.getNode(head.obj)
		headNode.prev = p
	}

	head.obj = p
}

func (head *listHeadManual) pop() unsafe.Pointer {
	if head.obj == nil {
		return nil
	}

	p := head.obj

	pNode := head.getNode(p)
	head.obj = pNode.next
	pNode.next = nil

	if head.obj != nil {
		headNode := head.getNode(head.obj)
		headNode.prev = nil
	}

	return p
}

func (head *listHeadManual) remove(p unsafe.Pointer) {
	if head.obj == p {
		head.pop()
		return
	}

	pNode := head.getNode(p)
	prevNode := head.getNode(pNode.prev)
	nextNode := head.getNode(pNode.next)

	if prevNode != nil {
		prevNode.next = pNode.next
	}
	if nextNode != nil {
		nextNode.prev = pNode.prev
	}

	pNode.prev = nil
	pNode.next = nil
}
