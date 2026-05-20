// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// IndexByte/IndexByteString for GOARCH=wasm3 are implemented in Go,
// in indexbyte_generic.go (this directory). The asm body in
// indexbyte_wasm.s uses SP-relative frame loads and a memchr
// helper using R0/R1/R2 wasm "registers" that the wasm3 obj
// backend can't lower into the typed-function ABI. The file
// remains so the directory still mirrors the other arches.
