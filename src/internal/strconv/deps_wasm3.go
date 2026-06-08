// Copyright 2025 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 mirror of deps.go. The only divergence from the !wasm3
// original is the codegen path for the floatN bits / frombits
// helpers: the wasm3 SSA backend intrinsifies them to single-
// instruction iN.reinterpret_fN / fN.reinterpret_iN emitted into
// the caller's body (cmd/compile/internal/ssagen/intrinsics.go).
// The bodies are self-recursive so the intrinsic fires when the
// symbol itself is compiled, leaving wasm3check clean. The
// recursion is never reached at run time — every call site, the
// recursive one included, is rewritten to a single wasm
// instruction.

package strconv

func float64frombits(b uint64) float64 { return float64frombits(b) }
func float32frombits(b uint32) float32 { return float32frombits(b) }
func float64bits(f float64) uint64     { return float64bits(f) }
func float32bits(f float32) uint32     { return float32bits(f) }

func inf(sign int) float64 {
	var v uint64
	if sign >= 0 {
		v = 0x7FF0000000000000
	} else {
		v = 0xFFF0000000000000
	}
	return float64frombits(v)
}

func isNaN(f float64) (is bool) { return f != f }

func nan() float64 { return float64frombits(0x7FF8000000000001) }
