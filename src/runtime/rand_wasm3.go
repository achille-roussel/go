// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import _ "unsafe" // for go:linkname

// rand on wasm3 uses a tiny xorshift64 seeded from a counter
// rather than the per-M chacha8 state the default rand() reads
// from getg().m.chacha8. The M2 cutover's *g/*m shape isn't
// populated yet, so any chacha8/Refill path nil-derefs. Maps
// need a non-zero seed (otherwise iteration order is fixed and
// the hash distribution collapses), but for the
// single-goroutine bootstrap a deterministic counter-seeded
// PRNG is fine — it produces a unique sequence per call.
//
// Replaced by the real per-M rand once goroutine state is wired
// (M4 stack-switching scheduler).

var wasm3RandState uint64 = 0x9E3779B97F4A7C15 // golden-ratio seed

//go:linkname rand
//go:nosplit
func rand() uint64 {
	x := wasm3RandState
	if x == 0 {
		x = 0x9E3779B97F4A7C15
	}
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	wasm3RandState = x
	return x
}
