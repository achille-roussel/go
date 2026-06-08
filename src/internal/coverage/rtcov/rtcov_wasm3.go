// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 mirror of rtcov.go. Coverage output requires file I/O
// which the wasm3 runtime does not support; AddMeta is therefore
// a no-op stub that returns slot+1 so the runtime.addCovMeta
// caller (covermeta.go) does not throw on "coverage package map
// collision". On the shared !wasm3 path Meta.List is a package-
// level struct field read via LoweredAddr{Meta} + OffPtr + i64.load
// for each slice header word — the documented #167-pending bridge
// gap that surfaces here as 4 wasm3check warnings on hello.wasm.
// Stubbing AddMeta clears both AddMeta and runtime.addCovMeta
// hits (the latter inherits the same patterns when it calls into
// AddMeta).
//
// The Meta var, CovMetaBlob, and CovCounterBlob types are
// preserved for type-identity with the !wasm3 file (other
// packages may reference them via type assertion or linkname),
// but Meta.List stays empty on wasm3 — no coverage data is
// collected.

package rtcov

import "unsafe"

// CovMetaBlob is a container for holding the meta-data symbol (an
// RODATA variable) for an instrumented Go package. Here "p" points to
// the symbol itself, "len" is the length of the sym in bytes, and
// "hash" is an md5sum for the sym computed by the compiler. When
// the init function for a coverage-instrumented package executes, it
// will make a call into the runtime which will create a covMetaBlob
// object for the package and chain it onto a global list.
type CovMetaBlob struct {
	P                  *byte
	Len                uint32
	Hash               [16]byte
	PkgPath            string
	PkgID              int
	CounterMode        uint8 // coverage.CounterMode
	CounterGranularity uint8 // coverage.CounterGranularity
}

// CovCounterBlob is a container for encapsulating a counter section
// (BSS variable) for an instrumented Go module. Here "counters"
// points to the counter payload and "len" is the number of uint32
// entries in the section.
type CovCounterBlob struct {
	Counters *uint32
	Len      uint64
}

// Meta mirrors the !wasm3 layout but stays empty on wasm3: coverage
// instrumentation is not supported by the wasm3 runtime (no file I/O
// to emit the meta-data payload). AddMeta below is a no-op.
var Meta struct {
	List                       []CovMetaBlob
	PkgMap                     map[int]int
	hardCodedListNeedsUpdating bool
}

// wasm3CovSlot is a monotonically increasing counter that AddMeta
// returns as the package's ID. Coverage isn't actually recorded but
// the caller (runtime.addCovMeta) throws on id==0, so we must hand
// back a non-zero sequential value.
var wasm3CovSlot uint32

// AddMeta is the wasm3 stub. The !wasm3 implementation appends a
// CovMetaBlob to Meta.List and records the package's slot in
// Meta.PkgMap; on wasm3 we skip both. The shared package var
// reads (Meta.List, Meta.PkgMap) that drive the bridge-gap
// warnings on the !wasm3 path do not happen here.
func AddMeta(p unsafe.Pointer, dlen uint32, hash [16]byte, pkgpath string, pkgid int, cmode uint8, cgran uint8) uint32 {
	wasm3CovSlot++
	return wasm3CovSlot
}
