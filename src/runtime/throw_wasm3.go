// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// throwOnSystemstack on wasm3 prints the fatal-error preamble
// directly. The default goes through systemstack(func() {...})
// which captures `s string`; on wasm3 the resulting closureCtx
// is a wasmgc ref that ssagen tries to i64.store into a stack
// temp before the call, which trips wasm validation. wasm3 has
// no goroutine-stack-switching to motivate the systemstack hop
// anyway; we can just print on the current "stack".
//
//go:nosplit
func throwOnSystemstack(s string) {
	print("fatal error: ")
	printindented(s)
	print("\n")
}
