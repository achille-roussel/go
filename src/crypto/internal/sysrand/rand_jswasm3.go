// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// js/wasm3 system-random stub. The js/wasm version uses a wasmimport
// to call JS crypto.getRandomValues, but the import surface (a []byte
// parameter) is rejected by the wasm3 toolchain today. Until a
// wasm3-friendly random source is wired through the runtime/wasm
// linear-memory bridge (or the wasmimport []byte gap is closed), use
// a small splitmix64 PRNG seeded from runtime nanotime so the read
// function is non-blocking and gives differing values across calls.
// This is NOT cryptographically secure — tests that need real
// entropy will need to be skipped or driven differently on wasm3.

package sysrand

import (
	"sync/atomic"
	_ "unsafe"
)

//go:linkname runtime_nanotime runtime.nanotime
func runtime_nanotime() int64

var sysrandState atomic.Uint64

func splitmix64(s *atomic.Uint64) uint64 {
	for {
		old := s.Load()
		next := old + 0x9E3779B97F4A7C15
		if !s.CompareAndSwap(old, next) {
			continue
		}
		z := next
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D209C745A6FCED
		z = z ^ (z >> 31)
		return z
	}
}

func read(b []byte) error {
	if sysrandState.Load() == 0 {
		seed := uint64(runtime_nanotime())
		if seed == 0 {
			seed = 1
		}
		sysrandState.CompareAndSwap(0, seed)
	}
	for len(b) >= 8 {
		v := splitmix64(&sysrandState)
		b[0] = byte(v)
		b[1] = byte(v >> 8)
		b[2] = byte(v >> 16)
		b[3] = byte(v >> 24)
		b[4] = byte(v >> 32)
		b[5] = byte(v >> 40)
		b[6] = byte(v >> 48)
		b[7] = byte(v >> 56)
		b = b[8:]
	}
	if len(b) > 0 {
		v := splitmix64(&sysrandState)
		for i := range b {
			b[i] = byte(v >> (i * 8))
		}
	}
	return nil
}
