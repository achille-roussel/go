// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// chan_jswasm3.go is the wasm3-native chan substrate. *channel
// mirrors the prelude $go.chan WasmGC struct exactly; the
// compiler-generated per-T trampolines (reflectdata/wasm3_changen.go)
// ref.cast a (ref $go.chan.<T>) down to *channel here to drive the
// common state (qcount/dataqsiz/sendx/recvx/parkIDs) through plain
// Go, and stay subtype-polymorphic across all chan T. Per-T slots
// access lives in the trampoline as ref.cast back to $go.chan.<T> +
// array.get/set. See doc/wasm3-chan-subtyping.md.

package runtime

import (
	"runtime/wasm"
	"unsafe"
)

// channel mirrors $go.chan field-for-field; the trampoline gets a
// *channel by ref.cast of (ref $go.chan.<T>) to (ref $go.chan).
// Unexported — internal to the compiler/runtime boundary.
type channel struct {
	qcount     int32
	dataqsiz   int32
	sendx      int32
	recvx      int32
	closed     bool
	sendParkID int32
	recvParkID int32
}

// wasm3ChanAlloc allocates a fresh $go.chan.<T> WasmGC struct. The
// intrinsic in cmd/compile/internal/ssagen/intrinsics.go replaces this
// call with OpWasm3MakeChan keyed on the chan type from the side-
// channel; this body is a placeholder.
//
//go:noinline
func wasm3ChanAlloc() unsafe.Pointer { return nil }

// wasm3ChanSlotsAlloc / Get / Set are intrinsified to OpWasm3Chan{Slots*}
// — they read/write/allocate the per-T slots array on a $go.chan.<T>
// via ref.cast + struct.get/set 7 + array.get/set. Bodies are
// placeholders the intrinsic replaces. wasm3ChanSlotsGet's `any`
// return type gets substituted with the chan's element type at call
// construction.

//go:noinline
func wasm3ChanSlotsAlloc(c *channel, cap int32) {}

//go:noinline
func wasm3ChanSlotsGet(c *channel, i int32) any { return nil }

//go:noinline
func wasm3ChanSlotsSet(c *channel, i int32, v any) {}

// State-machine helpers driving the chan's common fields. Called by
// the per-T trampoline directly (no intrinsic — these are real Go
// functions whose bodies do the work).

//go:noinline
func wasm3ChanReserveSend(c *channel) int32 {
	for c.qcount >= c.dataqsiz {
		c.sendParkID = wasm3MintChanParkID()
		wasm.WasmPark(c.sendParkID)
		c.sendParkID = 0
	}
	return c.sendx
}

//go:noinline
func wasm3ChanFinishSend(c *channel) {
	c.sendx = (c.sendx + 1) % c.dataqsiz
	c.qcount++
	if id := c.recvParkID; id != 0 {
		c.recvParkID = 0
		wasm.WasmReady(id)
	}
}

//go:noinline
func wasm3ChanReserveRecv(c *channel) int32 {
	for c.qcount <= 0 {
		c.recvParkID = wasm3MintChanParkID()
		wasm.WasmPark(c.recvParkID)
		c.recvParkID = 0
	}
	return c.recvx
}

//go:noinline
func wasm3ChanFinishRecv(c *channel) {
	c.recvx = (c.recvx + 1) % c.dataqsiz
	c.qcount--
	if id := c.sendParkID; id != 0 {
		c.sendParkID = 0
		wasm.WasmReady(id)
	}
}

//go:noinline
func wasm3ChanSetDataqsiz(c *channel, n int32) { c.dataqsiz = n }

// Park-ID generator — process-wide int32 counter; 0 reserved
// (signals "no waiter").

var wasm3NextChanParkID int32 = 1

//go:nosplit
//go:noinline
func wasm3MintChanParkID() int32 {
	wasm3NextChanParkID++
	if wasm3NextChanParkID == 0 {
		wasm3NextChanParkID = 1
	}
	return wasm3NextChanParkID
}
