// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// M0: copied from rt0_wasip1_wasm.s. The only divergence from the wasm version
// is the entry symbol name, which the linker derives as _rt0_<GOARCH>_<GOOS>
// (see cmd/link/internal/ld/lib.go). See doc/wasm3-design.md.

#include "go_asm.h"
#include "textflag.h"

TEXT _rt0_wasm3_wasip1(SB),NOSPLIT,$0
	MOVD $runtime·wasmStack+(m0Stack__size-16)(SB), SP

	I32Const $0 // entry PC_B
	Call runtime·rt0_go(SB)
	Drop
	Call wasm_pc_f_loop(SB)

	Return

TEXT _rt0_wasm3_wasip1_lib(SB),NOSPLIT,$0
	Call _rt0_wasm3_wasip1(SB)
	Return
