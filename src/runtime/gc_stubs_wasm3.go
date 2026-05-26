// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

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

// start records a new limiter-event timestamp. wasm3 has no limiter
// — no-op. Returns false so the caller skips its limiter-window
// branch.
func (e *limiterEvent) start(typ int, now int64) bool {
	_ = typ
	_ = now
	return false
}

// stop closes a previously-started event. No-op on wasm3.
func (e *limiterEvent) stop(typ int, now int64) {
	_ = typ
	_ = now
}

// limiterEventIdle is the event-type constant for "P went idle" the
// scheduler hands to limiterEvent.start. wasm3's stub start returns
// false unconditionally so the value never matters; matches upstream
// limiterEventIdle for clarity.
const limiterEventIdle = 0

// fmtNSAsMS formats a nanosecond duration as a millisecond decimal
// for the debugger output proc.go emits during scheduler tracing.
// The standard definition lives in mgcpacer.go (excluded). A simple
// stub that integer-divides into the buffer works.
func fmtNSAsMS(buf []byte, ns uint64) []byte {
	return itoaDiv(buf, ns, 6)
}

// maxProfStackDepth is the upper bound on stack depth the profile
// samplers record. runtime1.go reads it to size a local scratch
// slice. wasm3 never samples; use the upstream value (64).
const maxProfStackDepth = 64

// mutexprofilerate is the public runtime.SetMutexProfileFraction
// setting. sema.go reads it via the if-rate gate. wasm3 never
// samples; rate stays zero.
var mutexprofilerate int64

// pageSize is the host-OS page size constant the standard runtime
// reads in many places (mheap, trace, etc). wasm3 inherits the wasm
// page size (64 KiB) since the host engine grants memory in those
// chunks.
const pageSize = 65536

// Tracer-subsystem stubs. With trace*.go, profbuf.go, cpuprof.go all
// excluded on wasm3, the per-g / per-m / per-p tracer-state fields in
// runtime2.go still need stub types. None of these are reachable on
// wasm3 (the tracer subsystem is wholly excluded) but the struct
// definitions in runtime2.go need them to typecheck.
type gTraceState struct{}
type mTraceState struct{}
type pTraceState struct{}
type traceBlockReason uint8
type traceLocker struct{}

// traceAdvance is the runtime/trace advance hook panic.go calls
// during fatal traces. wasm3 has no tracer — no-op.
func traceAdvance(stopTrace bool) { _ = stopTrace }

// traceReaderAvailable reports whether trace data is ready for the
// reader goroutine. wasm3 has no tracer — always nil.
func traceReaderAvailable() *g { return nil }

// traceReader returns the trace-consumer goroutine to schedule.
// wasm3 has no tracer — always nil.
func traceReader() *g { return nil }

// defaultTraceAdvancePeriod is the trace's per-generation interval.
// wasm3 has no tracer; constant must exist.
const defaultTraceAdvancePeriod = 0

// traceBlockReason values the runtime emits as the "why this g blocked"
// tag. wasm3 doesn't trace; consts must exist for chan.go / proc.go /
// netpoll.go / sema.go references.
const (
	traceBlockGeneric traceBlockReason = iota
	traceBlockForever
	traceBlockNet
	traceBlockSelect
	traceBlockCondWait
	traceBlockSync
	traceBlockChanSend
	traceBlockChanRecv
	traceBlockGCMarkAssist
	traceBlockGCSweep
	traceBlockSystemGoroutine
	traceBlockPreempted
	traceBlockDebugCall
	traceBlockUntilGCEnds
	traceBlockSleep
)

// traceAcquire / traceRelease are the standard runtime's lock-style
// entries for emitting a tracer event from a non-blocking critical
// section. wasm3 has no tracer; return a zero traceLocker. Callers
// guard reads with `if trace.ok()` which is always false here.
func traceAcquire() traceLocker            { return traceLocker{} }
func traceRelease(tl traceLocker)          { _ = tl }

// ok reports whether this traceLocker is live. wasm3: always false.
func (tl traceLocker) ok() bool { return false }

