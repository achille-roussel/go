// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Compiler-policy placeholder. The Go compiler requires an .s
// file in a package that contains body-less function declarations
// (bytealg.MakeNoZero at bytealg.go:123 is the one here on
// wasm3 — its body comes from the runtime via go:linkname). No
// asm code goes in this file; the actual wasm3 implementations
// of memequal / memequal_varlen / IndexByte / IndexByteString
// live in runtime/memequal_wasm3.go and bytealg/indexbyte_generic
// .go. indexbyte_wasm3.s, which used to play the same placeholder
// role, was deleted since one .s per package is enough.
