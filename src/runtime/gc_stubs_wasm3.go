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
// and as parameter type in chan.go. The testing/synctest experimental
// runtime support lives in synctest.go, which is excluded — bubble
// semantics depend on the heap-special / mheap subsystems.
type synctestBubble struct {
	id uint64
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
// Empty stub: any actual reach into mSpanList is in code paths that
// are unreachable on wasm3 (no Go-managed stack growth — wasm
// frames live in wasm locals).
type mSpanList struct{}

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
	heapGoal          atomic.Uint64
	bgScanCredit      atomic.Int64
	assistWorkPerByte atomicFloat64Stub
}

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
// mark worker. wasm3 has no workers — return nil.
func (c *gcControllerStub) assignWaitingGCWorker(pp *p) *g { _ = pp; return nil }

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
type scavengerStub struct{}

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
var mheap_ struct {
	cachealloc fixalloc
	spanalloc  fixalloc
	pages      pageAllocStub
	lock       mutex
}

// pageAllocStub stands in for mheap.pages (pageAlloc). proc.go's
// p-flush path calls mheap_.pages.scav etc. wasm3 doesn't manage
// pages — the few touched methods are no-ops.
type pageAllocStub struct{}

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
	count   atomic.Uint64
}

type spanqMaskStub struct{}

func (s *spanqMaskStub) any() bool          { return false }
func (s *spanqMaskStub) resize(n uintptr)   { _ = n }

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
