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
	if c.dataqsiz == 0 {
		// Unbuffered rendez-vous: park until a receiver is waiting.
		// The receiver will be parked too (see ReserveRecv) and will
		// have woken us — we exit when recvParkID is non-zero (the
		// receiver's park-ID, planted before it called WasmReady on
		// our sendParkID). Slot 0 is the transit cell allocated by
		// MakeChan regardless of cap; the trampoline writes v there
		// next, and FinishSend wakes the receiver.
		for c.recvParkID == 0 {
			c.sendParkID = wasm3MintChanParkID()
			wasm.WasmPark(c.sendParkID)
			c.sendParkID = 0
		}
		return 0
	}
	for c.qcount >= c.dataqsiz {
		c.sendParkID = wasm3MintChanParkID()
		wasm.WasmPark(c.sendParkID)
		c.sendParkID = 0
	}
	return c.sendx
}

//go:noinline
func wasm3ChanFinishSend(c *channel) {
	if c.dataqsiz == 0 {
		// Unbuffered: receiver is parked on recvParkID (it planted
		// the ID then waited for us to write). Wake it; no buffer
		// bookkeeping (sendx/recvx/qcount stay 0).
		if id := c.recvParkID; id != 0 {
			c.recvParkID = 0
			wasm.WasmReady(id)
		}
		return
	}
	c.sendx = (c.sendx + 1) % c.dataqsiz
	c.qcount++
	if id := c.recvParkID; id != 0 {
		c.recvParkID = 0
		wasm.WasmReady(id)
	}
}

//go:noinline
func wasm3ChanReserveRecv(c *channel) int32 {
	if c.dataqsiz == 0 {
		// Unbuffered rendez-vous: two scenarios depending on arrival
		// order. We always plant our recvParkID first so the sender
		// (whenever it arrives) can find us via the ReserveSend loop's
		// "recvParkID != 0" exit condition.
		c.recvParkID = wasm3MintChanParkID()
		if c.sendParkID != 0 {
			// Sender already arrived and parked. Wake it so it can
			// proceed through SlotsSet (writing v to slot 0) and
			// FinishSend (which will wake us via recvParkID).
			id := c.sendParkID
			c.sendParkID = 0
			wasm.WasmReady(id)
		}
		// Sender (either just-woken from above or still to arrive)
		// will write slot 0 and call FinishSend → WasmReady(recvParkID).
		wasm.WasmPark(c.recvParkID)
		c.recvParkID = 0
		return 0
	}
	for c.qcount <= 0 {
		c.recvParkID = wasm3MintChanParkID()
		wasm.WasmPark(c.recvParkID)
		c.recvParkID = 0
	}
	return c.recvx
}

//go:noinline
func wasm3ChanFinishRecv(c *channel) {
	if c.dataqsiz == 0 {
		// Unbuffered: sender already returned from ReserveSend (we
		// woke it earlier) and ran SlotsSet + FinishSend; no wake-up
		// owed. sendx/recvx/qcount stay 0.
		return
	}
	c.recvx = (c.recvx + 1) % c.dataqsiz
	c.qcount--
	if id := c.sendParkID; id != 0 {
		c.sendParkID = 0
		wasm.WasmReady(id)
	}
}

//go:noinline
func wasm3ChanSetDataqsiz(c *channel, n int32) { c.dataqsiz = n }

// wasm3SelectYield is the no-ready-case fallback for multi-case
// select on wasm3 (walk/select.go's walkSelectCasesWasm3Multi).
// One round-trip through the JS event loop via SleepMs(1), so any
// goroutine parked on one of the select's chans gets a chance to
// run before we re-poll. 1ms is granular but caps busy-wait CPU.
//
//go:noinline
func wasm3SelectYield() {
	wasm.SleepMs(1)
}

// wasm3ChanClose implements the close(c) builtin on wasm3. Marks
// the chan closed and wakes any parked sender / receiver so they
// can observe the close in their re-checked loop bodies.
//
// Panics on double-close, matching the standard Go semantics.
//
//go:noinline
func wasm3ChanClose(c *channel) {
	if c.closed {
		panic(plainError("close of closed channel"))
	}
	c.closed = true
	if id := c.sendParkID; id != 0 {
		c.sendParkID = 0
		wasm.WasmReady(id)
	}
	if id := c.recvParkID; id != 0 {
		c.recvParkID = 0
		wasm.WasmReady(id)
	}
}

// Non-blocking select helpers — drive the buffered/unbuffered chan
// state machine without ever calling WasmPark, so the per-T select
// trampolines (reflectdata/wasm3_changen.go) can implement
// `select { case v := <-c: …; default: … }` as a peek-then-act
// sequence with no risk of suspending.

// wasm3ChanTryRecvBegin reports whether a value can be received right
// now. If ok, slot is the index for SlotsGet (always 0 for unbuffered).
// If !ok and closed, the caller delivers a zero value with
// selected=true, received=false. If !ok and !closed, the case is
// not ready; the caller takes its default branch.
//
//go:noinline
func wasm3ChanTryRecvBegin(c *channel) (slot int32, ok, closed bool) {
	if c.dataqsiz == 0 {
		if c.sendParkID != 0 {
			return 0, true, false
		}
		return 0, false, c.closed
	}
	if c.qcount > 0 {
		return c.recvx, true, false
	}
	return 0, false, c.closed
}

// wasm3ChanTryRecvCommit advances the recv bookkeeping (recvx, qcount)
// and wakes any parked sender, after the per-T trampoline has read the
// value via SlotsGet.
//
//go:noinline
func wasm3ChanTryRecvCommit(c *channel) {
	if c.dataqsiz == 0 {
		if id := c.sendParkID; id != 0 {
			c.sendParkID = 0
			wasm.WasmReady(id)
		}
		return
	}
	c.recvx = (c.recvx + 1) % c.dataqsiz
	c.qcount--
	if id := c.sendParkID; id != 0 {
		c.sendParkID = 0
		wasm.WasmReady(id)
	}
}

// wasm3ChanTrySendBegin reports whether a value can be sent right now.
// If ok, slot is the index for SlotsSet (always 0 for unbuffered).
// Panics on a closed chan, matching Go's send-on-closed semantics.
//
//go:noinline
func wasm3ChanTrySendBegin(c *channel) (slot int32, ok bool) {
	if c.closed {
		panic(plainError("send on closed channel"))
	}
	if c.dataqsiz == 0 {
		if c.recvParkID != 0 {
			return 0, true
		}
		return 0, false
	}
	if c.qcount < c.dataqsiz {
		return c.sendx, true
	}
	return 0, false
}

// wasm3ChanTrySendCommit advances the send bookkeeping (sendx, qcount)
// and wakes any parked receiver, after the per-T trampoline has written
// the value via SlotsSet.
//
//go:noinline
func wasm3ChanTrySendCommit(c *channel) {
	if c.dataqsiz == 0 {
		if id := c.recvParkID; id != 0 {
			c.recvParkID = 0
			wasm.WasmReady(id)
		}
		return
	}
	c.sendx = (c.sendx + 1) % c.dataqsiz
	c.qcount++
	if id := c.recvParkID; id != 0 {
		c.recvParkID = 0
		wasm.WasmReady(id)
	}
}

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
