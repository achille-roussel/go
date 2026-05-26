// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ir

import "sync"

// Wasm3MakeSliceElemTypes carries the slice's element *types.Type
// from walkMakeSlice (which has the Go-level type info) to the
// SSA-time intrinsic for runtime.makeslice (which receives only the
// already-built SSA args). Keyed on the *ir.CallExpr produced by
// mkcall, the entry is consumed (LoadAndDelete) by the intrinsic so
// the map shrinks back to empty after each function's SSA build.
// wasm3 only — other arches don't register this intrinsic and the
// map stays empty.
//
// The map lives in package ir because both cmd/compile/internal/walk
// (the producer) and cmd/compile/internal/ssagen (the consumer)
// import ir, and the key (*ir.CallExpr) is an ir-package value.
// Value type is interface{} (a *types.Type at the call sites); the
// types package itself is a separate import, kept out of this file
// to avoid widening the ir package's import surface.
var Wasm3MakeSliceElemTypes sync.Map

// Wasm3MakeMapTypes carries the map's *types.Type from walkMakeMap
// to the SSA-time intrinsic for runtime.makemap / runtime.makemap64
// / runtime.makemap_small. Same producer-to-consumer pattern as
// Wasm3MakeSliceElemTypes — keyed on the *ir.CallExpr, value is the
// map *types.Type (so the intrinsic emits OpWasm3MakeMap with the
// right $go.map.<K,V> wasm type index).
var Wasm3MakeMapTypes sync.Map
