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
	_ "unsafe" // for go:linkname
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
