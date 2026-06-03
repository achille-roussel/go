// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// wasm3 mirror of debuglog.go.
//
// debuglog.go is tagged !wasm3 because its per-M ring buffer
// allocates dloggerImpl records via sysAllocOS and threads them
// through a global lock-free list using
// `(*dloggerImpl)(unsafe.Pointer(atomic.Loaduintptr(headp)))`
// pointer-as-uintptr storage that anyref cannot represent.
//
// On wasm3 there's no per-M debug buffer — the runtime's debug
// log was an in-memory ring meant to be dumped on panic, and
// wasm3 panics degrade to print + exit (see panic_wasm3.go).
// The active build tag is the default `!debuglog`, so dlogger
// aliases to dloggerFake and every callsite chains no-op
// methods. This file provides only what debuglog_off.go needs:
//
//   - `type dloggerImpl struct{}` — referenced as the return type
//     of getCachedDlogger / putCachedDlogger.
//   - `type dloggerFake struct{}` + the no-op method set that
//     dlogger callers (dlog().b(x).i(y).end()) chain through.
//   - `func dlogFake() dloggerFake` — debuglog_off.go's dlog1
//     returns the result.

type dloggerImpl struct{}

type dloggerFake struct{}

func dlogFake() dloggerFake { return dloggerFake{} }

//go:nosplit
func (l dloggerFake) end() {}

//go:nosplit
func (l dloggerFake) b(x bool) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) i(x int) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) i8(x int8) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) i16(x int16) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) i32(x int32) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) i64(x int64) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) u(x uint) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) uptr(x uintptr) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) u8(x uint8) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) u16(x uint16) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) u32(x uint32) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) u64(x uint64) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) hex(x uint64) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) p(x any) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) s(x string) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) hexdump(p unsafe.Pointer, bytes uintptr) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) pc(x uintptr) dloggerFake { return l }

//go:nosplit
func (l dloggerFake) traceback(x []uintptr) dloggerFake { return l }
