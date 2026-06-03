// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// Lock-free stack.

package runtime

import "unsafe"

// wasm3 mirror of lfstack.go.
//
// lfstack.go is tagged !wasm3 because its push / pop / pack /
// unpack operations stuff an unsafe.Pointer + tag into a uint64
// via taggedPointerPack (lfstack.go:79) — anyref has no scalar
// representation that can be packed alongside a tag in 64 bits.
//
// On wasm3 every direct user of lfstack — mcleanup.go (cleanup
// queue), mgc.go (work buffers), mspanset.go (free-span lists) —
// is already gated !wasm3 because the GC subsystem is dead under
// the host WasmGC. Nothing reachable from the wasm3 build invokes
// the lfstack methods, so the mirror is a thin set of trap stubs
// that exists only so the symbols resolve and so unsafe.Sizeof(
// lfstack(0)) reads the same in any leftover layout calculation
// (e.g. cache-line padding in mcleanup struct embeddings).
//
// If a wasm3 caller of lfstack ever surfaces, the right move is
// to give it a real single-threaded singly-linked-list backing —
// no atomicity needed, but the body would still skip the pointer-
// in-uint64 packing entirely (just store the *lfnode in a Go-typed
// field).

type lfstack uint64

//go:nosplit
func (head *lfstack) push(node *lfnode) {
	throw("runtime.lfstack.push unreachable on wasm3 (GC subsystem gated)")
}

//go:nosplit
func (head *lfstack) pop() unsafe.Pointer {
	throw("runtime.lfstack.pop unreachable on wasm3 (GC subsystem gated)")
	return nil
}

//go:nosplit
func (head *lfstack) empty() bool {
	// Used by some idle / drain checks before the throw'ing push/pop;
	// keep it cheap and consistent with the zero-value semantics.
	return uint64(*head) == 0
}

//go:nosplit
func lfnodeValidate(node *lfnode) {
	// No-op: wasm3 doesn't allocate lfnodes from the heap (no GC
	// subsystem to confuse), and the upstream check would call
	// findObject which doesn't apply on wasm3.
}

func lfstackPack(node *lfnode, cnt uintptr) uint64 {
	throw("runtime.lfstackPack unreachable on wasm3")
	return 0
}

func lfstackUnpack(val uint64) *lfnode {
	throw("runtime.lfstackUnpack unreachable on wasm3")
	return nil
}
