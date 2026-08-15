// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// Export the min/max reduction kernels for testing. On amd64 with
// GOEXPERIMENT=simd these are the vectorized kernels; elsewhere they
// are the scalar fallbacks.

var MinUint8Kernel = minUint8
var MaxUint8Kernel = maxUint8
var MinInt16Kernel = minInt16
var MaxInt16Kernel = maxInt16
