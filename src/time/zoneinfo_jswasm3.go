// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// js/wasm3 timezone stub. The js/wasm version queries the JS Date
// timezone via syscall/js; on wasm3 that path is unavailable, so
// default Local to UTC. Tests that read the system timezone get UTC
// — good enough for the bottom-up test sweep until a runtime-side
// time source is wired through the wasm3 host.

package time

var platformZoneSources []string

func initLocal() {
	localLoc.name = "UTC"
	localLoc.zone = []zone{{name: "UTC", offset: 0, isDST: false}}
}