// All tracer event methods on traceLocker are no-ops on wasm3.
// Listed alphabetically and exhaustively to cover everything proc.go,
// coro.go, time.go, chan.go, sema.go, mfinal.go etc. reach for.
func (tl traceLocker) GoBlock(reason traceBlockReason, skip int) { _ = reason; _ = skip }
func (tl traceLocker) GoCreate(newg *g, pc uintptr, blocked bool) {
	_ = newg
	_ = pc
	_ = blocked
}
func (tl traceLocker) GoCreateSyscall(gp *g)             { _ = gp }
func (tl traceLocker) GoDestroySyscall()                 {}
func (tl traceLocker) GoEnd()                            {}
func (tl traceLocker) GoPark(reason traceBlockReason, skip int) {
	_ = reason
	_ = skip
}
func (tl traceLocker) GoPreempt()                        {}
func (tl traceLocker) GoSched()                          {}
func (tl traceLocker) GoStart()                          {}
func (tl traceLocker) GoStop(reason traceBlockReason, skip int) {
	_ = reason
	_ = skip
}
func (tl traceLocker) GoSwitch(nextg *g, destroy bool)   { _ = nextg; _ = destroy }
func (tl traceLocker) GoSwitchDestroy(nextg *g)          { _ = nextg }
func (tl traceLocker) GoSysBlock(pp *p)                  { _ = pp }
func (tl traceLocker) GoSysCall()                        {}
func (tl traceLocker) GoSysExit(lostP bool)              { _ = lostP }
func (tl traceLocker) GoUnpark(gp *g, skip int)          { _ = gp; _ = skip }
func (tl traceLocker) GCActive()                         {}
func (tl traceLocker) GCDone()                           {}
func (tl traceLocker) GCMarkAssistStart()                {}
func (tl traceLocker) GCMarkAssistDone()                 {}
func (tl traceLocker) GCStart()                          {}
func (tl traceLocker) GCSweepDone()                      {}
func (tl traceLocker) GCSweepSpan(bytesSwept uintptr)    { _ = bytesSwept }
func (tl traceLocker) GCSweepStart()                     {}
func (tl traceLocker) HeapAlloc(live uint64)             { _ = live }
func (tl traceLocker) HeapGoal()                         {}
func (tl traceLocker) OneNewExtraM(gp *g)                { _ = gp }
func (tl traceLocker) ProcStart()                        {}
func (tl traceLocker) ProcSteal(pp *p, inSyscall ...bool) {
	_ = pp
	_ = inSyscall
}
func (tl traceLocker) ProcStop(pp *p)                    { _ = pp }
func (tl traceLocker) STWStart(reason stwReason)         { _ = reason }
func (tl traceLocker) STWDone()                          {}
func (tl traceLocker) GoroutineLeak(gp *g)               { _ = gp }
func (tl traceLocker) ProcsChange()                      {}
func (tl traceLocker) Gomaxprocs(n int32)                { _ = n }

// traceEnabled reports whether tracing is on. wasm3: always false.
func traceEnabled() bool { return false }

// traceLockInit / traceShuttingDown / traceThreadDestroy / cpuprof
// are the few non-method tracer entry points proc.go still touches.
// All no-ops on wasm3.
func traceLockInit()                  {}
func traceShuttingDown() bool         { return true }
func traceThreadDestroy(mp *m)        { _ = mp }

// cpuprof is the runtime's CPU-profiler state. cpuprof.go is excluded;
// proc.go's sysmon calls cpuprof.add. Empty stub with no-op fields.
var cpuprof cpuProfileStub

type cpuProfileStub struct {
	lock        mutex
	lostAtomic  uint64
}

func (c *cpuProfileStub) add(tagPtr *unsafe.Pointer, stk []uintptr) {
	_ = tagPtr
	_ = stk
}

// gTraceState's reset clears the per-g tracer state during goroutine
// reset / startup. wasm3 doesn't trace — no-op.
func (s *gTraceState) reset() { _ = s }

// traceExitingSyscall / traceExitedSyscall are the syscall-bracket
// tracer entries proc.go calls. wasm3 doesn't trace — no-op.
func traceExitingSyscall() {}
func traceExitedSyscall()  {}

// traceCPUSample is the CPU profiler's per-sample tracer hook proc.go
// calls. wasm3 doesn't trace — no-op.
func traceCPUSample(gp *g, mp *m, pp *p, stk []uintptr) {
	_ = gp
	_ = mp
	_ = pp
	_ = stk
}

// maxCPUProfStack is the max stack-depth the CPU profiler records.
// cpuprof.go is excluded; proc.go uses the constant for a buffer
// size literal.
const maxCPUProfStack = 64
// (stwReason is defined in proc.go; no stub needed here.)

// gcWork is referenced as struct field type in p (runtime2.go) and as
// parameter type in mcheckmark.go and preempt_noxreg.go. The mark
// queue itself lives in mgcwork.go, which is excluded.
type gcWork struct {
	id    int32
	spanq gcSpanQueueStub
}

// gcSpanQueueStub is the per-P mark-span queue. proc.go calls
// pp.gcw.spanq.empty() etc. wasm3 has no spans — always empty.
type gcSpanQueueStub struct{}

func (q *gcSpanQueueStub) empty() bool { return true }
func (q *gcSpanQueueStub) destroy()    {}

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

// prepareForSweep is the per-P sweep-prep hook proc.go's GC-cycle
// boundary calls. wasm3 has no sweeper — no-op.
func (c *mcache) prepareForSweep() { _ = c }

// mspan is referenced as struct field type in mlink (runtime2.go) and
// as parameter type in arena.go (the latter being itself excluded in
// a later island). The span metadata struct lives in mheap.go, which
// is excluded.
type mspan struct {
	elemsize uintptr
}

// typePointersOfUnchecked is the per-element pointer-bitmap iterator
// the cgo pointer-scan reaches for. wasm3 has no heap to scan; the
// returned typePointers' next() never has anything to yield. Empty
// stub satisfies the call site.
func (s *mspan) typePointersOfUnchecked(addr uintptr) typePointers {
	_ = s
	_ = addr
	return typePointers{}
}

// typePointers is the iterator yield-state for typePointersOfUnchecked.
// The cgo scan calls next() on it in a loop; with the wasm3 stub
// returning zero, the loop exits immediately.
type typePointers struct{}

