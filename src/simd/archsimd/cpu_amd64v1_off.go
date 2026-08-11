// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.simd && !amd64.v1

package archsimd

// goamd64v1 reports whether the program is compiled for the GOAMD64=v1
// microarchitecture level or higher, the baseline for GOARCH amd64.
// No feature check in this package requires it yet; it is defined so
// that feature checks guaranteed at this level can use it.
const goamd64v1 = false
