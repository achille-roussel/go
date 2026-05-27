// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// Fake network poller for GOOS=js GOARCH=wasm3. Same shape as
// netpoll_fake.go — the JS event loop drives I/O through JSPI
// suspending host imports (Phase 6+), so the runtime's netpoll
// machinery doesn't actually poll anything.
//
// Filename `netpoll_jswasm3` (no implicit GOOS_GOARCH suffix
// because `jswasm3` doesn't match the `_GOOS_GOARCH` pattern);
// the explicit //go:build tag is the sole constraint.

package runtime

func netpollinit() {}

func netpollIsPollDescriptor(fd uintptr) bool { return false }

func netpollopen(fd uintptr, pd *pollDesc) int32 { return 0 }

func netpollclose(fd uintptr) int32 { return 0 }

func netpollarm(pd *pollDesc, mode int) {}

func netpollBreak() {}

func netpoll(delay int64) (gList, int32) { return gList{}, 0 }
