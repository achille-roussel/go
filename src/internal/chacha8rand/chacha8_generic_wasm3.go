// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// ChaCha8 is ChaCha with 8 rounds.
// See https://cr.yp.to/chacha/chacha-20080128.pdf.
//
// This file is the GOARCH=wasm3 mirror of chacha8_generic.go. The
// non-wasm3 implementation reinterprets the [16][4]uint32 block buffer
// as [16][2]uint64 via unsafe.Pointer to halve the number of stores
// during setup, and reinterprets [32]uint64 as [16][4]uint32 in
// block_generic so the qr round logic can run on uint32 lanes. Both
// reinterprets cross WasmGC array element types (array i32 vs array
// i64), which is not expressible on wasm3. The mirror keeps the same
// chacha8rand algorithm but issues plain uint32 stores during setup
// and packs the final uint32 lanes into the uint64 output buffer with
// an explicit shift+or instead of a type pun. The wasm engine emits a
// single i32.const + array.set per assignment either way, so the
// store-halving the original chases is moot on this target.
//
// See doc/wasm3-design.md.
package chacha8rand

// setup sets up 4 ChaCha8 blocks in b32 with the counter and seed.
// Note that b32 is [16][4]uint32 not [4][16]uint32: the blocks are interlaced
// the same way they would be in a 4-way SIMD implementations.
func setup(seed *[4]uint64, b32 *[16][4]uint32, counter uint32) {
	// Constants; same as in ChaCha20: "expand 32-byte k".
	// The non-wasm3 path writes each row as a pair of uint64 stores via
	// a reinterpret cast; here we issue four plain uint32 stores per row.
	b32[0][0], b32[0][1], b32[0][2], b32[0][3] = 0x61707865, 0x61707865, 0x61707865, 0x61707865
	b32[1][0], b32[1][1], b32[1][2], b32[1][3] = 0x3320646e, 0x3320646e, 0x3320646e, 0x3320646e
	b32[2][0], b32[2][1], b32[2][2], b32[2][3] = 0x79622d32, 0x79622d32, 0x79622d32, 0x79622d32
	b32[3][0], b32[3][1], b32[3][2], b32[3][3] = 0x6b206574, 0x6b206574, 0x6b206574, 0x6b206574

	// Seed values. Each uint32 half of seed[i] broadcasts to all four
	// lanes of one row, matching the non-wasm3 path's b[r][p] = (x<<32)|x.
	s00 := uint32(seed[0])
	b32[4][0], b32[4][1], b32[4][2], b32[4][3] = s00, s00, s00, s00
	s01 := uint32(seed[0] >> 32)
	b32[5][0], b32[5][1], b32[5][2], b32[5][3] = s01, s01, s01, s01
	s10 := uint32(seed[1])
	b32[6][0], b32[6][1], b32[6][2], b32[6][3] = s10, s10, s10, s10
	s11 := uint32(seed[1] >> 32)
	b32[7][0], b32[7][1], b32[7][2], b32[7][3] = s11, s11, s11, s11
	s20 := uint32(seed[2])
	b32[8][0], b32[8][1], b32[8][2], b32[8][3] = s20, s20, s20, s20
	s21 := uint32(seed[2] >> 32)
	b32[9][0], b32[9][1], b32[9][2], b32[9][3] = s21, s21, s21, s21
	s30 := uint32(seed[3])
	b32[10][0], b32[10][1], b32[10][2], b32[10][3] = s30, s30, s30, s30
	s31 := uint32(seed[3] >> 32)
	b32[11][0], b32[11][1], b32[11][2], b32[11][3] = s31, s31, s31, s31

	// Counters. wasm is always little-endian, so this mirrors only the
	// !goarch.BigEndian arm of the original.
	b32[12][0] = counter + 0
	b32[12][1] = counter + 1
	b32[12][2] = counter + 2
	b32[12][3] = counter + 3

	// Zeros.
	b32[13][0], b32[13][1], b32[13][2], b32[13][3] = 0, 0, 0, 0
	b32[14][0], b32[14][1], b32[14][2], b32[14][3] = 0, 0, 0, 0
	b32[15][0], b32[15][1], b32[15][2], b32[15][3] = 0, 0, 0, 0
}