// next reports the next pointer slot within the span. The stub
// returns 0 to signal "no more pointers" so cgocall.go's scan loop
// terminates immediately on wasm3.
func (tp typePointers) next(limit uintptr) (typePointers, uintptr) {
	_ = limit
	return tp, 0
}

// wbBuf is referenced as struct field type in p (runtime2.go). The
// write-barrier buffer lives in mwbbuf.go, which is excluded.
type wbBuf struct{}

// get2 returns a pointer to a two-slot region in the write-barrier
// buffer. atomic_pointer.go calls it before storing a pointer pair
// for later draining. wasm3 has no WB so we return a static array;
// the bytes are never consumed because writeBarrier.enabled stays
// false on wasm3 and the call site is dead.
func (b *wbBuf) get2() *[2]uintptr {
	_ = b
	return &wasm3WBDummy
}

// reset clears the write-barrier buffer. wasm3 has no WB — no-op.
func (b *wbBuf) reset() { _ = b }

var wasm3WBDummy [2]uintptr

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
// and as parameter type in chan.go / time.go. The testing/synctest
// experimental runtime support lives in synctest.go, which is
// excluded. .now / .timers are fields (not methods) so time.go's
// `bubble.now / 1e9` and `&bubble.timers` typecheck.
type synctestBubble struct {
	id     uint64
	now    int64
	timers timers
}

// incActive / decActive are coro.go's per-bubble active-coroutine
// counters. wasm3 has no coroutine scheduler that honours bubbles;
// the calls are no-op.
func (b *synctestBubble) incActive() { _ = b }
func (b *synctestBubble) decActive() { _ = b }

// changegstatus is proc.go's bubble hook called on goroutine status
// transitions. wasm3 has no synctest scheduler — no-op.
func (b *synctestBubble) changegstatus(gp *g, oldval, newval uint32) {
	_ = b
	_ = gp
	_ = oldval
	_ = newval
}

// raceaddr returns a synthetic address representing the bubble for
// race-detector synchronisation. wasm3 doesn't run the race detector;
// return nil.
func (b *synctestBubble) raceaddr() unsafe.Pointer { _ = b; return nil }


// mSpanList is referenced from stack.go's pool of free stack spans.
// stack.go's growth machinery is part of the M2 exclusion set per
// doc/wasm3-design.md, but the surviving stack.go bits (signature
// touches in proc.go etc.) still mention mSpanList in declarations.
// .first / .last are fields not methods so stack.go's `s := list.
// first` typechecks as `*mspan`, not a method value.
type mSpanList struct {
	first *mspan
	last  *mspan
}

func (l *mSpanList) init()                  {}
func (l *mSpanList) insert(span *mspan)     { _ = span }
func (l *mSpanList) remove(span *mspan)     { _ = span }
func (l *mSpanList) isEmpty() bool          { return true }
func (l *mSpanList) insertBack(span *mspan) { _ = span }

// gcMarkWorkerMode is the worker-role enum that the GC scheduler
// hands to background mark workers. wasm3 has no concurrent GC so the
// mode is meaningless; runtime2.go still has a per-p field of this
// type. Empty stub: never read on wasm3.
type gcMarkWorkerMode int

// gcBgMarkWorkerNode is the background-mark-worker queue link.
// wasm3 has no background mark workers; runtime2.go has a *node
// pointer that stays nil. Empty stub with the gp field proc.go
// uses to reach back to the worker goroutine.
type gcBgMarkWorkerNode struct {
	gp guintptr
}

// persistentAlloc is the per-P off-heap allocator's local cache.
// runtime2.go has a per-p field of this type. wasm3 routes all
// persistent allocations through the bump-heap shim so this cache
// is never populated. Empty stub.
type persistentAlloc struct{}

// notInHeap is the marker type that the standard allocator uses to
// distinguish off-heap (mmap'd) memory from GC-managed memory.
// slice.go's notInHeapSlice header references it. wasm3 has no
// notion of "not in heap" — everything is host-WasmGC-managed —
// but the type must exist for the slice header to type-check.
type notInHeap struct{}

// mallocHeaderSize is the size of the malloc header that the standard
// allocator prepends to every allocation. runtime2.go uses it in a
// compile-time-asserted padding calculation for type m. wasm3 has no
// such header; 0 makes the padding consistent with the no-header case.
const mallocHeaderSize = 0

// gcMarkWorkerModeStrings is the per-mode display label for the GC
// background mark workers. trace.go uses len(gcMarkWorkerModeStrings)
// to size a per-marker-mode label array. wasm3 has no mark workers
// so the array is unused; size-3 matches the upstream constant
// (gcMarkWorkerDedicatedMode/FractionalMode/IdleMode).
var gcMarkWorkerModeStrings = [3]string{}

// writeBarrier is the global write-barrier control struct (read as
// `if writeBarrier.enabled` in atomic_pointer.go and elsewhere).
// wasm3 has no Go-side write barrier — the host WasmGC handles all
// pointer tracking — so enabled stays false unconditionally and the
// barrier branches are dead.
var writeBarrier struct {
	enabled bool
}

