// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package sync

// copyCheckerCheck is a no-op on wasm3. The non-wasm3 version stores
// uintptr(unsafe.Pointer(c)) in c and compares on subsequent calls
// to detect copies of sync.Cond. WasmGC refs have no scalar address
// so the uintptr↔unsafe.Pointer conversion in that body would be
// rejected by SSA-gen. Single-thread scheduling on wasm3 also makes
// the race-protected version of "did someone copy me" unobservable
// — there's no other goroutine to copy from concurrently — so the
// degraded check costs no safety in practice. A program that copies
// a sync.Cond on wasm3 stays misbehaved (just as `go vet` would
// flag), but the runtime won't panic for it.
func copyCheckerCheck(c *copyChecker) {}
