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

// Wasm3MapClearTypes carries the map's *types.Type from walkClear /
// mapClear to the SSA-time intrinsic for runtime.mapclear. Same
// side-channel pattern as Wasm3MakeMapTypes.
var Wasm3MapClearTypes sync.Map

// Wasm3MapHelperTypes carries the map's *types.Type from the per-(K,V)
// map operation generators (reflectdata/wasm3_mapgen.go) to the SSA-
// time intrinsics for the runtime stubs wasm3MapUsed / wasm3MapCap /
// wasm3MapKeys / wasm3MapValues (+ Set counterparts). Same side-channel
// pattern as Wasm3MapClearTypes — keyed on the *ir.CallExpr the
// generator builds when emitting a stub call, value is the map type.
var Wasm3MapHelperTypes sync.Map

// Wasm3MakeChanTypes carries the chan's *types.Type from
// walkMakeChan to the SSA-time intrinsic for runtime.makechan /
// runtime.makechan64. Same side-channel pattern as
// Wasm3MakeMapTypes — keyed on the *ir.CallExpr the walker
// produces, value is the chan *types.Type. The intrinsic bypasses
// runtime.makechan entirely (emitting struct.new_default
// $go.chan.<T>) so the *chantype arg's i64-vs-anyref calling-
// convention mismatch never arises.
var Wasm3MakeChanTypes sync.Map

// Wasm3ChanSendTypes / Wasm3ChanRecvTypes carry the chan type
// from walkSend / walkRecv / convas to the SSA-time intrinsics
// for runtime.chansend1 / runtime.chanrecv1. Same side-channel
// pattern as Wasm3MakeChanTypes.
var (
	Wasm3ChanSendTypes sync.Map
	Wasm3ChanRecvTypes sync.Map
)
