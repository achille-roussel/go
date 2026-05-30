// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// chan_jswasm3.go is the wasm3-native chan substrate. Chan[T] is
// a generic struct allocated on the WasmGC heap; Send/Recv block
// via WasmPark/WasmReady directly — no sudog queue. Single-slot
// wait registers (sendParkID/recvParkID) — multiple waiters per
// side need a queue (follow-up).

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

type Chan[T any] struct {
	qcount     int64
	dataqsiz   int64
	sendx      int64
	recvx      int64
	buf        []T
	closed     bool
	sendParkID int32
	recvParkID int32
}

//go:noinline
func MakeChan[T any](n int) *Chan[T] {
	c := new(Chan[T])
	c.dataqsiz = int64(n)
	if n > 0 {
		c.buf = make([]T, n)
	}
	return c
}

//go:noinline
func (c *Chan[T]) Send(v T) {
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

//go:noinline
func (c *Chan[T]) Recv() T {
	for c.qcount <= 0 {
		c.recvParkID = mintChanParkID()
		WasmPark(c.recvParkID)
		c.recvParkID = 0
	}
	v := c.buf[c.recvx]
	var zero T
	c.buf[c.recvx] = zero
	c.recvx = (c.recvx + 1) % c.dataqsiz
	c.qcount--
	if c.sendParkID != 0 {
		id := c.sendParkID
		c.sendParkID = 0
		WasmReady(id)
	}
	return v
}

