// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// mallocgc is the runtime's allocation entry — convT* (interface
// boxing), growslice, append, etc. funnel through it on other
// platforms. On wasm3 every allocation site has been intrinsified
// at the SSA layer (OpWasm3MakeSlice → array.new_default,
// OpWasm3StructNewDefault → struct.new_default, …), so a live call
// reaching this body indicates a missed intrinsification — trap so
// it surfaces immediately rather than silently corrupting state
// against a defunct linear-memory bump arena. The symbol must stay
// (other runtime files linkname-bind it).
//
//go:linkname mallocgc
//go:nosplit
func mallocgc(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	_ = size
	_ = typ
	_ = needzero
	throw("wasm3: runtime.mallocgc called — allocation site failed to intrinsify")
	return nil
}
