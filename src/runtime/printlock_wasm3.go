// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// printlock and printunlock are no-ops for GOARCH=wasm3.
//
// The standard implementation increments getg().m.printlock and,
// on the first acquisition, calls lock(&debuglock). lock() reaches
// gopark on contention — wasm3's proc stub leaves gopark as an
// `unreachable` trap, so any uncontended-but-contested call into
// the lock subsystem crashes. Until the goroutine machinery's own
// milestone (M4) brings up a real scheduler with a working park /
// goready pair, wasm3 stays single-goroutine; printlock has
// nothing to lock against.
func printlock() {}

func printunlock() {}
