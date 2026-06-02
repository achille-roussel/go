// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// Stub execution-tracer entry points for wasm3 (trace.go is tagged
// !wasm3 because it depends on goroutine internals + pclntab — both
// excluded on wasm3). Functions return errors / empty results; runtime/
// trace and the testing tracer downgrade gracefully.

package runtime

// errTraceUnsupportedW3 is a tiny error implementation that doesn't
// pull in the errors package (which would cycle through runtime).
type errTraceUnsupportedW3 struct{}

func (errTraceUnsupportedW3) Error() string {
	return "runtime/trace: execution tracing not supported on GOARCH=wasm3"
}

// StartTrace returns an error on wasm3 — no tracer is wired up.
func StartTrace() error { return errTraceUnsupportedW3{} }

// StopTrace is a no-op on wasm3.
func StopTrace() {}

// ReadTrace returns nil on wasm3 — there is no trace buffer.
func ReadTrace() []byte { return nil }
