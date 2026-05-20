// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm3

package runtime

import "unsafe"

//go:linkname memequal
//go:noescape
func memequal(a, b unsafe.Pointer, size uintptr) bool
