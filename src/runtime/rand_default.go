// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm3

package runtime

import _ "unsafe" // for go:linkname

// rand returns a random uint64 from the per-m chacha8 state.
// This is called from compiler-generated code.
//
// Do not change signature: used via linkname from other packages.
//
//go:nosplit
//go:linkname rand
func rand() uint64 {
	// Note: We avoid acquirem here so that in the fast path
	// there is just a getg, an inlined c.Next, and a return.
	// The performance difference on a 16-core AMD is
	// 3.7ns/call this way versus 4.3ns/call with acquirem (+16%).
	mp := getg().m
	c := &mp.chacha8
	for {
		// Note: c.Next is marked nosplit,
		// so we don't need to use mp.locks
		// on the fast path, which is that the
		// first attempt succeeds.
		x, ok := c.Next()
		if ok {
			return x
		}
		mp.locks++ // hold m even though c.Refill may do stack split checks
		c.Refill()
		mp.locks--
	}
}
