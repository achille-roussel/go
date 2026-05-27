// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// Phase 4 prototype: a userspace concurrent-goroutine substrate
// over JSPI. Does NOT integrate with the standard Go scheduler.
// `wasm.Spawn(fn)` creates a JSPI suspender that runs fn
// concurrently with the calling activation.
//
// Mechanism: Spawn stashes fn in a global slot, then asks the JS
// host (via the SpawnGoroutine import) to schedule a new
// promising-wrapped call to the goroutine_run wasm export. JS
// calls `WebAssembly.promising(instance.exports.goroutine_run)(0)`;
// each such call is its own JSPI suspender stack.
//
// Phase 4 minimum is one-pending-spawn (single global slot — slot
// arrays of func() values currently hit wasm3 backend codegen bugs
// around local.tee with ref-typed elements). The full bag-of-
// stacks design lands once the codegen for ref-array indexing is
// fixed.

package wasm

var spawnSlot func()

// Spawn schedules fn to run as a new JSPI-goroutine. fn runs in its
// own suspender stack the next time the JS event loop runs the
// queued promising call.
//
// Phase 4 minimum constraint: only one un-started Spawn may be
// pending at a time. The caller must let the previous Spawn's
// goroutine_run be invoked before Spawning again. In practice this
// is fine because SpawnGoroutine queueMicrotask's the runner
// immediately and the only way the caller can interfere is by
// also Spawning before yielding — don't.
//
// //go:noinline because the spawnSlot=fn assignment must survive
// inlining — the only reader is goroutineRun via the //go:wasmexport
// entry, which the inliner doesn't see as a use of spawnSlot.
//
//go:noinline
func Spawn(fn func()) {
	spawnSlot = fn
	wasmSpawn(0)
}

// wasmSpawn tells the JS host to schedule a new promising call to
// goroutine_run(id). Non-suspending — returns immediately.
//
//go:wasmimport gojs runtime.SpawnGoroutine
//go:noescape
func wasmSpawn(id int32)

// goroutineRun is the entry point JS calls for each spawn. It picks
// up the registered entry function and invokes it. Exposed as a
// promising-wrapped export by the JS shim.
//
// //go:noinline because the global-slot dance is reordered out of
// existence otherwise.
//
//go:wasmexport goroutine_run
//go:noinline
func goroutineRun(id int32) {
	if spawnSlot == nil {
		return
	}
	fn := spawnSlot
	spawnSlot = nil
	fn()
}
