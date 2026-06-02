// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// Stub profiler entry points for wasm3 (mprof.go and cpuprof.go are
// tagged !wasm3 because they depend on the excluded GC subsystem +
// goroutine internals). All functions are no-ops or return zero;
// profiling is not supported on wasm3.

package runtime

// SetBlockProfileRate is a no-op on wasm3.
func SetBlockProfileRate(rate int) { _ = rate }

// SetMutexProfileFraction is a no-op on wasm3 — always returns 0.
func SetMutexProfileFraction(rate int) int { _ = rate; return 0 }

// SetCPUProfileRate is a no-op on wasm3.
func SetCPUProfileRate(hz int) { _ = hz }

// StackRecord describes a single execution stack — minimal wasm3 stub.
type StackRecord struct {
	Stack0 [32]uintptr
}

// Stack returns Stack0[:n] where n is the number of valid pcs in r.
// On wasm3 it always returns nil (no recorded pcs).
func (r *StackRecord) Stack() []uintptr { return nil }

// BlockProfileRecord describes blocking events from sync primitives.
// Minimal wasm3 stub.
type BlockProfileRecord struct {
	Count  int64
	Cycles int64
	StackRecord
}

// MemProfileRecord describes the live objects allocated by a particular
// call sequence (stack trace). Minimal wasm3 stub.
type MemProfileRecord struct {
	AllocBytes, FreeBytes     int64
	AllocObjects, FreeObjects int64
	Stack0                    [32]uintptr
}

func (r *MemProfileRecord) InUseBytes() int64   { return r.AllocBytes - r.FreeBytes }
func (r *MemProfileRecord) InUseObjects() int64 { return r.AllocObjects - r.FreeObjects }
func (r *MemProfileRecord) Stack() []uintptr    { return nil }

// MemProfile returns 0, true on wasm3 (no records available).
func MemProfile(p []MemProfileRecord, inuseZero bool) (n int, ok bool) {
	_ = p
	_ = inuseZero
	return 0, true
}

// BlockProfile returns 0, true on wasm3.
func BlockProfile(p []BlockProfileRecord) (n int, ok bool) { _ = p; return 0, true }

// MutexProfile returns 0, true on wasm3.
func MutexProfile(p []BlockProfileRecord) (n int, ok bool) { _ = p; return 0, true }

// ThreadCreateProfile returns 0, true on wasm3.
func ThreadCreateProfile(p []StackRecord) (n int, ok bool) { _ = p; return 0, true }

// GoroutineProfile returns 0, true on wasm3.
func GoroutineProfile(p []StackRecord) (n int, ok bool) { _ = p; return 0, true }

// CPUProfile is deprecated; on wasm3 returns nil.
func CPUProfile() []byte { return nil }