// gclinkptr is the linked-list pointer type for the stack-cache free
// list (stack.go). The cache is part of the stack-growth machinery
// which wasm3 does not use (no Go-managed stack); the surviving
// declarations need the type to exist for the package to compile.
type gclinkptr uintptr

// _NumStackOrders is the number of stack size classes the stack-cache
// supports. stack.go sizes an [_NumStackOrders]mSpanList table off it.
// wasm3 has no Go-managed stack so the table is never populated;
// match the upstream wasm value (4) to keep the table size consistent.
const _NumStackOrders = 4

// heapAddrBits is the number of address bits used by the heap. Used
// in stack.go's lookup-table sizing. wasm3 has no heap of its own —
// it inherits whatever the wasm engine grants — so this is the wasm-
// upstream value (48, as the wasm spec allows up to 48-bit addresses
// in the 64-bit memory proposal).
const heapAddrBits = 48

// pageCache is the per-P page cache. runtime2.go has a per-p field
// of this type. wasm3 has no page allocator (host WasmGC owns the
// address space) so the cache is never populated. Empty stub.
type pageCache struct{}

// flush drops cached pages back to the page allocator. wasm3 has no
// pages — no-op.
func (c *pageCache) flush(p *pageAllocStub) { _ = c; _ = p }

// freemcache releases a per-P size-class cache back to mheap.
// proc.go's p-tear-down path calls it. wasm3 has no caches — no-op.
func freemcache(c *mcache) { _ = c }

// inheap reports whether p points into a heap-managed allocation.
// cgocall.go's runtime.cgoCheckPointer guards check this before
// scanning. cgo is unsupported on wasm3 so this branch is dead;
// return false to short-circuit the scan.
func inheap(p uintptr) bool { _ = p; return false }

// isPinned reports whether the object containing ptr has been pinned
// via runtime.Pinner. On wasm3 nothing is pinned (host WasmGC does
// not move objects, so the concept does not apply) — always false.
func isPinned(ptr unsafe.Pointer) bool { _ = ptr; return false }

// findObject finds the heap-managed object containing p, if any, and
// returns its base address, span, and object index. cgocall.go uses
// this for cgo's runtime pointer-tracing scan. wasm3 has no Go-side
// heap to walk so it returns zero — the caller treats that as "not
// found" and skips the scan.
func findObject(p, refBase, refOff uintptr) (base uintptr, s *mspan, objIndex uintptr) {
	_ = p
	_ = refBase
	_ = refOff
	return 0, nil, 0
}

// inHeapOrStack reports whether b lies inside a heap-managed allocation
// or a Go-managed stack. cgocall.go uses it for cgo pointer checks.
// wasm3 has neither — return false.
func inHeapOrStack(b uintptr) bool { _ = b; return false }

// inPersistentAlloc reports whether p was allocated from the runtime's
// off-heap persistent arena. cgocheck.go uses it to skip the pointer
// scan for off-heap memory. wasm3's persistentalloc shim has no such
// arena, so return false.
func inPersistentAlloc(p uintptr) bool { _ = p; return false }

// addb is the standard heap-bitmap pointer-arithmetic helper.
// cgocheck.go uses it to walk the per-object pointer bitmap. wasm3
// has no heap bitmap (host WasmGC owns pointer tracing) so the
// callers are in dead code; the stub satisfies the call signature.
func addb(p *byte, n uintptr) *byte {
	return (*byte)(unsafe.Add(unsafe.Pointer(p), n))
}

// maxAlloc is the largest possible allocation size on the heap. wasm3
// gets it from heapAddrBits the same way the upstream malloc.go does.
const maxAlloc = (1 << heapAddrBits) - 1

// typeBitsBulkBarrier is the typed bulk write-barrier helper used by
// chan.go's send/receive copy paths. wasm3 has no write barrier
// (host WasmGC handles all references), so the call is a no-op.
func typeBitsBulkBarrier(typ *_type, dst, src, size uintptr) {
	_ = typ
	_ = dst
	_ = src
	_ = size
}

// minLegalPointer is the smallest valid heap-pointer value the
// checkptr machinery considers a non-bogus pointer. checkptr.go uses
// it to short-circuit obvious garbage. wasm3 has no heap of its own;
// matches the upstream constant from malloc.go's pre-exclusion value.
const minLegalPointer = 4096

// bulkBarrierPreWrite is the pre-write barrier the standard runtime
// emits before bulk memory writes (typedmemmove, wbZero, wbMove etc.
// in mbarrier.go). wasm3 has no write barrier (host WasmGC handles
// every reference store) so this is unconditionally a no-op.
func bulkBarrierPreWrite(dst, src, size uintptr, typ *_type) {
	_ = dst
	_ = src
	_ = size
	_ = typ
}

// bulkBarrierPreWriteSrcOnly is the source-only variant the slice
// code uses when the destination is freshly allocated and needs no
// pre-image barrier. No-op on wasm3.
func bulkBarrierPreWriteSrcOnly(dst, src, size uintptr, typ *_type) {
	_ = dst
	_ = src
	_ = size
	_ = typ
}

