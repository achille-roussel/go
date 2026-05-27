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

// WasmPark suspends the calling wasm activation, registering parkID
// in a JS-side resolver Map. The activation stays parked until
// WasmReady(parkID) is called from any wasm activation (the same
// one, or another goroutine via the JS scheduler). parkID is a
// caller-supplied identifier — the Go scheduler stashes a fresh
// integer per gopark call, keyed off the goroutine pointer.
//
// This is the Phase 3 primitive gopark lowers to. WasmReady is the
// goready side.
//
//go:wasmimport gojs runtime.WasmPark
//go:noescape
func WasmPark(parkID int32)

// WasmReady resolves a Promise registered by an earlier WasmPark
// call. The corresponding suspended wasm activation resumes on the
// next JS event-loop tick. If parkID has no pending park, the call
// is a no-op (matches the goready semantics: ready-before-park is
// fine, the next park races and wins).
//
//go:wasmimport gojs runtime.WasmReady
//go:noescape
func WasmReady(parkID int32)
