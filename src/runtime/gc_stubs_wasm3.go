// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// gc_stubs_wasm3.go provides empty type/var/func stand-ins for the
// GC-subsystem symbols that non-excluded runtime files still reference
// after the wasm3 GC exclusion. The wasm3 build deletes the marker,
// sweeper, scavenger, heap/cache, and page-allocator subsystems
// outright (per doc/wasm3-design.md §9 and project_wasm3_no_linear_malloc):
// heap data lives on the host WasmGC heap, allocation lowers to
// struct.new / array.new intrinsics, and the deleted files' Go-level
// state (gcWork queues, limiter events, scan stats, etc.) has no
// wasm3 analogue.
//
// Stubs here exist solely to keep the runtime package linkable.
// Every type is an empty struct, every var is the zero value, every
// func is a no-op. None of these should be reached at runtime; if any
// becomes reachable on wasm3 it indicates a non-excluded caller that
// needs its own wasm3-specific branch or exclusion.
//
// New stubs land here as additional GC-subsystem files are excluded;
// nothing is removed until the corresponding caller path is rewritten
// or excluded.

// limiterEvent is referenced as a struct field type in p (runtime2.go).
// The GC CPU limiter is part of mgcpacer.go, which is excluded.
type limiterEvent struct{}

// gcWork is referenced as struct field type in p (runtime2.go) and as
// parameter type in mcheckmark.go and preempt_noxreg.go. The mark
// queue itself lives in mgcwork.go, which is excluded.
type gcWork struct{}

// stackScanState is referenced as parameter type in preempt_noxreg.go.
// The stack-scan state lives in mgcmark.go, which is excluded.
type stackScanState struct{}

// sizeClassScanStats is referenced as element type in mstats.go's
// lastScanStats array (length gc.NumSizeClasses). The scan-statistics
// accumulator lives in mgcmark.go, which is excluded.
type sizeClassScanStats struct{}

// mcache is referenced as struct field type in p (runtime2.go). The
// per-P size-class cache lives in mcache.go, which is excluded.
type mcache struct{}

// mspan is referenced as struct field type in mlink (runtime2.go) and
// as parameter type in arena.go (the latter being itself excluded in
// a later island). The span metadata struct lives in mheap.go, which
// is excluded.
type mspan struct{}

// wbBuf is referenced as struct field type in p (runtime2.go). The
// write-barrier buffer lives in mwbbuf.go, which is excluded.
type wbBuf struct{}

// gcBits is referenced as struct field type in the pinner allocator
// (pinner.go). The pin bitmap manipulation lives in mheap.go /
// mbitmap.go, both excluded.
type gcBits struct{}

// pinner is referenced as struct field type in m (runtime2.go). The
// per-M pinner cache lives in pinner.go, which is excluded — runtime.
// Pin/Unpin become unsupported on wasm3 (the host WasmGC will not
// move objects, so Go-level pinning is a no-op concept).
type pinner struct{}

// Pinner is the exported runtime API (runtime.Pinner). The two
// methods are no-op on wasm3 — host-GC objects don't move, so
// Pin/Unpin are unnecessary. Kept exported to satisfy reflect.New /
// users that compile in pin calls.
type Pinner struct{}

// Pin records that obj should not be moved by the garbage collector.
// On wasm3 this is unconditionally a no-op since the host WasmGC
// does not move objects.
func (*Pinner) Pin(obj any) { _ = obj }

// Unpin releases all pinned objects.
func (*Pinner) Unpin() {}

// synctestBubble is referenced as struct field type in g (runtime2.go)
// and as parameter type in chan.go. The testing/synctest experimental
// runtime support lives in synctest.go, which is excluded — bubble
// semantics depend on the heap-special / mheap subsystems.
type synctestBubble struct {
	id uint64
}

// mSpanList is referenced from stack.go's pool of free stack spans.
// stack.go's growth machinery is part of the M2 exclusion set per
// doc/wasm3-design.md, but the surviving stack.go bits (signature
// touches in proc.go etc.) still mention mSpanList in declarations.
// Empty stub: any actual reach into mSpanList is in code paths that
// are unreachable on wasm3 (no Go-managed stack growth — wasm
// frames live in wasm locals).
type mSpanList struct{}
