// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The explicit build constraint (in addition to the _wasm3 filename suffix)
// keeps this file out of the bootstrap toolchain: a bootstrap Go that predates
// wasm3 treats the unknown "wasm3" tag as false, whereas an unknown filename
// suffix would impose no constraint at all.
//go:build wasm3

package goarch

const (
	_ArchFamily          = WASM3
	_DefaultPhysPageSize = 65536
	_PCQuantum           = 1
	_MinFrameSize        = 0
	_StackAlign          = PtrSize
)
