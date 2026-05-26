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
