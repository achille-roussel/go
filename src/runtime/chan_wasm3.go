// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// wasm3 mirror of chan.go and select.go.
//
// chan.go and select.go are tagged !wasm3 because their hchan-buffer
// and hchan.sortkey paths use `unsafe.Pointer(uintptr(p) + x)`
// pointer arithmetic that anyref cannot represent. The wasm3 chan
// runtime lives in chan_jswasm3.go: a per-T (type-specialized)
// trampoline scheme that owns make / send / recv / close / select /
// range — the chan ops emitted by the compiler call wasm3Chan*
// directly, never chansend1/chanrecv1.
//
// This file's job is therefore narrow: provide just the symbols
// that the rest of the wasm3 runtime build references from chan.go
// — concretely `hchan` (held in timer.arg and maybeTraceableChan,
// referenced from runtime2.go and time.go) and `timerchandrain`
// (the timer-chan drain hook called from time.go). Everything else
// in chan.go and select.go is unreachable on wasm3, so omitting it
// is correct.

// hchan is the runtime-side channel structure. On non-wasm3 it
// also holds a linear-memory ring buffer at `buf` with
// dataqsiz * elemsize bytes of element storage. On wasm3 the buffer
// lives in a per-T WasmGC array owned by the trampoline path (see
// chan_jswasm3.go's `channel` type and wasm3Chan* primitives); the
// fields below are kept only so that the type laid out in time.go's
// timer.arg (and maybeTraceableChan) remains a *hchan as on every
// other arch.
type hchan struct {
	qcount   uint           // total data in the queue
	dataqsiz uint           // size of the circular queue
	buf      unsafe3Pointer // unused on wasm3 (per-T trampoline holds storage)
	elemsize uint16
	closed   uint32
	timer    *timer
	elemtype *_type
	sendx    uint
	recvx    uint
	recvq    waitq
	sendq    waitq
	bubble   *synctestBubble

	lock mutex
}

// unsafe3Pointer is a placeholder for unsafe.Pointer; using the
// alias avoids importing "unsafe" just to declare an always-nil
// field. The buf field is never read on wasm3.
type unsafe3Pointer = *byte

type waitq struct {
	first *sudog
	last  *sudog
}

// timerchandrain is called from time.go to flush any pending values
// from a timer-feeding channel before the timer is stopped / reset.
// On wasm3 the timer-chan path doesn't run yet (#128 M4 phase 2-7
// is in progress); return false to signal "no values drained". When
// timer chans come online on wasm3, route this through the per-T
// trampoline (wasm3ChanTryRecvBegin loop) instead.
func timerchandrain(c *hchan) bool {
	return false
}
