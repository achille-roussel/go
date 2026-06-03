// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import (
	"internal/coverage/rtcov"
	_ "unsafe" // for go:linkname
)

// wasm3 mirror of covercounter.go.
//
// The non-wasm3 body walks moduledata.covctrs (uintptr) and casts
// it to (*uint32) — `unsafe.Pointer(uintptr)` rejected. wasm3's
// stub pclntab (#166) doesn't emit cover counter blobs either, so
// the body would have no work to do regardless.

//go:linkname coverage_getCovCounterList internal/coverage/cfile.getCovCounterList
func coverage_getCovCounterList() []rtcov.CovCounterBlob {
	return nil
}
