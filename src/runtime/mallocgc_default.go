// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm3

package runtime

import (
	"internal/goexperiment"
	"internal/runtime/gc"
	"unsafe"
)

// Allocate an object of size bytes.
// Small objects are allocated from the per-P cache's free lists.
// Large objects (> 32 kB) are allocated straight from the heap.
//
// mallocgc should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/gopkg
//   - github.com/bytedance/sonic
//   - github.com/cloudwego/frugal
//   - github.com/cockroachdb/cockroach
//   - github.com/cockroachdb/pebble
//   - github.com/ugorji/go/codec
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname mallocgc
func mallocgc(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	if doubleCheckMalloc {
		if gcphase == _GCmarktermination {
			throw("mallocgc called with gcphase == _GCmarktermination")
		}
	}

	// Short-circuit zero-sized allocation requests.
	if size == 0 {
		return unsafe.Pointer(&zerobase)
	}

	if sizeSpecializedMallocEnabled && size < uintptr(len(mallocNoScanTable)) {
		if typ == nil || !typ.Pointers() {
			if size >= maxTinySize {
				return mallocNoScanTable[size](size, typ, needzero)
			}
			return mallocgcTinySC2(size, typ, needzero)
		} else {
			if !needzero {
				throw("objects with pointers must be zeroed")
			}
			return mallocScanTable[size](size, typ, needzero)
		}
	}

	// It's possible for any malloc to trigger sweeping, which may in
	// turn queue finalizers. Record this dynamic lock edge.
	// N.B. Compiled away if lockrank experiment is not enabled.
	lockRankMayQueueFinalizer()

	// Pre-malloc debug hooks.
	if debug.malloc {
		if x := preMallocgcDebug(size, typ); x != nil {
			return x
		}
	}

	// For ASAN, we allocate extra memory around each allocation called the "redzone."
	// These "redzones" are marked as unaddressable.
	var asanRZ uintptr
	if asanenabled {
		asanRZ = redZoneSize(size)
		size += asanRZ
	}

	// Assist the GC if needed. (On the reuse path, we currently compensate for this;
	// changes here might require changes there.)
	if gcBlackenEnabled != 0 {
		deductAssistCredit(size)
	}

	// Actually do the allocation.
	var x unsafe.Pointer
	var elemsize uintptr
	if sizeSpecializedMallocEnabled {
		if size <= maxSmallSize-gc.MallocHeaderSize {
			if typ == nil || !typ.Pointers() {
				x, elemsize = mallocgcSmallNoscan(size, typ, needzero)
			} else {
				if !needzero {
					throw("objects with pointers must be zeroed")
				}
				if heapBitsInSpan(size) {
					x, elemsize = mallocgcSmallScanNoHeader(size, typ)
				} else {
					x, elemsize = mallocgcSmallScanHeader(size, typ)
				}
			}
		} else {
			x, elemsize = mallocgcLarge(size, typ, needzero)
		}
	} else {
		if size <= maxSmallSize-gc.MallocHeaderSize {
			if typ == nil || !typ.Pointers() {
				// tiny allocations might be kept alive by other co-located values.
				// Make sure secret allocations get zeroed by avoiding the tiny allocator
				// See go.dev/issue/76356
				gp := getg()
				if size < maxTinySize && gp.secret == 0 {
					x, elemsize = mallocgcTiny(size, typ)
				} else {
					x, elemsize = mallocgcSmallNoscan(size, typ, needzero)
				}
			} else {
				if !needzero {
					throw("objects with pointers must be zeroed")
				}
				if heapBitsInSpan(size) {
					x, elemsize = mallocgcSmallScanNoHeader(size, typ)
				} else {
					x, elemsize = mallocgcSmallScanHeader(size, typ)
				}
			}
		} else {
			x, elemsize = mallocgcLarge(size, typ, needzero)
		}
	}

	gp := getg()
	if goexperiment.RuntimeSecret && gp.secret > 0 {
		// Mark any object allocated while in secret mode as secret.
		// This ensures we zero it immediately when freeing it.
		addSecret(x, size)
	}

	// Notify sanitizers, if enabled.
	if raceenabled {
		racemalloc(x, size-asanRZ)
	}
	if msanenabled {
		msanmalloc(x, size-asanRZ)
	}
	if asanenabled {
		// Poison the space between the end of the requested size of x
		// and the end of the slot. Unpoison the requested allocation.
		frag := elemsize - size
		if typ != nil && typ.Pointers() && !heapBitsInSpan(elemsize) && size <= maxSmallSize-gc.MallocHeaderSize {
			frag -= gc.MallocHeaderSize
		}
		asanpoison(unsafe.Add(x, size-asanRZ), asanRZ)
		asanunpoison(x, size-asanRZ)
	}
	if valgrindenabled {
		valgrindMalloc(x, size-asanRZ)
	}

	// Adjust our GC assist debt to account for internal fragmentation.
	if gcBlackenEnabled != 0 && elemsize != 0 {
		if assistG := getg().m.curg; assistG != nil {
			assistG.gcAssistBytes -= int64(elemsize - size)
		}
	}

	// Post-malloc debug hooks.
	if debug.malloc {
		postMallocgcDebug(x, elemsize, typ)
	}
	return x
}
