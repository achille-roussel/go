// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simd && amd64.v2

package archsimd

// goamd64v2 reports whether the program is compiled for the GOAMD64=v2
// microarchitecture level or higher, which guarantees the SSE3, SSSE3,
// SSE4.1, SSE4.2, and POPCNT CPU features. No feature check in this
// package requires it yet; it is defined so that feature checks
// guaranteed at this level can use it.
const goamd64v2 = true
