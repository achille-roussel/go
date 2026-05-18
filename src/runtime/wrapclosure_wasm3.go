// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// wasm3WrapClosure is the wasm3-only closure-construction wrapper
// that walkClosure emits to turn a linear-memory captures struct
// into a wasmgc `(ref $go.closure.<sig>)`. The compiler intrinsifies
// the call at the SSA layer into OpWasm3MakeClosureRef, which emits
// `ref.func $funcsym; getValue captures; struct.new $closureCtx`.
//
// At runtime this body should never run — if it does, the SSA
// intrinsic failed to fire. The Go-level signature uses *any in
// the typecheck/_builtin decl so LookupRuntime can substitute the
// closure's actual func type as the polymorphic return, letting
// the call expression typecheck without an explicit conversion.
//
//go:nosplit
func wasm3WrapClosure(funcsym uintptr, captures unsafe.Pointer) unsafe.Pointer {
	_ = funcsym
	_ = captures
	throw("wasm3WrapClosure: not intrinsified by the wasm3 backend")
	return nil
}
