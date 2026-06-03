// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// wasm3 mirror of mem_sbrk.go.
//
// mem_sbrk.go is gated `(plan9 || wasm) && !wasm3` because it
// maintains a linked list of free memory blocks via metadata
// headers — `memHdr` pointers are compared with `uintptr(
// unsafe.Pointer(p))` arithmetic (lines 121, 123, 133, 137, 144,
// 167, 171, 175, 179, 181) that anyref cannot represent.
//
// On wasm3 the malloc path is entirely intrinsified (mallocgc is
// emitted as `array.new_default` on the WasmGC backing) and the
// non-malloc clients of sysAllocOS / sysFreeOS / etc. (debuglog,
// malloc.go's heap-arena index) all live in subsystems that are
// already gated !wasm3. Nothing reachable from the wasm3 build
// calls these helpers, so the mirror is trap stubs.

const isSbrkPlatform = false

// bloc / blocMax / memlock / memRound / initBloc / sysReserveAlignedSbrk
// are referenced by mem_wasm3.go's sbrk (linear-memory bridge growth)
// and os_wasm3.go's runtime init. The wasm3 sbrk uses bloc as the
// linear-memory bump cursor and blocMax as the highest reserved
// page count; the metadata-headers free list from mem_sbrk.go is
// dead on wasm3 because sbrk-reserved linear memory is only ever
// used as a one-shot scratch arena for the WasmGC↔host boundary
// (runtime/wasm.WriteLinearMemory / ReadLinearMemory).

var bloc uintptr    // wasm3 linear-memory bump cursor
var blocMax uintptr // wasm3 highest reserved linear-memory page
var memlock mutex

func memRound(p uintptr) uintptr {
	return alignUp(p, physPageSize)
}

func initBloc() {
	bloc = memRound(firstmoduledata.end)
	blocMax = bloc
}

// sysReserveAlignedSbrk is called from mem.go:190 on sbrk platforms.
// On wasm3 the heap path doesn't reserve aligned regions (mallocgc
// is intrinsified to array.new); throw if reached.
func sysReserveAlignedSbrk(size, align uintptr) (unsafe.Pointer, uintptr) {
	throw("runtime.sysReserveAlignedSbrk unreachable on wasm3")
	return nil, 0
}

func sysAllocOS(n uintptr, _ string) unsafe.Pointer {
	throw("runtime.sysAllocOS unreachable on wasm3 (no linear-memory allocator; use runtime/wasm.WriteLinearMemory)")
	return nil
}

func sysFreeOS(v unsafe.Pointer, n uintptr) {
	throw("runtime.sysFreeOS unreachable on wasm3")
}

func sysUnusedOS(v unsafe.Pointer, n uintptr)            {}
func sysUsedOS(v unsafe.Pointer, n uintptr)              {}
func sysHugePageOS(v unsafe.Pointer, n uintptr)          {}
func sysNoHugePageOS(v unsafe.Pointer, n uintptr)        {}
func sysHugePageCollapseOS(v unsafe.Pointer, n uintptr)  {}
func sysMapOS(v unsafe.Pointer, n uintptr, _ string)     {}
func sysFaultOS(v unsafe.Pointer, n uintptr)             {}

func sysReserveOS(v unsafe.Pointer, n uintptr, _ string) unsafe.Pointer {
	throw("runtime.sysReserveOS unreachable on wasm3")
	return nil
}

func needZeroAfterSysUnusedOS() bool { return true }
