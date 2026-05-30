// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// chan_jswasm3.go is the runtime-side dispatcher for stock
// `chan T` ops on js/wasm3. The wasm3 compiler intrinsifies
// runtime.makechan / runtime.chansend1 / runtime.chanrecv1 to
// emit calls to per-T helpers here, which forward to the
// runtime/wasm.Chan[T] substrate.

package runtime

import (
	"runtime/wasm"
	"unsafe"
)

//go:linkname wasm3MakeChanInt32
//go:noinline
func wasm3MakeChanInt32(n int) *wasm.Chan[int32] {
	return wasm.MakeChan[int32](n)
}

//go:linkname wasm3ChanInt32Send
//go:noinline
func wasm3ChanInt32Send(c *wasm.Chan[int32], v int32) {
	c.Send(v)
}

//go:linkname wasm3ChanInt32Recv
//go:noinline
func wasm3ChanInt32Recv(c *wasm.Chan[int32]) int32 {
	return c.Recv()
}

// Per-T chan field-access stubs intrinsified by the wasm3 backend to
// the OpWasm3Chan* SSA ops. Bodies are placeholders — the intrinsic
// replaces the call. Used by the per-T chan trampoline generator
// (reflectdata/wasm3_changen.go). The unsafe.Pointer chan arg is the
// $go.chan.<T> ref typed as a pointer at the Go level.

//go:noinline
func wasm3ChanQcount(c unsafe.Pointer) int64 { return 0 }

//go:noinline
func wasm3ChanQcountSet(c unsafe.Pointer, v int64) {}

//go:noinline
func wasm3ChanDataqsiz(c unsafe.Pointer) int64 { return 0 }

//go:noinline
func wasm3ChanDataqsizSet(c unsafe.Pointer, v int64) {}

//go:noinline
func wasm3ChanSendx(c unsafe.Pointer) int64 { return 0 }

//go:noinline
func wasm3ChanSendxSet(c unsafe.Pointer, v int64) {}

//go:noinline
func wasm3ChanRecvx(c unsafe.Pointer) int64 { return 0 }

//go:noinline
func wasm3ChanRecvxSet(c unsafe.Pointer, v int64) {}

//go:noinline
func wasm3ChanSendParkID(c unsafe.Pointer) int32 { return 0 }

//go:noinline
func wasm3ChanSendParkIDSet(c unsafe.Pointer, v int32) {}

//go:noinline
func wasm3ChanRecvParkID(c unsafe.Pointer) int32 { return 0 }

//go:noinline
func wasm3ChanRecvParkIDSet(c unsafe.Pointer, v int32) {}

// wasm3ChanBufGet / wasm3ChanBufSet take `any` for the value type;
// LookupRuntime substitutes the chan's element type at call site so
// the SSA value carries the right wasm-level type.

//go:noinline
func wasm3ChanBufGet(c unsafe.Pointer, i int64) any { return nil }

//go:noinline
func wasm3ChanBufSet(c unsafe.Pointer, i int64, v any) {}

//go:noinline
func wasm3ChanBufAlloc(c unsafe.Pointer, cap int64) {}

// wasm3ChanAlloc allocates a fresh $go.chan.<T> via OpWasm3MakeChan;
// the chan type comes from the Wasm3ChanHelperTypes side-channel.
// Body is a placeholder.
//
//go:noinline
func wasm3ChanAlloc() unsafe.Pointer { return nil }

// Park-ID generator and JSPI park/ready wrappers callable from the
// per-T chan trampolines via typecheck.LookupRuntime. The counter is
// process-wide; 0 is reserved (used to signal "no waiter").

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

//go:noinline
func wasm3ChanPark(id int32) { wasm.WasmPark(id) }

//go:noinline
func wasm3ChanReady(id int32) { wasm.WasmReady(id) }
