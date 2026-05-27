// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// Package wasm — JSPI bridge for GOOS=js GOARCH=wasm3.
//
// JSPI (JavaScript Promise Integration) is V8's production-quality
// stack-switching primitive: a wasm function that imports a
// Promise-returning JS function can suspend mid-call, returning
// control to the JS event loop until the Promise resolves; on
// resumption the imported function returns.
//
// The Phase 1 primitives below are all VOID-result suspending
// imports. Returning values (externref or otherwise) through
// //go:wasmimport on wasm3 has a gap today — the wrapper signature
// drops the result — so this seam stays value-less for now. Phase
// 3+ uses the same pattern for gopark: a void suspending import
// that the JS-side scheduler resolves when goready fires.
//
// SleepMs is the canonical Phase 1 fixture: a Go program that
// calls SleepMs(150) suspends for ~150ms wall-clock, then resumes
// — proving JSPI works end-to-end through the wasm3 toolchain.

package wasm

// SleepMs suspends the calling wasm activation for at least ms
// milliseconds, then resumes. The JS shim wraps this import via
// WebAssembly.Suspending around a function that returns a
// setTimeout Promise.
//
//go:wasmimport gojs runtime.SleepMs
//go:noescape
func SleepMs(ms int32)
