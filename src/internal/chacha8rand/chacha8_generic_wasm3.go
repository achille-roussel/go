// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// ChaCha8 is ChaCha with 8 rounds.
// See https://cr.yp.to/chacha/chacha-20080128.pdf.
//
// wasm3 mirror of chacha8_generic.go. The non-wasm3 implementation
// reinterprets the [16][4]uint32 ChaCha block as [16][2]uint64 via
// unsafe.Pointer in setup, and reinterprets the caller's [32]uint64
// output buffer as [16][4]uint32 in block_generic. Both casts cross
// incompatible WasmGC array element types and are not expressible
// on wasm3. The mirror keeps the algorithm bit-identical and
// replaces the reinterprets with direct stores; it also uses a flat
// [64]uint32 working array (instead of [16][4]uint32) to sidestep a
// wasm3 SSA classifier issue where reading from a multi-dim local
// array misclassifies the produced uint32 value as anyref.
//
// See doc/wasm3-design.md.
package chacha8rand

// setup sets up 4 ChaCha8 blocks in b32 with the counter and seed.
// The original takes b32 as *[16][4]uint32 and writes through it; on
// wasm3 we take a flat *[64]uint32 instead, indexed as b32[r*4+l]
// for row r and lane l, to avoid the multi-dim wasm3 codegen path.
func setup(seed *[4]uint64, b32 *[64]uint32, counter uint32) {
	// Constants; same as in ChaCha20: "expand 32-byte k".
	b32[0] = 0x61707865
	b32[1] = 0x61707865
	b32[2] = 0x61707865
	b32[3] = 0x61707865
	b32[4] = 0x3320646e
	b32[5] = 0x3320646e
	b32[6] = 0x3320646e
	b32[7] = 0x3320646e
	b32[8] = 0x79622d32
	b32[9] = 0x79622d32
	b32[10] = 0x79622d32
	b32[11] = 0x79622d32
	b32[12] = 0x6b206574
	b32[13] = 0x6b206574
	b32[14] = 0x6b206574
	b32[15] = 0x6b206574

	// Seed values. Each uint32 half of seed[i] broadcasts to all four
	// lanes of one row, matching the non-wasm3 path's b[r][p] = (x<<32)|x.
	s00 := uint32(seed[0])
	b32[16] = s00
	b32[17] = s00
	b32[18] = s00
	b32[19] = s00
	s01 := uint32(seed[0] >> 32)
	b32[20] = s01
	b32[21] = s01
	b32[22] = s01
	b32[23] = s01
	s10 := uint32(seed[1])
	b32[24] = s10
	b32[25] = s10
	b32[26] = s10
	b32[27] = s10
	s11 := uint32(seed[1] >> 32)
	b32[28] = s11
	b32[29] = s11
	b32[30] = s11
	b32[31] = s11
	s20 := uint32(seed[2])
	b32[32] = s20
	b32[33] = s20
	b32[34] = s20
	b32[35] = s20
	s21 := uint32(seed[2] >> 32)
	b32[36] = s21
	b32[37] = s21
	b32[38] = s21
	b32[39] = s21
	s30 := uint32(seed[3])
	b32[40] = s30
	b32[41] = s30
	b32[42] = s30
	b32[43] = s30
	s31 := uint32(seed[3] >> 32)
	b32[44] = s31
	b32[45] = s31
	b32[46] = s31
	b32[47] = s31

	// Counters. wasm is always little-endian, so this mirrors only the
	// !goarch.BigEndian arm of the original.
	b32[48] = counter + 0
	b32[49] = counter + 1
	b32[50] = counter + 2
	b32[51] = counter + 3

	// Zeros.
	b32[52] = 0
	b32[53] = 0
	b32[54] = 0
	b32[55] = 0
	b32[56] = 0
	b32[57] = 0
	b32[58] = 0
	b32[59] = 0
	b32[60] = 0
	b32[61] = 0
	b32[62] = 0
	b32[63] = 0
}

func _() {
	// block and block_generic must have same type
	x := block
	x = block_generic
	_ = x
}

// block_generic is the non-assembly block implementation. The
// wasm3 mirror uses a flat [64]uint32 working array (logically a
// [16][4]uint32 with row r and lane i at b[r*4+i]) so the inner
// round logic doesn't go through a 2D local array — the wasm3 SSA
// codegen for that path mistypes the produced uint32 element load
// as anyref. At the end of each lane i, we read the lane's 16
// uint32 words into b0..b15 and run the standard qr-round logic.
func block_generic(seed *[4]uint64, buf *[32]uint64, counter uint32) {
	var b [64]uint32

	setup(seed, &b, counter)

	for i := 0; i < 4; i++ {
		// Load lane i from b[r*4+i] into local variables.
		b0 := b[0*4+i]
		b1 := b[1*4+i]
		b2 := b[2*4+i]
		b3 := b[3*4+i]
		b4 := b[4*4+i]
		b5 := b[5*4+i]
		b6 := b[6*4+i]
		b7 := b[7*4+i]
		b8 := b[8*4+i]
		b9 := b[9*4+i]
		b10 := b[10*4+i]
		b11 := b[11*4+i]
		b12 := b[12*4+i]
		b13 := b[13*4+i]
		b14 := b[14*4+i]
		b15 := b[15*4+i]

		// 4 iterations of eight quarter-rounds each is 8 rounds.
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

		// Store block i back into b[r*4+i].
		// Add b4..b11 back to the original key material,
		// like in ChaCha20, to avoid trivial invertibility.
		// There is no entropy in b0..b3 and b12..b15
		// so we can skip the additions and save some time.
		b[0*4+i] = b0
		b[1*4+i] = b1
		b[2*4+i] = b2
		b[3*4+i] = b3
		b[4*4+i] += b4
		b[5*4+i] += b5
		b[6*4+i] += b6
		b[7*4+i] += b7
		b[8*4+i] += b8
		b[9*4+i] += b9
		b[10*4+i] += b10
		b[11*4+i] += b11
		b[12*4+i] = b12
		b[13*4+i] = b13
		b[14*4+i] = b14
		b[15*4+i] = b15
	}

	// Pack the [64]uint32 lanes into the [32]uint64 output buffer in
	// little-endian order: buf[r*2] = b[r*4] | b[r*4+1]<<32, etc.
	for r := 0; r < 16; r++ {
		buf[r*2+0] = uint64(b[r*4+0]) | uint64(b[r*4+1])<<32
		buf[r*2+1] = uint64(b[r*4+2]) | uint64(b[r*4+3])<<32
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
