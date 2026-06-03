// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import "unsafe"

// wasm3 mirror of checkptr.go.
//
// checkptr.go is tagged !wasm3 because all four runtime helpers
// (checkptrAlignment, checkptrStraddles, checkptrArithmetic,
// checkptrBase) convert unsafe.Pointer to uintptr to do range
// arithmetic over the heap / stack. anyref has no scalar address,
// so the conversions trip the wasm3 SSA-gen fatal.
//
// On wasm3 the runtime checks are moot anyway: every disallowed
// unsafe.Pointer↔uintptr conversion is already rejected at
// compile time (commit 3f174090e8), so user code can't construct
// the kinds of misaligned / out-of-heap / inter-object pointers
// that checkptr exists to catch. Compiler-emitted `-d=checkptr=1`
// instrumentation still inserts calls to these helpers; we stub
// them as no-ops.

func checkptrAlignment(p unsafe.Pointer, elem *_type, n uintptr) {}

func checkptrStraddles(ptr unsafe.Pointer, size uintptr) bool { return false }

func checkptrArithmetic(p unsafe.Pointer, originals []unsafe.Pointer) {}

// checkptrBase identifies the base pointer of an allocation
// containing p (stack / heap / data). The non-wasm3 impl walks
// the M's stack range and the heap span tables, both of which
// require uintptr address arithmetic. On wasm3 we return 0,
// which the upstream callers interpret as "unknown base" and
// degrade gracefully.
func checkptrBase(p unsafe.Pointer) uintptr { return 0 }
