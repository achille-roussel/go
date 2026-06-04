// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// Package reflectlite on wasm3.
//
// reflectlite is the runtime's interface to the type-system memory
// model. On non-wasm3 architectures that model is "linear-memory
// layout with byte offsets reached via uintptr arithmetic", and the
// existing type.go / value.go / swapper.go encode it directly:
// `n.data(off)` does `add(unsafe.Pointer(n.bytes), uintptr(off))`,
// `arrayAt(p, i, eltSize)` does `add(p, uintptr(i)*eltSize)`, and
// `Value.ptr` is an unsafe.Pointer treated as the address of a
// linearly-laid-out value. None of that translates: on wasm3 the
// type-system memory model is WasmGC structs (field *indices*, not
// byte offsets) reached via struct.get/set on typed refs, and there
// are no scalar addresses at all (uintptr↔unsafe.Pointer conversion
// is rejected by SSA-gen).
//
// This file provides a compile-only mirror of the reflectlite
// surface so packages that import reflectlite — most importantly
// `errors` (transitive prerequisite for `os`) — link cleanly on
// wasm3. The methods bodies are stubs: TypeOf / ValueOf return
// zero values, Kind queries answer Invalid, and the
// shape-introspection methods (Implements / AssignableTo / Elem /
// Field accessors) panic with a clear "not yet implemented on
// wasm3" message. Programs that don't actually exercise reflect
// at runtime — the common case for an os.Stdout.Write or a panic
// + recover that doesn't type-assert — run as if reflectlite were
// inert.
//
// A functional reflect implementation needs:
//   - #167 (WasmGC ref-globals carrying real abi.Type metadata, so
//     Kind/Size/Name/PkgPath can be read off a descriptor)
//   - M6 (#110): compiler-generated per-type accessor funcrefs hung
//     off the descriptor so Value.Field / Value.Index / Value.Set
//     can dispatch without knowing the static type at the call site.
//
// Both are tracked separately; until they land, this file just
// keeps the import graph honest.

package reflectlite

import (
	"internal/abi"
	"unsafe"
)

// Kind aliases the abi.Kind enum exactly as the non-wasm3 type.go
// does. The constants are the same values across architectures —
// they're metadata IDs, not memory offsets.
type Kind = abi.Kind

const Ptr = abi.Pointer

const (
	Interface = abi.Interface
	Slice     = abi.Slice
	String    = abi.String
	Struct    = abi.Struct
)

// Type is the reflectlite-level type descriptor handle. On non-wasm3
// it's implemented by rtype{*abi.Type}; on wasm3 the descriptor will
// eventually be a WasmGC ref-global (per #167), and the methods will
// dispatch through compiler-generated per-type accessors. For now
// the methods are stubs that either panic (shape introspection) or
// return zero values (queries that errors only checks for nil).
type Type interface {
	Name() string
	PkgPath() string
	Size() uintptr
	Kind() Kind
	Implements(u Type) bool
	AssignableTo(u Type) bool
	Comparable() bool
	String() string
	Elem() Type

	common() *abi.Type
	uncommon() *abi.UncommonType
}

// rtype is the wasm3 placeholder Type implementation. The *abi.Type
// field is nil today (#167 makes it a real WasmGC ref later); the
// methods don't dereference it.
type rtype struct {
	t *abi.Type
}

func (r rtype) Name() string                 { return "" }
func (r rtype) PkgPath() string              { return "" }
func (r rtype) Size() uintptr                { return 0 }
func (r rtype) Kind() Kind                   { return abi.Invalid }
func (r rtype) Implements(u Type) bool       { panic("reflectlite: Implements not implemented on wasm3") }
func (r rtype) AssignableTo(u Type) bool     { panic("reflectlite: AssignableTo not implemented on wasm3") }
func (r rtype) Comparable() bool             { panic("reflectlite: Comparable not implemented on wasm3") }
func (r rtype) String() string               { return "<wasm3 stub Type>" }
func (r rtype) Elem() Type                   { panic("reflectlite: Type.Elem not implemented on wasm3") }
func (r rtype) common() *abi.Type            { return r.t }
func (r rtype) uncommon() *abi.UncommonType  { return nil }

// TypeOf returns the dynamic type of i as a reflectlite.Type. On
// non-wasm3 it reads `i`'s eface.type field directly; on wasm3 the
// eface.type slot holds an anyref to a descriptor ref-global (per
// the iface poly migration), which the wasm3 backend exposes via
// compiler intrinsics but doesn't yet route through reflectlite.
// Return a zero rtype until the plumbing lands — callers (errors)
// use this for identity checks that would panic anyway on a
// non-stub mismatch.
func TypeOf(i any) Type {
	return rtype{}
}

// Value is the reflectlite-level value handle. The non-wasm3
// representation packs (typ, ptr, flag) into 3 machine words and
// reaches the underlying data via unsafe pointer arithmetic; the
// wasm3 representation will eventually carry an anyref to the
// boxed value plus a flag word. For now the struct is empty
// except for a flag so the Value methods (Kind / IsNil / Set) can
// answer something benign.
type Value struct {
	flag uintptr
}

// ValueError is reused as-is from the non-wasm3 surface — it's a
// plain struct with no memory-model dependencies.
type ValueError struct {
	Method string
	Kind   Kind
}

func (e *ValueError) Error() string {
	if e.Kind == abi.Invalid {
		return "reflect: call of " + e.Method + " on zero Value"
	}
	return "reflect: call of " + e.Method + " on " + "<wasm3 Value>"
}

func (v Value) Kind() Kind        { return abi.Invalid }
func (v Value) IsNil() bool       { return true }
func (v Value) IsValid() bool     { return false }
func (v Value) Elem() Value       { panic("reflectlite: Value.Elem not implemented on wasm3") }
func (v Value) Set(x Value)       { panic("reflectlite: Value.Set not implemented on wasm3") }
func (v Value) Type() Type        { return rtype{} }
func (v Value) numMethod() int    { return 0 }
func (v Value) CanSet() bool      { return false }

// ValueOf returns a Value for the supplied interface. Same stub
// rationale as TypeOf — until #167 / M6 land, the wasm3 reflectlite
// can't introspect arbitrary values, so we return the zero Value
// that subsequent methods (IsNil / Kind / Elem) answer benignly
// for.
func ValueOf(i any) Value {
	return Value{}
}

// Swapper would return a swap closure backed by element-typed
// memmove on non-wasm3. The wasm3 substrate is array.copy on the
// $go.bytes-style backing rather than byte-by-byte memmove, and
// the per-type slice element index lives at the ssa-gen layer
// (OpWasm3ArrayElemRef / OpWasm3ArrayElemSet). Plumbing reflect
// through to those ops needs the M6 per-type accessor work too,
// so for now Swapper panics — sort.Slice is the main caller and
// it doesn't fire from anything `os` exercises.
func Swapper(slice any) func(i, j int) {
	panic("reflectlite: Swapper not implemented on wasm3")
}

// toType is the internal helper non-wasm3 uses to convert an
// *abi.Type to a Type. Kept here for symbol parity with the
// non-wasm3 file — it's referenced by reflectlite's own internal
// callers (which are all gated !wasm3 already, but the symbol may
// be linknamed from reflect).
//
//go:noinline
func toType(t *abi.Type) Type {
	return rtype{t: t}
}

// unsafe import is retained for symbol parity; nothing references
// unsafe in the wasm3 stubs but the non-wasm3 surface uses it
// extensively, so build-tagged code that linknames into reflectlite
// (for example go.dev/issue/67401 hall-of-shame imports) stays
// honest.
var _ = unsafe.Sizeof(uintptr(0))
