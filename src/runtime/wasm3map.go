// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// wasm3MapUsed / wasm3MapCap / wasm3MapKeys / wasm3MapValues and their
// *Set counterparts are stub functions whose calls are intrinsified at
// SSA time to the OpWasm3MapUsed / Cap / Keys / Values (+ Set) field-
// access ops on the per-type $go.map.<K,V> WasmGC struct. The bodies
// are unreachable — every call site goes through the ssagen intrinsic
// which replaces the call with the corresponding wasm3 SSA op carrying
// the map's *types.Type as v.Aux. See cmd/compile/internal/ssagen/
// intrinsics.go for the dispatch.
//
// These exist so the per-(K,V) generated map operation functions (in
// reflectdata/wasm3_mapgen.go) can construct typed IR bodies that
// read/write the wasm3-only $go.map.<K,V> fields without needing a
// Go-level proxy struct. The compiler's typecheck.LookupRuntime
// substitutes the `any` placeholders in the Keys/Values stubs with
// the concrete K and V types per call, so the IR sees typed []K /
// []V rather than []any.

//go:noinline
func wasm3MapUsed(m unsafe.Pointer) uintptr { panic("wasm3MapUsed: not intrinsified") }

//go:noinline
func wasm3MapCap(m unsafe.Pointer) uintptr { panic("wasm3MapCap: not intrinsified") }

//go:noinline
func wasm3MapKeys(m unsafe.Pointer) []any { panic("wasm3MapKeys: not intrinsified") }

//go:noinline
func wasm3MapValues(m unsafe.Pointer) []any { panic("wasm3MapValues: not intrinsified") }

//go:noinline
func wasm3MapUsedSet(m unsafe.Pointer, n uintptr) { panic("wasm3MapUsedSet: not intrinsified") }

//go:noinline
func wasm3MapCapSet(m unsafe.Pointer, n uintptr) { panic("wasm3MapCapSet: not intrinsified") }

//go:noinline
func wasm3MapKeysSet(m unsafe.Pointer, keys []any) { panic("wasm3MapKeysSet: not intrinsified") }

//go:noinline
func wasm3MapValuesSet(m unsafe.Pointer, values []any) { panic("wasm3MapValuesSet: not intrinsified") }