// roundupsize is the size-class round-up the standard allocator uses
// to convert a requested byte count into the next-largest span class.
// wasm3 has no span classes — return the requested size unchanged
// (modulo 8-byte alignment) so callers behave consistently.
func roundupsize(size uintptr, noscan bool) uintptr {
	_ = noscan
	return (size + 7) &^ 7
}

// freegc releases a typed allocation back to the heap. wasm3 has no
// freelist — host WasmGC reclaims unreferenced objects. Return false
// so the caller's "freed-via-runtime" branch is skipped.
func freegc(ptr unsafe.Pointer, size uintptr, noscan bool) bool {
	_ = ptr
	_ = size
	_ = noscan
	return false
}

// mutexevent is the standard runtime's mutex-contention sampling
// entry. wasm3 never samples — no-op.
func mutexevent(cycles int64, skip int) {
	_ = cycles
	_ = skip
}

// persistentalloc is the standard runtime's off-heap allocator, used
// for itab allocation, the lock-rank dependency graph, etc. wasm3
// routes everything through the bump-heap shim — there is no separate
// persistent arena, and any allocation that previously came from one
// just lives in the same wasm3Heap. The sysStat parameter is ignored
// (no per-sysmem-stat accounting on wasm3).
func persistentalloc(size, align uintptr, sysStat *sysMemStat) unsafe.Pointer {
	_ = align
	_ = sysStat
	return wasm3BumpAlloc(size)
}

// gcController is the GC pacer's global controller state. mem.go's
// sysAlloc/sysFree paths call gcController.mappedReady.Add to track
// virtual-memory pressure. wasm3 has no pacer (no concurrent GC) but
// mem.go still compiles, so expose just the mappedReady counter.
// The Add calls are harmless no-state-side-effects (the value is
// never read by surviving code).
// gcControllerStub is a stand-in for mgcpacer's gcControllerState that
// exposes only the fields surviving callers read.
type gcControllerStub struct {
	mappedReady       atomic.Uint64
	memoryLimit       atomic.Int64
	gcPercent         atomic.Int32
	heapMarked        uint64
	bgScanCredit      atomic.Int64
	assistWorkPerByte atomicFloat64Stub
}

// heapGoal is the target heap size for the next GC cycle. mstats.go
// and traceruntime.go call it as a function. wasm3 has no GC; return
// math.MaxUint64 so the heuristic "we're under the goal" is always
// true.
func (c *gcControllerStub) heapGoal() uint64 { return ^uint64(0) }

// atomicFloat64Stub is a stand-in for atomic.Float64 used by the GC
// pacer for sub-cycle work bookkeeping. wasm3 never updates it; the
// Load method returns 0 so callers see "no per-byte assist needed".
type atomicFloat64Stub struct{}

func (a *atomicFloat64Stub) Load() float64 { return 0 }

// findRunnableGCWorker is the scheduler's hand-off path for getting
// a P into background-mark mode. wasm3 has no mark workers — return
// nil + 0 so the caller falls through.
func (c *gcControllerStub) findRunnableGCWorker(pp *p, now int64) (*g, int64) {
	_ = pp
	_ = now
	return nil, now
}

// assignWaitingGCWorker is the scheduler hook for grabbing a queued
// mark worker. wasm3 has no workers — return false and pass `now`
// through unchanged so the caller's for-loop exits.
func (c *gcControllerStub) assignWaitingGCWorker(pp *p, now int64) (bool, int64) {
	_ = pp
	return false, now
}

// addIdleMarkWorker / removeIdleMarkWorker manage the count of
// idle-mode mark workers the scheduler may run. wasm3 has no mark
// workers — both are no-ops; addIdleMarkWorker returns false so
// the scheduler skips the idle-worker branch.
func (c *gcControllerStub) addIdleMarkWorker() bool             { return false }
func (c *gcControllerStub) removeIdleMarkWorker()               {}
func (c *gcControllerStub) needIdleMarkWorker() bool            { return false }
func (c *gcControllerStub) releaseNextGCMarkWorker(pp *p) *g    { _ = pp; return nil }
func (c *gcControllerStub) addScannableStack(pp *p, n int64) {
	_ = pp
	_ = n
}

// addGlobals adds a globals-region byte count to the GC's scan
// budget. wasm3 has no GC — no-op.
func (c *gcControllerStub) addGlobals(amount int64) { _ = amount }

// gcMarkWorkerNotWorker is the gcMarkWorkerMode constant signalling
// "this g isn't a mark worker". wasm3 has no mark workers; constant
// must exist for traceruntime.go to compile.
const gcMarkWorkerNotWorker = 0

// fingRunningFinalizer is the special stackguard0 / status value the
// runtime sets while a finalizer is executing. wasm3 has no
// finalizers; constant must exist for traceback.go's check.
const fingRunningFinalizer = 0

// traceSnapshotMemory records the heap layout into the trace. wasm3
// has no managed heap to snapshot — no-op.
func traceSnapshotMemory(gen uintptr) { _ = gen }

// traceAllocFreeTypesBatch is the per-type-batch tag byte the
// runtime tracer prefixes when emitting alloc/free type records.
// wasm3's tracer never emits these; constant must exist.
const traceAllocFreeTypesBatch = 0


// gcMarkWorkerIdleMode is the gcMarkWorkerMode constant for idle-time
// mark work. wasm3 never uses it but proc.go references it.
const gcMarkWorkerIdleMode = 2

