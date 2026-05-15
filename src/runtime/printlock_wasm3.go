// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// printlock and printunlock are no-ops for GOARCH=wasm3 during the M2
// cutover. The standard implementations dereference getg().m and call
// lock(&debuglock); both g0/m0 and the runtime lock subsystem are not
// initialized at this stage of the cutover (no schedinit, no g/m
// wiring in the wasip1 entry), so the standard versions trap
// immediately. Print is single-goroutine for now — there is nothing
// to lock against.
//
// When the runtime fork lands (proc_wasm3.go with a real g0/m0
// setup, schedinit_wasm3.go with the lock subsystem online), these
// stubs can either gain a body or fall through to the default
// implementation in printlock.go.

func printlock() {}

func printunlock() {}
