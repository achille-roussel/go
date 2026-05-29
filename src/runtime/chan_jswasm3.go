// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// chan_jswasm3.go is the runtime-side dispatcher for stock
// `chan T` ops on js/wasm3. The wasm3 compiler intrinsifies
// runtime.makechan / runtime.chansend1 / runtime.chanrecv1 to
// emit calls to the helpers here, which in turn forward to the
// runtime/wasm chan substrate. Living in package runtime lets
// the compiler look the helpers up via typecheck.LookupRuntimeFunc.
//
// Per-T variants (Int32, Int64, ...) until the wasm3 backend's
// generic-dictionary calling convention is fixed — see
// doc/wasm3-js-chan-status.md.

package runtime

import (
	"runtime/wasm"
	_ "unsafe" // for go:linkname
)

// Publish markers — let external packages reach these via
// //go:linkname while the compiler intrinsic dispatch is being
// wired up.

//go:linkname wasm3MakeChanInt32
//go:noinline
func wasm3MakeChanInt32(n int) *wasm.ChanInt32 {
	return wasm.MakeChanInt32(n)
}

//go:linkname wasm3ChanInt32Send
//go:noinline
func wasm3ChanInt32Send(c *wasm.ChanInt32, v int32) {
	c.Send(v)
}

//go:linkname wasm3ChanInt32Recv
//go:noinline
func wasm3ChanInt32Recv(c *wasm.ChanInt32) int32 {
	return c.Recv()
}
