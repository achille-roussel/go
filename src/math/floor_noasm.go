// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !386 && !amd64 && !arm64 && !loong64 && !ppc64 && !ppc64le && !riscv64 && !s390x && !wasm && !wasm3

// wasm3 is excluded here too; see floor_wasm3.go for the wasm3
// implementation (haveArchFloor=true, archFloor/Ceil/Trunc
// intrinsified to single wasm instructions).

package math

const haveArchFloor = false

func archFloor(x float64) float64 {
	panic("not implemented")
}

const haveArchCeil = false

func archCeil(x float64) float64 {
	panic("not implemented")
}

const haveArchTrunc = false

func archTrunc(x float64) float64 {
	panic("not implemented")
}
