// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package sync

import "unsafe"

// poolRaceAddr is unreachable on wasm3: race.Enabled is a build-time
// constant false on this target (the race detector requires
// thread-sanitizer instrumentation that the wasm3 toolchain doesn't
// emit), so every poolRaceAddr call site is dead. The non-wasm3
// body hashes uintptr(unsafe.Pointer(x)) into poolRaceHash; that
// conversion is rejected by SSA-gen on wasm3 (no scalar addresses
// for WasmGC refs), so we keep just the symbol with a benign body
// to preserve the linker's view of pool.go.
func poolRaceAddr(x any) unsafe.Pointer {
	return unsafe.Pointer(&poolRaceHash[0])
}
