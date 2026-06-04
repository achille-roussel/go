// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm3

package sync

import (
	"sync/atomic"
	"unsafe"
)

// copyCheckerCheck holds the copy-detection body for sync.Cond on
// every arch where unsafe.Pointer↔uintptr round-trips work. The body
// is the original three-step self-pointer check; see copyChecker
// definition in cond.go for the protocol.
func copyCheckerCheck(c *copyChecker) {
	if uintptr(*c) != uintptr(unsafe.Pointer(c)) &&
		!atomic.CompareAndSwapUintptr((*uintptr)(c), 0, uintptr(unsafe.Pointer(c))) &&
		uintptr(*c) != uintptr(unsafe.Pointer(c)) {
		panic("sync.Cond is copied")
	}
}
