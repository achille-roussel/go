// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm3

package runtime

//go:nosplit
func throwOnSystemstack(s string) {
	systemstack(func() {
		print("fatal error: ")
		printindented(s) // logically printpanicval(s), but avoids convTstring write barrier
		print("\n")
	})
}
