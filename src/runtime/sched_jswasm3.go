// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// sched_jswasm3.go is the GOOS=js GOARCH=wasm3 replacement for
// the standard scheduler glue that gopark / goready / chan /
// select rely on. The strategy: route the standard runtime's
// gopark/goready calls to a tiny dispatcher that maps each "park"
// to a fresh WasmPark and each "ready" to a matching WasmReady
// on the same id. The id-per-park is stashed on the sudog (which
// the chan/select waker dequeues) so there's no need for per-
// goroutine g identity — the sudog pointer is unique per
// blocking call.
//
// Mechanism:
//
//   chan/select BLOCKING path:
//     mysg := acquireSudog()
//     ... set up mysg, enqueue on chan/select queue ...
//     wasm3PreparePark(mysg)     // stash the sudog being parked
//     gopark(unlockf, lock, ...) // standard runtime call site
//
//   gopark dispatch (in proc.go):
//     if js+wasm3 { wasm3Park(unlockf, lock); return }
//
//   chan/select WAKER path:
//     sg := q.dequeue()
//     wasm3Goready(sg)           // replaces goready(sg.g, ...)
//
// On other arches wasm3PreparePark / wasm3Goready are stubbed
// (sched_notjswasm3.go), so chan.go / select.go can call them
// unconditionally on every build.

package runtime

import (
	"runtime/wasm"
	"unsafe"
)

// wasm3PendingSudog is the sudog being parked by the current
// goroutine. Single-threaded: wasm under JSPI is single-
// activation (one suspender holds the engine or is suspended on
// a Promise), so the "current sudog" is unambiguous between
// wasm3PreparePark and the subsequent gopark dispatch.
//
// Nil between gopark calls — set by wasm3PreparePark immediately
// before gopark, consumed (and cleared) by wasm3Park.
var wasm3PendingSudog *sudog

// wasm3NextParkID hands out fresh park identifiers. Wraps at
// int32 max — practical programs won't hit 2^31 parks per
// session, and even if they did, the ones that wrap are
// guaranteed no longer active (only a finite number park at
// once).
var wasm3NextParkID int32

// wasm3PreparePark stashes sg as the sudog about to be parked.
// Called by chan/select right before gopark on js/wasm3 so
// wasm3Park can find the right sudog to record the parkID on.
// On non-js/wasm3 builds this is a no-op (sched_notjswasm3.go).
//
//go:nosplit
func wasm3PreparePark(sg *sudog) {
	wasm3PendingSudog = sg
}

// wasm3Park implements gopark for js/wasm3. Allocates a fresh
// parkID, records it on the pending sudog (so wasm3Goready can
// find it), runs unlockf (which releases the chan/select lock —
// must happen BEFORE WasmPark or the waker can't make progress),
// then suspends via WasmPark.
//
// On resume (WasmPark returns when WasmReady(parkID) fires), the
// function returns and gopark's caller continues.
//
//go:nosplit
func wasm3Park(unlockf func(*g, unsafe.Pointer) bool, lock unsafe.Pointer) {
	sg := wasm3PendingSudog
	wasm3PendingSudog = nil
	wasm3NextParkID++
	parkID := wasm3NextParkID
	if sg != nil {
		sg.wasm3ParkID = parkID
	}
	// else: no sudog (e.g. gopark called with nil/nil for "wait
	// forever"). The parkID is allocated but no WasmReady will
	// fire on it, so this goroutine sleeps until process exit.
	if unlockf != nil {
		gp := getg()
		if !unlockf(gp, lock) {
			// unlockf rejected the park — standard runtime
			// contract says resume immediately. Return without
			// suspending.
			return
		}
	}
	wasm.WasmPark(parkID)
}

// wasm3Goready wakes the goroutine parked on sg. Called by
// chan/select on the waker side. On non-js/wasm3 builds this
// forwards to goready(sg.g, traceskip).
//
//go:nosplit
func wasm3Goready(sg *sudog) {
	wasm.WasmReady(sg.wasm3ParkID)
}
