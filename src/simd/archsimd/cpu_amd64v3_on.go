// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simd && amd64.v3

package archsimd

// goamd64v3 reports whether the program is compiled for the GOAMD64=v3
// microarchitecture level or higher, which guarantees the AVX, AVX2,
// and FMA CPU features.
const goamd64v3 = true