var gcController gcControllerStub

// cleanupBlock is a freelist node for runtime.AddCleanup queued work.
// runtime2.go's p has a *cleanupBlock field. Cleanups are unsupported
// on wasm3 (per doc/wasm3-design.md §12.4 — drop / no-op for M2–M5)
// so the field stays nil. Empty stub.
type cleanupBlock struct{}

// physPageSize is the host OS's physical page size. malloc.go's
// mallocinit normally probes this from the platform; mem.go,
// proc.go, stack.go etc. read it. wasm3 has no OS page concept
// (the host engine manages the memory backing) — pick the wasm
// page-size value to match the rest of the wasm toolchain.
var physPageSize uintptr = 65536

// gcCPULimiter is the GC CPU-time-budget enforcement state. metrics.go
// reads gcCPULimiter.lastEnabledCycle to expose a Prometheus-style
// counter. wasm3 has no concurrent GC so no limiter; the counter
// stays zero. Just need a struct with that field.
type gcCPULimiterStub struct {
	lastEnabledCycle atomic.Uint64
}

// resetCapacity is the GOMAXPROCS-change hook. wasm3 has no limiter
// — no-op.
func (l *gcCPULimiterStub) resetCapacity(now int64, capacity int32) {
	_ = now
	_ = capacity
}

var gcCPULimiter gcCPULimiterStub

// scavenger is the page-scavenger goroutine state machine. proc.go's
// sysmon wakes it. wasm3 has no pages to scavenge — stub.
type scavengerStub struct {
	sysmonWake atomic.Uint32
}

func (s *scavengerStub) wake()  {}
func (s *scavengerStub) ready() {}

var scavenger scavengerStub

// timeHistogram is the bucketed time-distribution counter mfinal.go /
// mgcsweep.go etc. update. runtime2.go's schedstat aggregation has
// timeHistogram-typed fields. wasm3 has no histograms to update.
type timeHistogram struct{}

// record adds a duration to the histogram. wasm3 doesn't record —
// no-op.
func (h *timeHistogram) record(duration int64) { _ = duration }

// KeepAlive is the public runtime.KeepAlive that defeats dead-store
// elimination on the argument. The standard definition lives in
// mfinal.go which is excluded on wasm3; reprovide it here so calling
// code (cgocall.go, chan.go) still links. The empty body suffices —
// the compiler treats KeepAlive specially as an op that keeps its
// arg live, regardless of body content.
func KeepAlive(x any) { _ = x }

// setprofilebucket is the heap-profile bucket setter the standard
// runtime calls when sampling. wasm3 never samples — no-op.
func setprofilebucket(p unsafe.Pointer, b *bucket) {
	_ = p
	_ = b
}

// bucket is mprof.go's heap-profile sample bucket. With mprof.go
// excluded the type still needs to exist for the runtime2.go and
// chan.go references — empty stub, never read.
type bucket struct{}

// sysMemStat is the atomic per-memory-region counter mem.go and
// persistentalloc thread through their sysAlloc/sysFree paths.
// Standard definition lives in mstats.go (excluded). wasm3 has no
// per-region memory accounting; this stub keeps the param types
// consistent. Atomic Add via the embedded counter; nothing reads it.
type sysMemStat atomic.Uint64

// add records a memory delta on this stat. wasm3 doesn't read these
// counters but the standard runtime expects an Add method for sysAlloc
// and persistentalloc to update them.
func (s *sysMemStat) add(n int64) { (*atomic.Uint64)(s).Add(n) }

// load returns the current counter value. Unused on wasm3 but kept
// for API parity.
func (s *sysMemStat) load() uint64 { return (*atomic.Uint64)(s).Load() }

// goroutineProfileStateHolder is the per-g state for runtime.
// GoroutineProfile sampling. The standard definition lives in
// mprof.go (excluded). runtime2.go has a g field of this type;
// wasm3 never samples goroutines. Stub.
type goroutineProfileStateHolder atomic.Uint32

// Store records a profile-state transition. wasm3 never samples;
// the underlying Uint32 is unused but the method must exist for
// proc.go to compile.
func (h *goroutineProfileStateHolder) Store(v uint32) {
	(*atomic.Uint32)(h).Store(v)
}

// Load returns the recorded profile state.
func (h *goroutineProfileStateHolder) Load() uint32 {
	return (*atomic.Uint32)(h).Load()
}

// mLockProfile is the per-m mutex-contention profile state. With
// mprof.go excluded runtime2.go's m needs a stub type for the field.
// wasm3 never samples contention.
type mLockProfile struct {
	waitTime atomic.Int64
	stack    []uintptr
}

// blockprofilerate is the public runtime.SetBlockProfileRate setting
// — sampling rate for blocking operations (chan, select, sync).
// chan.go reads it via the if-rate gate. wasm3 never samples; rate
// stays zero to short-circuit the gate.
var blockprofilerate int64

// blockevent is the standard runtime's block-profile sampling entry
// point invoked by the chan / select / sync paths after a blocking
// op. wasm3 never samples — no-op.
func blockevent(cycles int64, skip int) {
	_ = cycles
	_ = skip
}

