// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// Boxed-value hashing helpers for GOARCH=wasm3.
//
// The shared signature-based hash scheme (genhashSig / hashFunc) hashes
// a value by scanning its flat linear-memory layout — memhash over byte
// regions, strhash over a string's data pointer. In the wasm3 boxed
// object model there is no flat layout and no linear data pointer, so
// the compiler instead generates a typed, per-type hash function that
// walks the value field by field and mixes each leaf into the running
// hash by VALUE through these helpers (rather than by address, which
// would require interior pointers). The mixing must agree with the
// boxed equality helpers in alg_string_wasm3.go: equal values must hash
// equal. See genhashWasm3 in cmd/compile/internal/reflectdata/alg.go
// and the boxed bulk-ops project memory.

// wasm3HashMul is a 64-bit odd multiplicative-hash constant (the golden
// ratio), used to mix bits between rounds.
const wasm3HashMul = 0x9E3779B97F4A7C15

// wasm3Uint64Hash mixes the 64-bit value v into the running hash h.
//
//go:nosplit
func wasm3Uint64Hash(v uint64, h uintptr) uintptr {
	x := uint64(h) ^ v
	x *= wasm3HashMul
	x ^= x >> 32
	x *= wasm3HashMul
	return uintptr(x)
}

// wasm3BoolHash mixes a boolean into h.
//
//go:nosplit
func wasm3BoolHash(b bool, h uintptr) uintptr {
	if b {
		return wasm3Uint64Hash(1, h)
	}
	return wasm3Uint64Hash(0, h)
}

// wasm3Float64Hash mixes a float64 into h, normalizing the two zeros and
// NaN so the result is consistent with float ==.
//
//go:nosplit
func wasm3Float64Hash(f float64, h uintptr) uintptr {
	switch {
	case f == 0:
		return wasm3Uint64Hash(0, h) // +0 and -0 are equal, hash the same
	case f != f:
		// NaN: not equal to anything (including itself), so the hash only
		// needs to be deterministic. Mix the seed with a constant.
		return wasm3Uint64Hash(0x9E3779B9, h)
	default:
		return wasm3Uint64Hash(float64bits(f), h)
	}
}

// wasm3StringHash mixes a string's bytes into h, reading them through
// the boxed (ref $go.bytes) backing via indexing (array.get).
//
//go:nosplit
func wasm3StringHash(s string, h uintptr) uintptr {
	x := uint64(h)
	for i := 0; i < len(s); i++ {
		x = (x ^ uint64(s[i])) * wasm3HashMul
	}
	return uintptr(x)
}
