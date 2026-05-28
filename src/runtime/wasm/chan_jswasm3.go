// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// chan_jswasm3.go is the wasm3-native chan substrate. Until the
// wasm3 backend's generic-dictionary calling convention is fixed
// (see doc/wasm3-js-chan-status.md), the types are non-generic
// per-T variants (ChanInt32, ChanInt64, ...). Once generics on
// wasm3 work, these collapse to a single generic Chan[T].
//
// Per-T methods use WasmPark/WasmReady directly for blocking
// handoff — no sudog queue, no standard scheduler integration.
// Single-slot wait registers (sendParkID/recvParkID) — Phase 4
// minimum, multiple waiters per side need a queue (follow-up).

package wasm

var nextChanParkID int32 = 1

//go:nosplit
//go:noinline
func mintChanParkID() int32 {
	nextChanParkID++
	if nextChanParkID == 0 {
		nextChanParkID = 1
	}
	return nextChanParkID
}

// ChanInt32 is the wasm3-native int32 channel.
type ChanInt32 struct {
	qcount     int64
	dataqsiz   int64
	sendx      int64
	recvx      int64
	buf        []int32
	closed     bool
	sendParkID int32
	recvParkID int32
}

// MakeChanInt32 allocates a buffered (n > 0) or unbuffered (n == 0)
// ChanInt32. //go:noinline forces the struct allocation through
// a function-call boundary — the wasm3 backend mishandles inline
// mixed-scalar+ref struct literals (memory.fill on a WasmGC ref).
//
//go:noinline
func MakeChanInt32(n int) *ChanInt32 {
	c := new(ChanInt32)
	c.dataqsiz = int64(n)
	if n > 0 {
		c.buf = make([]int32, n)
	}
	return c
}

// Send transmits v on c. Blocks until buf has space; after writing,
// wakes any waiting receiver.
//
//go:noinline
func (c *ChanInt32) Send(v int32) {
	for c.qcount >= c.dataqsiz {
		c.sendParkID = mintChanParkID()
		WasmPark(c.sendParkID)
		c.sendParkID = 0
	}
	c.buf[c.sendx] = v
	c.sendx = (c.sendx + 1) % c.dataqsiz
	c.qcount++
	if c.recvParkID != 0 {
		id := c.recvParkID
		c.recvParkID = 0
		WasmReady(id)
	}
}

// Recv receives a value from c. Blocks until buf has data; after
// reading, wakes any waiting sender.
//
//go:noinline
func (c *ChanInt32) Recv() int32 {
	for c.qcount <= 0 {
		c.recvParkID = mintChanParkID()
		WasmPark(c.recvParkID)
		c.recvParkID = 0
	}
	v := c.buf[c.recvx]
	c.buf[c.recvx] = 0
	c.recvx = (c.recvx + 1) % c.dataqsiz
	c.qcount--
	if c.sendParkID != 0 {
		id := c.sendParkID
		c.sendParkID = 0
		WasmReady(id)
	}
	return v
}