// goroutineProfile / tryRecordGoroutineProfile are coro.go's hooks
// into the goroutine sampling subsystem. wasm3 never samples — no-op
// stubs.
var goroutineProfile struct {
	active bool
}

func tryRecordGoroutineProfile(gp *g, pcbuf []uintptr, yield func()) {
	_ = gp
	_ = pcbuf
	_ = yield
}

// tryRecordGoroutineProfileWB is the write-barrier-safe variant the
// scheduler reaches when racing GC. wasm3 has no profiler — no-op.
func tryRecordGoroutineProfileWB(gp *g) { _ = gp }

// goroutineProfileSatisfied is the sentinel value stored into a new
// goroutine's profile-state to mark it as "no sampling needed". wasm3
// never samples; treat every fresh g as already covered.
const goroutineProfileSatisfied uint32 = 0

// memstats is the global runtime-stats aggregator that iface.go,
// netpoll.go, and many other files Add into. The standard struct
// lives in mstats.go (excluded). Empty stub with the few fields
// that surviving call sites read or update.
var memstats struct {
	other_sys sysMemStat
	heapStats consistentHeapStats
}

// consistentHeapStats is the per-P-sharded heap-stats accumulator
// proc.go's stats merge reaches. wasm3 doesn't accumulate — stub.
type consistentHeapStats struct {
	noPLock mutex
}

// zerobase is the standard runtime's address of the zero-byte
// allocation. allocations of size 0 return &zerobase. newobject_wasm3.go
// uses it for the size==0 fast path; the standard definition is in
// malloc.go (excluded). One byte of static storage works as well as
// any nonzero address.
var zerobase uintptr

// synctestDeadlockError is the panic value emitted when synctest
// detects a deadlock inside a bubble. panic.go's deadlock detector
// references it. With synctest.go excluded the type still needs to
// exist for the type-assertion not to be a compile error; the value
// is unreachable on wasm3 (no synctest scheduler hooks).
type synctestDeadlockError struct {
	bubble *synctestBubble
}

func (e *synctestDeadlockError) Error() string { return "synctest deadlock" }

// mheap_ is the global mheap singleton. Most readers are inside
// excluded files; surviving touchpoints (panic.go, proc.go) reach
// for a handful of fields/locks. Empty mutex / fixalloc stubs are
// enough for the surviving paths (which are dead-code at runtime on
// wasm3 — proc.go's p-flush only fires when there's actual heap
// state).
type mheapStub struct {
	cachealloc fixalloc
	spanalloc  fixalloc
	pages      pageAllocStub
	lock       mutex
}

var mheap_ mheapStub

// pageAllocStub stands in for mheap.pages (pageAlloc). proc.go's
// p-flush path calls mheap_.pages.scav etc. wasm3 doesn't manage
// pages — the few touched methods are no-ops.
type pageAllocStub struct {
	inUse addrRangesStub
}

// addrRangesStub mirrors the standard pageAlloc.inUse — an
// address-range list. wasm3 never tracks pages.
type addrRangesStub struct {
	ranges []addrRangeStub
}

type addrRangeStub struct {
	base, limit offAddrStub
}

// offAddrStub mirrors the standard pageAlloc's offAddr — a uintptr
// wrapper with an addr() method that strips the per-space offset.
// wasm3 doesn't track address spaces; addr() returns the underlying
// uintptr unchanged.
type offAddrStub uintptr

func (a offAddrStub) addr() uintptr { return uintptr(a) }

// _StackCacheSize is the per-mcache stack-cache byte capacity. stack.go
// uses it for cache-slot indexing. wasm3 has no Go-managed stacks so
// the indexing is dead; match upstream value (32 KiB) for consistency.
const _StackCacheSize = 32 * 1024

// pageMask is the bit-mask for page-aligned addresses. wasm3 has no
// pages of its own; the wasm spec uses 64KiB pages. _PageSize - 1.
const pageMask = 65535

// allocManual is mheap.allocManual — manual non-GC span allocation
// for stack frames etc. wasm3 doesn't manage spans — return nil so
// the caller's "needs to allocate" branch hits a nil-deref guard.
func (h *mheapStub) allocManual(npages uintptr, typ int) *mspan {
	_ = npages
	_ = typ
	return nil
}

// spanAllocStack is the "stack" tag for allocManual. wasm3 doesn't
// allocate stack spans — value is unread.
const spanAllocStack = 0

// fixalloc is mheap's per-class allocator. panic.go has a code path
// that frees an mcache through mheap_.cachealloc.free; with mheap.go
// excluded the type is gone, but a stub with a no-op free keeps the
// call site compilable.
type fixalloc struct {
	size uintptr
}

func (f *fixalloc) free(p unsafe.Pointer) { _ = p }

// gcenable is the GC startup hook proc.go calls during boot. wasm3
// has no concurrent GC — no-op.
func gcenable() {}

// gcphase is the global GC-state machine word. _GCoff (0) means
// "not in GC". wasm3 stays in this state forever.
var gcphase uint32

const (
	_GCoff             uint32 = 0
	_GCmark            uint32 = 1
	_GCmarktermination uint32 = 2
)

