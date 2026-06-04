// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Empty file kept so the wasm3 build of internal/runtime/atomic
// retains an .s in the package. The compiler accepts body-less
// function declarations (Cas / Load / Storep / etc.) only when
// the package contains at least one assembly file, on the
// assumption that the asm provides the body. For wasm3 those
// bodies actually come from go:linkname directives in
// runtime/atomic_pointer_wasm3.go and friends (resolved at link
// time), but the compiler still wants to see *some* .s file.
//
// StorepNoWB used to live here as an FP-based linear-memory
// store; that was incompatible with wasm3's typed function ABI
// and produced malformed wasm (an uninitialized SP local fed
// into i64.add). The Go implementation in atomic_wasm3.go now
// owns StorepNoWB on wasm3.
