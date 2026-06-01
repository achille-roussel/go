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

// Send/recv protocol on unbuffered chans uses a write-before-park
// invariant: the sender writes slot 0 BEFORE setting sendParkID, so
// any concurrent reader (blocking ReserveRecv or non-blocking select
// TryRecvBegin) that observes sendParkID != 0 can safely read slot 0
// without further synchronisation. The earlier protocol set
// sendParkID first then waited for ReserveSend to return so the
// trampoline could write — that left a window where TryRecvBegin
// reported "ready" while slot 0 was empty.
//
// Per-T trampolines branch on dataqsiz: dataqsiz == 0 routes to
// SlotsSet(0, v) + wasm3ChanSendUnbuf; dataqsiz > 0 keeps the
// existing buffered ReserveSend / SlotsSet / FinishSend path. So
// the unbuffered branches of wasm3ChanReserveSend / FinishSend
// below are dead under the new trampoline shape — kept as a
// safety net if someone calls them directly.

//go:noinline
func wasm3ChanReserveSend(c *channel) int32 {
	if c.dataqsiz == 0 {
		// Dead path (trampoline routes unbuffered sends through
		// SlotsSet + wasm3ChanSendUnbuf instead). Trap loudly if
		// reached so the bug is visible.
		panic(plainError("wasm3ChanReserveSend: unbuffered chan; trampoline should branch to wasm3ChanSendUnbuf"))
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
		// Dead path; see wasm3ChanReserveSend.
		panic(plainError("wasm3ChanFinishSend: unbuffered chan; trampoline should branch to wasm3ChanSendUnbuf"))
	}
	c.sendx = (c.sendx + 1) % c.dataqsiz
	c.qcount++
	if id := c.recvParkID; id != 0 {
		c.recvParkID = 0
		wasm.WasmReady(id)
	}
}

// wasm3ChanSendUnbuf finalises an unbuffered send AFTER the caller
// has written its value to slot 0. Two scenarios:
//
//   - A receiver is already parked (recvParkID != 0): clear it, plant
//     sendParkID, wake the receiver, then park ourselves. Receiver
//     reads slot 0 then calls FinishRecv (or TryRecvCommit), which
//     wakes us via sendParkID.
//
//   - No receiver yet: plant sendParkID (sender is parked WITH value
//     in slot 0; ReserveRecv / TryRecvBegin treat sendParkID != 0 as
//     "slot 0 ready"), park. Receiver eventually arrives, reads, then
//     wakes us.
//
// Both paths exit with the sender having waited for the receiver to
// consume the value — the rendez-vous is fully synchronous, so the
// sender's next iteration won't overwrite slot 0 mid-read.
//
//go:noinline
func wasm3ChanSendUnbuf(c *channel) {
	if c.closed {
		panic(plainError("send on closed channel"))
	}
	if id := c.recvParkID; id != 0 {
		c.recvParkID = 0
		c.sendParkID = wasm3MintChanParkID()
		wasm.WasmReady(id)
		wasm.WasmPark(c.sendParkID)
		c.sendParkID = 0
		return
	}
	c.sendParkID = wasm3MintChanParkID()
	wasm.WasmPark(c.sendParkID)
	c.sendParkID = 0
}

// wasm3ChanGetDataqsiz exposes c.dataqsiz to the per-T send
// trampoline so it can branch between the unbuffered (SlotsSet +
// SendUnbuf) and buffered (ReserveSend + SlotsSet + FinishSend)
// paths.
//
//go:noinline
func wasm3ChanGetDataqsiz(c *channel) int32 { return c.dataqsiz }

//go:noinline
func wasm3ChanReserveRecv(c *channel) int32 {
	if c.dataqsiz == 0 {
		if c.sendParkID != 0 {
			// Sender already wrote slot 0 and is parked waiting for
			// our FinishRecv to wake them. Skip the park-and-wait
			// dance; the trampoline reads slot 0 next, FinishRecv
			// wakes the sender.
			return 0
		}
		// Plant recvParkID and park. Sender will arrive, write slot
		// 0, then call wasm3ChanSendUnbuf — which wakes us (clearing
		// recvParkID) and parks ON sendParkID waiting for FinishRecv
		// to ack.
		c.recvParkID = wasm3MintChanParkID()
		wasm.WasmPark(c.recvParkID)
		// On wake: sender cleared recvParkID before WasmReady; slot 0
		// holds the value; sender is now parked on sendParkID
		// waiting for FinishRecv.
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
		// Unbuffered: under the write-before-park protocol the sender
		// is parked on sendParkID waiting for us to ack the read.
		// Wake it.
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

// wasm3SelectStart returns a pseudo-random offset in [0, n) used to
// rotate per-case polling order in multi-case selects. The Go spec
// requires uniform pseudo-random selection when multiple cases can
// proceed; without rotation, a deterministic source-order poll
// would starve later cases (e.g. `case <-ticker.C` always winning
// over `case <-stop`). Same LCG used by wasm3MapRandStart.
var wasm3SelectCounter uint64

//go:noinline
func wasm3SelectStart(n int32) int32 {
	if n <= 0 {
		return 0
	}
	wasm3SelectCounter += 0x9e3779b97f4a7c15
	return int32(wasm3SelectCounter % uint64(n))
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