// work is mgc.go's global GC bookkeeping aggregate. Surviving proc.go
// references just check work.full / etc. wasm3 keeps it all-zero.
var work struct {
	full          atomic.Uint64
	startSema     uint32
	markDoneSema  uint32
	goroutineLeak goroutineLeakStub
	spanqMask     spanqMaskStub
}

type goroutineLeakStub struct {
	enabled atomic.Bool
	count   int
}

type spanqMaskStub struct{}

func (s spanqMaskStub) any() bool                  { return false }
func (s spanqMaskStub) resize(n int32) spanqMaskStub { _ = n; return s }

// allocmcache returns a fresh per-P size-class cache. mcache.go owns
// the real implementation. wasm3 has no caches (no mallocgc) so
// return nil — proc.go's only use is to assign to p.mcache, and
// nothing on wasm3 reads back.
func allocmcache() *mcache { return nil }

// gcStart starts a new GC cycle. proc.go's sysmon background trigger
// invokes it. wasm3 has no concurrent GC — no-op.
func gcStart(trigger gcTrigger) { _ = trigger }

// gcTrigger / gcTriggerTime are the GC pacer's trigger-reason enum
// and time-trigger constant. proc.go constructs gcTrigger{kind:
// gcTriggerTime, now: ...} when forcing GC.
type gcTrigger struct {
	kind int
	now  int64
	n    uint32
}

// test reports whether this trigger has been satisfied (should fire
// a GC cycle). wasm3 has no GC — always false so the sysmon never
// schedules one.
func (t gcTrigger) test() bool { return false }

const gcTriggerTime = 0

// finlock is the global finalizer-queue lock. proc.go acquires it
// during shutdown / drain. wasm3 has no finalizers; using a stub
// mutex keeps the lock/unlock pairs compilable.
var finlock mutex

// mallocinit is the allocator's bootstrap entry called from proc.go
// during runtime startup. wasm3's allocation is intrinsified at the
// SSA layer (struct.new) and falls back to the wasm3Heap bump arena;
// no initialization needed.
func mallocinit() {}

// gcinit is the GC bootstrap. wasm3 has no GC — no-op.
func gcinit() {}

// gcBlackenEnabled is the global flag set by mgc.go to indicate the
// concurrent mark phase is running. wasm3 has no concurrent GC; the
// flag stays zero and the readers (proc.go scheduling decisions)
// take the GC-disabled branch.
var gcBlackenEnabled uint32

// gcShouldScheduleWorker reports whether the scheduler should hand
// the next P to a background mark worker. wasm3 has no mark workers
// — always false.
func gcShouldScheduleWorker(pp *p) bool { _ = pp; return false }

// fingStatus is the finalizer-goroutine state word. proc.go's
// scheduler tickles it during runqueue draining. wasm3 has no
// finalizers — value never changes.
var fingStatus atomic.Uint32

// fingWait / fingWake are the finalizer-goroutine status flag bits
// proc.go tests when deciding whether to wake the finalizer
// goroutine. wasm3's stubs are zero — the flags never fire.
const (
	fingWait = 1 << 0
	fingWake = 1 << 1
)

// wakefing returns the finalizer goroutine to schedule (or nil if
// none). wasm3 has no finalizer queue — always nil.
func wakefing() *g { return nil }

// blockUntilEmptyFinalizerQueue blocks until pending finalizers run.
// wasm3 has none — return immediately true to satisfy callers
// waiting on a drain.
func blockUntilEmptyFinalizerQueue(timeout int64) bool { _ = timeout; return true }

// gcCleanups is the per-runtime cleanup queue maintained by mcleanup.go.
// proc.go's sysmon checks it for pending cleanups. wasm3 has none.
type gcCleanupsStub struct {
	asleep atomic.Bool
	full   atomic.Bool
	queued uint64
}

func (c *gcCleanupsStub) needsWake() bool { return false }
func (c *gcCleanupsStub) wake()           {}

var gcCleanups gcCleanupsStub

// disableMemoryProfiling is a bool variable set by the linker via
// runtime.disableMemoryProfiling; proc.go consults it to suppress the
// heap profiler. wasm3 never samples — stays false.
var disableMemoryProfiling bool

// MemProfileRate is the public sampling-rate knob. wasm3 stays at
// the upstream default (528 KiB) so user code that reads it sees a
// plausible value; the actual sampling path is excluded.
var MemProfileRate int = 512 * 1024

// maxSkip is the lock-profile stack-skip cap from mprof.go. wasm3
// never samples — value is irrelevant but the constant must exist.
const maxSkip = 0

// itoaDiv writes val as a decimal into buf with dec fractional digits.
// debuglog.go uses it for floating-point-free dec emission. The
// excluded mgc.go defines the canonical version; this is a verbatim
// copy hoisted into the wasm3 stub file so the debug-log printer
// still links.
func itoaDiv(buf []byte, val uint64, dec int) []byte {
	i := len(buf) - 1
	idec := i - dec
	for val >= 10 || i >= idec {
		buf[i] = byte(val%10 + '0')
		i--
		if i == idec {
			buf[i] = '.'
			i--
		}
		val /= 10
	}
	buf[i] = byte(val + '0')
	return buf[i:]
}
