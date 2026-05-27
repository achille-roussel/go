// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// Phase 4 prototype: a userspace concurrent-goroutine substrate
// over JSPI. Does NOT integrate with the standard Go scheduler.
// `wasm.Spawn(fn)` creates a JSPI suspender that runs fn
// concurrently with the calling activation.
//
// Mechanism: Spawn enqueues fn onto a FIFO of pending spawns, then
// asks the JS host (via the SpawnGoroutine import) to schedule a
// new promising-wrapped call to the goroutine_run wasm export. JS
// calls `WebAssembly.promising(instance.exports.goroutine_run)(0)`;
// each such call is its own JSPI suspender stack. goroutine_run
// dequeues the next pending fn and invokes it.
//
// The queue lets a tight `for { go worker() }` loop spawn N
// goroutines before any of them runs — every spawn lands on the
// queue, every queueMicrotask'd goroutine_run consumes one.

package wasm

// spawnNode is one entry on the pending-spawn FIFO. fn is the
// goroutine entry point; next chains nodes in spawn order.
type spawnNode struct {
	fn   func()
	next *spawnNode
}

// Pending spawn FIFO. head is the oldest pending spawn (next to
// run); tail is the newest (where Spawn appends). nil = empty.
//
// Single-threaded: wasm is single-activation under JSPI (a
// suspender either holds the engine or is suspended on a Promise
// — never two at once), so Spawn and goroutineRun never race.
var (
	spawnHead *spawnNode
	spawnTail *spawnNode
)

// Spawn schedules fn to run as a new JSPI-goroutine. fn runs in
// its own suspender stack the next time the JS event loop runs
// the queued promising call to goroutine_run.
//
// //go:noinline so the queue mutations and the wasmSpawn host call
// survive inlining as a coherent block — the only reader is
// goroutineRun via the //go:wasmexport entry, which the inliner
// doesn't see as a use of the FIFO.
//
//go:noinline
func Spawn(fn func()) {
	n := &spawnNode{fn: fn}
	if spawnTail == nil {
		spawnHead = n
	} else {
		spawnTail.next = n
	}
	spawnTail = n
	wasmSpawn(0)
}

// wasmSpawn tells the JS host to schedule a new promising call to
// goroutine_run(id). Non-suspending — returns immediately.
//
//go:wasmimport gojs runtime.SpawnGoroutine
//go:noescape
func wasmSpawn(id int32)

// goroutineRun is the entry point JS calls for each spawn. It
// dequeues the next pending fn and invokes it. Exposed as a
// promising-wrapped export by the JS shim.
//
// //go:noinline because the dequeue dance must survive across the
// JSPI activation boundary as a single logical block.
//
//go:wasmexport goroutine_run
//go:noinline
func goroutineRun(id int32) {
	if spawnHead == nil {
		return
	}
	n := spawnHead
	spawnHead = n.next
	if spawnHead == nil {
		spawnTail = nil
	}
	n.next = nil // drop the reference so the node can be collected.
	n.fn()
}
