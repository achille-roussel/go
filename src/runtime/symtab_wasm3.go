// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import (
	"internal/abi"
	"internal/runtime/sys"
	"unsafe"
)

// wasm3 mirror of symtab.go and stkframe.go.
//
// symtab.go and stkframe.go are tagged !wasm3 because their
// pclntab-walking code (the (*_func).funcInfo / findfunc /
// funcdata bodies) does `unsafe.Pointer(uintptr(p)) +
// arithmetic` traversals that anyref cannot represent.
//
// On wasm3 the linker emits a stub pclntab (#166): an empty
// ftab and an empty pclntable. The runtime never has any
// PC → funcInfo lookup to do, so the entire pclntab-walking
// machinery degrades to "return no info". This file provides
// the surface the rest of the wasm3 runtime build references —
// findfunc, funcname, funcdata, funcline, pcdatavalue,
// funcspdelta, funcMaxSPDelta, FuncForPC, CallersFrames — as
// stubs that return the zero value.
//
// The moduledata / funcInfo / srcFunc / Frames / Frame / Func
// types are mirrored field-for-field with the upstream because
// other files read fields like firstmoduledata.{text, etext,
// data, next} directly. Per [[wasm3-runtime-rung0-inventory]]
// the linker still emits moduledata on wasm3 even though pclntab
// is a stub.

// ---------------------------------------------------------------
// Types (verbatim from symtab.go / stkframe.go where the field
// surface matters; comments trimmed for brevity)
// ---------------------------------------------------------------

type Frames struct {
	callers    []uintptr
	nextPC     uintptr
	frames     []Frame
	frameStore [2]Frame
}

type Frame struct {
	PC       uintptr
	Func     *Func
	Function string
	File     string
	Line     int
	startLine int
	Entry    uintptr
	funcInfo funcInfo
}

func CallersFrames(callers []uintptr) *Frames {
	f := &Frames{callers: callers}
	f.frames = f.frameStore[:0]
	return f
}

func (ci *Frames) Next() (frame Frame, more bool) {
	return Frame{}, false
}

type Func struct {
	opaque struct{}
}

func (f *Func) Name() string                       { return "?" }
func (f *Func) Entry() uintptr                     { return 0 }
func (f *Func) FileLine(pc uintptr) (string, int)  { return "?", 0 }
func (f *Func) startLine() int32                   { return 0 }
func (f *Func) raw() *_func                        { return nil }
func (f *Func) funcInfo() funcInfo                 { return funcInfo{} }

func FuncForPC(pc uintptr) *Func { return nil }

type pcHeader struct {
	magic          abi.PCLnTabMagic
	pad1, pad2     uint8
	minLC          uint8
	ptrSize        uint8
	nfunc          int
	nfiles         uint
	textStart      uintptr
	funcnameOffset uintptr
	cuOffset       uintptr
	filetabOffset  uintptr
	pctabOffset    uintptr
	pclnOffset     uintptr
}

type moduledata struct {
	sys.NotInHeap

	pcHeader     *pcHeader
	funcnametab  []byte
	cutab        []uint32
	filetab      []byte
	pctab        []byte
	pclntable    []byte
	ftab         []functab
	findfunctab  uintptr
	minpc, maxpc uintptr

	text, etext                uintptr
	noptrdata, enoptrdata      uintptr
	data, edata                uintptr
	bss, ebss                  uintptr
	noptrbss, enoptrbss        uintptr
	covctrs, ecovctrs          uintptr
	end, gcdata, gcbss         uintptr
	types, typedesclen, etypes uintptr
	itaboffset, itabsize       uintptr
	rodata                     uintptr
	gofunc                     uintptr
	epclntab                   uintptr

	textsectmap []textsect

	ptab []ptabEntry

	pluginpath string
	pkghashes  []modulehash

	inittasks []*initTask

	modulename   string
	modulehashes []modulehash

	hasmain uint8
	bad     bool

	gcdatamask, gcbssmask bitvector

	typemap map[*_type]*_type

	next *moduledata
}

type modulehash struct {
	modulename   string
	linktimehash string
	runtimehash  *string
}

// textAddr converts an offset from minpc to the absolute address.
// On wasm3 the linker still emits text section addresses, so this
// is a plain add. The non-wasm3 implementation also handles
// multi-section text, which wasm3 doesn't have.
func (md *moduledata) textAddr(off32 uint32) uintptr {
	return md.text + uintptr(off32)
}

// textOff is the inverse: given a PC, return the offset from
// minpc. wasm3 never calls this (findfunc returns zero), but
// keep it for symbol parity with the !wasm3 surface.
func (md *moduledata) textOff(pc uintptr) (uint32, bool) {
	if pc < md.text || pc >= md.etext {
		return 0, false
	}
	return uint32(pc - md.text), true
}

var pinnedTypemaps []map[*_type]*_type

var firstmoduledata moduledata
var lastmoduledatap *moduledata
var modulesSlice *[]*moduledata

type functab struct {
	entryoff uint32
	funcoff  uint32
}