func _() {
	// block and block_generic must have same type
	x := block
	x = block_generic
	_ = x
}

// block_generic is the non-assembly block implementation,
// for use on systems without special assembly.
// Even on such systems, it is quite fast: on GOOS=386,
// ChaCha8 using this code generates random values faster than PCG-DXSM.
func block_generic(seed *[4]uint64, buf *[32]uint64, counter uint32) {
	// Work in a local [16][4]uint32 block, then pack into the caller's
	// [32]uint64 buf at the end. The non-wasm3 path reinterprets buf as
	// [16][4]uint32 in place via unsafe.Pointer; on wasm3 the two arrays
	// have incompatible WasmGC element types so we keep them separate.
	var b [16][4]uint32

	setup(seed, &b, counter)

	for i := range b[0] {
		// Load block i from b[*][i] into local variables.
		b0 := b[0][i]
		b1 := b[1][i]
		b2 := b[2][i]
		b3 := b[3][i]
		b4 := b[4][i]
		b5 := b[5][i]
		b6 := b[6][i]
		b7 := b[7][i]
		b8 := b[8][i]
		b9 := b[9][i]
		b10 := b[10][i]
		b11 := b[11][i]
		b12 := b[12][i]
		b13 := b[13][i]
		b14 := b[14][i]
		b15 := b[15][i]

		// 4 iterations of eight quarter-rounds each is 8 rounds
		for round := 0; round < 4; round++ {
			b0, b4, b8, b12 = qr(b0, b4, b8, b12)
			b1, b5, b9, b13 = qr(b1, b5, b9, b13)
			b2, b6, b10, b14 = qr(b2, b6, b10, b14)
			b3, b7, b11, b15 = qr(b3, b7, b11, b15)

			b0, b5, b10, b15 = qr(b0, b5, b10, b15)
			b1, b6, b11, b12 = qr(b1, b6, b11, b12)
			b2, b7, b8, b13 = qr(b2, b7, b8, b13)
			b3, b4, b9, b14 = qr(b3, b4, b9, b14)
		}

		// Store block i back into b[*][i].
		// Add b4..b11 back to the original key material,
		// like in ChaCha20, to avoid trivial invertibility.
		// There is no entropy in b0..b3 and b12..b15
		// so we can skip the additions and save some time.
		b[0][i] = b0
		b[1][i] = b1
		b[2][i] = b2
		b[3][i] = b3
		b[4][i] += b4
		b[5][i] += b5
		b[6][i] += b6
		b[7][i] += b7
		b[8][i] += b8
		b[9][i] += b9
		b[10][i] += b10
		b[11][i] += b11
		b[12][i] = b12
		b[13][i] = b13
		b[14][i] = b14
		b[15][i] = b15
	}

	// Pack the [16][4]uint32 block into the [32]uint64 output buffer in
	// little-endian order. The non-wasm3 path writes the uint32 lanes
	// directly into the uint64 buffer via the reinterpret cast, then
	// applies a uint64 byte-swap on big-endian targets; wasm is always
	// little-endian, so only the LE arrangement is materialized here.
	for r := 0; r < 16; r++ {
		buf[r*2+0] = uint64(b[r][0]) | uint64(b[r][1])<<32
		buf[r*2+1] = uint64(b[r][2]) | uint64(b[r][3])<<32
	}
}

// qr is the (inlinable) ChaCha8 quarter round.
func qr(a, b, c, d uint32) (_a, _b, _c, _d uint32) {
	a += b
	d ^= a
	d = d<<16 | d>>16
	c += d
	b ^= c
	b = b<<12 | b>>20
	a += b
	d ^= a
	d = d<<8 | d>>24
	c += d
	b ^= c
	b = b<<7 | b>>25
	return a, b, c, d
}
