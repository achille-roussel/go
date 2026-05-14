// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package goarch

// IsWasmFamily is 1 on the WebAssembly architectures (wasm and wasm3) and 0
// elsewhere. Use it instead of IsWasm for code that applies to the whole
// WebAssembly family rather than to the original linear-memory wasm port
// specifically. IsWasm and IsWasm3 are exactly one of {0,1} and never both 1,
// so the bitwise OR is itself 0 or 1.
const IsWasmFamily = IsWasm | IsWasm3