type textsect struct {
	vaddr    uintptr
	end      uintptr
	baseaddr uintptr
}

// ptabEntry is the plugin-symbol descriptor moduledata.ptab holds.
// plugin.go is gated !wasm3 (wasm3 has no plugin support), so the
// type doesn't get redeclared from there on wasm3; we declare it
// here.
type ptabEntry struct {
	name nameOff
	typ  typeOff
}

type findfuncbucket struct {
	idx        uint32
	subbuckets [16]byte
}

// aixStaticDataBase is referenced from runtime1.go / type.go on
// all arches via `firstmoduledata.data - aixStaticDataBase` style
// math. On wasm3 (not AIX) it's always zero; the symbol exists for
// symbol parity.
var aixStaticDataBase uintptr

// activeModules / modulesinit / moduledataverify track multi-module
// programs (plugins) on the non-wasm3 platforms. wasm3 has no
// plugin support, so the active set is always just firstmoduledata.
//
// The !wasm3 path uses atomicLoadPointer / atomicStorePointer
// wrappers around `*(*unsafe.Pointer)(addr)` to read/write the
// modulesSlice package var. On wasm3 those helpers run as
// polyptr-mediated pointer derefs (anyref-classified per-value
// locals receiving i64 from the deref tail) — the documented
// #167-pending bridge gap. Since wasm3 is single-threaded the
// "atomic" qualifier is no-op anyway: read and write the
// package var directly. This drops 3 hits (activeModules +
// modulesinit + a transitive schedinit caller).
func activeModules() []*moduledata {
	if modulesSlice == nil {
		return nil
	}
	return *modulesSlice
}

func modulesinit() {
	// Allocate the slice header off-heap (new() in runtime context)
	// to match the upstream modulesinit pattern; wasm3 has no
	// dynamically loaded modules so the slice contents never grow.
	modules := new([]*moduledata)
	*modules = append(*modules, &firstmoduledata)
	modulesSlice = modules
}

func moduledataverify() {
	// No pclntab to verify on wasm3.
}

func moduledataverify1(*moduledata) {
	// No pclntab to verify on wasm3.
}

// stkobjinit is the per-module stack-object init that the regular
// runtime calls during schedinit. Wasm3 doesn't carry stack objects
// because there's no stack-walking GC scan.
func stkobjinit() {}

// ---------------------------------------------------------------
// funcInfo + accessors (all stubbed)
// ---------------------------------------------------------------

type funcInfo struct {
	*_func
	datap *moduledata
}

func (f funcInfo) valid() bool { return f._func != nil }

func (f funcInfo) _Func() *Func { return nil }

func (f funcInfo) entry() uintptr { return 0 }

func (f funcInfo) srcFunc() srcFunc { return srcFunc{} }

type srcFunc struct {
	datap     *moduledata
	nameOff   int32
	startLine int32
	funcID    abi.FuncID
}

func (s srcFunc) name() string { return "?" }

type pcvalueCache struct {
	entries [2][8]pcvalueCacheEnt
	inUse   int
}

type pcvalueCacheEnt struct {
	targetpc uintptr
	off      uint32
	val      int32
	valPC    uintptr
}

// ---------------------------------------------------------------
// pclntab-walking stubs
// ---------------------------------------------------------------

//go:nosplit
//go:linkname findfunc
func findfunc(pc uintptr) funcInfo {
	// wasm3 emits an empty ftab; no PC has function info. Returning
	// funcInfo{} makes every traceback / panic-site call degrade to
	// "no info" cleanly.
	return funcInfo{}
}

func funcname(f funcInfo) string { return "?" }

func funcpkgpath(f funcInfo) string { return "" }

func funcline(f funcInfo, targetpc uintptr) (file string, line int32) {
	return "?", 0
}

func funcline1(f funcInfo, targetpc uintptr, strict bool) (file string, line int32) {
	return "?", 0
}

func funcdata(f funcInfo, i uint8) unsafe.Pointer { return nil }

func pcdatavalue(f funcInfo, table uint32, targetpc uintptr) int32 {
	return -1
}

func pcdatavalue1(f funcInfo, table uint32, targetpc uintptr, strict bool) int32 {
	return -1
}

func pcdatavalue2(f funcInfo, table uint32, targetpc uintptr) (int32, uintptr) {
	return -1, 0
}

func funcspdelta(f funcInfo, targetpc uintptr) int32 { return 0 }

func funcMaxSPDelta(f funcInfo) int32 { return 0 }

// ---------------------------------------------------------------
// stkframe — used by traceback
// ---------------------------------------------------------------

type stkframe struct {
	fn       funcInfo
	pc       uintptr
	continpc uintptr
	lr       uintptr
	sp       uintptr
	fp       uintptr
	varp     uintptr
	argp     uintptr
}

// Keep sys references live so the compiler doesn't strip the
// import; sys is needed for the NotInHeap embedding above on some
// configurations where it would otherwise be elided.
var _ = sys.PCQuantum
