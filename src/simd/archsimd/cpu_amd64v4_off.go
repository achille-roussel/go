// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simd && !amd64.v4

package archsimd

// goamd64v4 reports whether the program is compiled for the GOAMD64=v4
// microarchitecture level or higher, which guarantees the AVX512F, BW,
// CD, DQ, and VL CPU features that make up the combined AVX512 feature.
const goamd64v4 = false
