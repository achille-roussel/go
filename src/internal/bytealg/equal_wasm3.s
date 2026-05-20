// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// memequal/memequal_varlen for GOARCH=wasm3 are implemented in Go.
// See src/runtime/memequal_wasm3.go and src/runtime/memequal_varlen_wasm3.go
// — the asm bodies in equal_wasm.s use SP-relative frame loads
// (`I64Load a+0(FP)`) that the wasm3 obj backend can't lower, and
// the wasm3 typed calling convention passes arguments in wasm
// function params instead of the linear-memory frame this asm
// expects. The file remains so the directory still mirrors the
// other arches; the actual function symbols are provided by the Go
// shims.
