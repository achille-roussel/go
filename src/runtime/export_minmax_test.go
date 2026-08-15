// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// Export the min/max reduction kernels for testing. On amd64 with
// GOEXPERIMENT=simd these are the vectorized kernels; elsewhere they
// are the scalar fallbacks.

var MinUint8Kernel = minUint8
var MaxUint8Kernel = maxUint8
var MinInt8Kernel = minInt8
var MaxInt8Kernel = maxInt8
var MinInt16Kernel = minInt16
var MaxInt16Kernel = maxInt16
var MinUint16Kernel = minUint16
var MaxUint16Kernel = maxUint16
var MinInt32Kernel = minInt32
var MaxInt32Kernel = maxInt32
var MinUint32Kernel = minUint32
var MaxUint32Kernel = maxUint32
var MinInt64Kernel = minInt64
var MaxInt64Kernel = maxInt64
var MinUint64Kernel = minUint64
var MaxUint64Kernel = maxUint64
var MinFloat32Kernel = minFloat32
var MaxFloat32Kernel = maxFloat32
var MinFloat64Kernel = minFloat64
var MaxFloat64Kernel = maxFloat64
