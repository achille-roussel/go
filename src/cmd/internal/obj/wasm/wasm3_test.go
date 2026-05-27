// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm

import (
	"bytes"
	"cmd/internal/obj"
	"testing"
)

// TestWasm3Opcodes verifies that writeOpcode emits the exact opcode bytes
// specified by the WebAssembly 3.0 garbage-collection, exception-handling,
// and typed-function-references proposals. These opcodes are recognized by
// the assembler as of milestone M1; the compiler does not emit them until
// milestone M2. See doc/wasm3-design.md.
func TestWasm3Opcodes(t *testing.T) {
	tests := []struct {
		as   obj.As
		want []byte
	}{
		// Exception handling.
		{AThrow, []byte{0x08}},
		{AThrowRef, []byte{0x0a}},
		{ATryTable, []byte{0x1f}},

		// Typed function references.
		{ACallRef, []byte{0x14}},
		{AReturnCallRef, []byte{0x15}},
		{ARefNull, []byte{0xd0}},
		{ARefIsNull, []byte{0xd1}},
		{ARefFunc, []byte{0xd2}},
		{ARefEq, []byte{0xd3}},
		{ARefAsNonNull, []byte{0xd4}},
		{ABrOnNull, []byte{0xd5}},
		{ABrOnNonNull, []byte{0xd6}},

		// Garbage collection. All 0xFB-prefixed, sub-opcodes 0x00..0x1E.
		{AStructNew, []byte{0xfb, 0x00}},
		{AStructNewDefault, []byte{0xfb, 0x01}},
		{AStructGet, []byte{0xfb, 0x02}},
		{AStructGetS, []byte{0xfb, 0x03}},
		{AStructGetU, []byte{0xfb, 0x04}},
		{AStructSet, []byte{0xfb, 0x05}},
		{AArrayNew, []byte{0xfb, 0x06}},
		{AArrayNewDefault, []byte{0xfb, 0x07}},
		{AArrayNewFixed, []byte{0xfb, 0x08}},
		{AArrayNewData, []byte{0xfb, 0x09}},
		{AArrayNewElem, []byte{0xfb, 0x0a}},
		{AArrayGet, []byte{0xfb, 0x0b}},
		{AArrayGetS, []byte{0xfb, 0x0c}},
		{AArrayGetU, []byte{0xfb, 0x0d}},
		{AArraySet, []byte{0xfb, 0x0e}},
		{AArrayLen, []byte{0xfb, 0x0f}},
		{AArrayFill, []byte{0xfb, 0x10}},
		{AArrayCopy, []byte{0xfb, 0x11}},
		{AArrayInitData, []byte{0xfb, 0x12}},
		{AArrayInitElem, []byte{0xfb, 0x13}},
		{ARefTest, []byte{0xfb, 0x14}},
		{ARefTestNull, []byte{0xfb, 0x15}},
		{ARefCast, []byte{0xfb, 0x16}},
		{ARefCastNull, []byte{0xfb, 0x17}},
		{ABrOnCast, []byte{0xfb, 0x18}},
		{ABrOnCastFail, []byte{0xfb, 0x19}},
		{AAnyConvertExtern, []byte{0xfb, 0x1a}},
		{AExternConvertAny, []byte{0xfb, 0x1b}},
		{ARefI31, []byte{0xfb, 0x1c}},
		{AI31GetS, []byte{0xfb, 0x1d}},
		{AI31GetU, []byte{0xfb, 0x1e}},

		// Stack switching. Top-level single-byte opcodes 0xE0-0xE6 (0xE5
		// reserved). Operand encoding (typeidx / tagidx / handler-vec) is
		// the caller's responsibility — writeOpcode emits only the opcode
		// byte. Byte values pinned against wasm-tools / V8 December 2025.
		// See doc/wasm3-design.md and the M4 plan.
		{AContNew, []byte{0xe0}},
		{AContBind, []byte{0xe1}},
		{ASuspend, []byte{0xe2}},
		{AResume, []byte{0xe3}},
		{AResumeThrow, []byte{0xe4}},
		{ASwitch, []byte{0xe6}},
	}
	for _, tt := range tests {
		var w bytes.Buffer
		writeOpcode(&w, tt.as)
		if got := w.Bytes(); !bytes.Equal(got, tt.want) {
			t.Errorf("writeOpcode(%v) = % x, want % x", tt.as, got, tt.want)
		}
	}
}
