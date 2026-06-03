// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// wasm3 mirror of tagptr_64bit.go.
//
// The non-wasm3 file packs a pointer + tag into a uint64 via
// `unsafe.Pointer(uintptr(...) << ...)` — anyref has no scalar
// form, fatal. On wasm3 every direct user of the pack/unpack
// methods is either gated off (lfstack via Rung 14,
// netpoll_epoll/_kqueue/_solaris/_windows not in the wasm3 build)
// or routed through the wasm3-specific stub mirror
// (lfstack_wasm3.go). The (taggedPointer).pointer / .tag /
// taggedPointerPack symbols still need to exist because the
// vestigial netpoll_wasm3.go references tagBits in the fdseq
// rollover (`(fdseq + 1) & (1<<tagBits - 1)`).

// addrBits / tagBits are referenced from malloc.go's startup
// sanity check (`if minTagBits > tagBits { throw(...) }`) and
// from netpoll_wasm3.go's fdseq mask. Use the same values as the
// upstream amd64 path.
const addrBits = 48
const tagBits = 64 - addrBits + tagAlignBits

// taggedPointerPack / pointer / tag are unreachable on wasm3
// but the symbols stay defined so any leftover references (and
// future cleanups) keep linking.

func taggedPointerPack(ptr unsafe.Pointer, tag uintptr) taggedPointer {
	throw("runtime.taggedPointerPack unreachable on wasm3 (no scalar pointer + tag packing)")
	return 0
}

func (tp taggedPointer) pointer() unsafe.Pointer {
	throw("runtime.(*taggedPointer).pointer unreachable on wasm3")
	return nil
}

func (tp taggedPointer) tag() uintptr {
	return uintptr(tp & (1<<tagBits - 1))
}
