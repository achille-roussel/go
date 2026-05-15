// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm3

package runtime

func printlock() {
	mp := getg().m
	mp.locks++ // do not reschedule between printlock++ and lock(&debuglock).
	mp.printlock++
	if mp.printlock == 1 {
		lock(&debuglock)
	}
	mp.locks-- // now we know debuglock is held and holding up mp.locks for us.
}

func printunlock() {
	mp := getg().m
	mp.printlock--
	if mp.printlock == 0 {
		unlock(&debuglock)
	}
}
