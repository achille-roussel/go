// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/runtime/sys"
	"unsafe"
)

// bitvector is the standard runtime's bit-packed pointer-map
// representation. Excluded stack.go owns the canonical version;
// reprovide here so symtab.go / stkframe.go (still compiled on
// wasm3) can refer to it without dragging in the GC-side scanning.
type bitvector struct {
	n        int32
	bytedata *uint8
}

// stackObjectRecord is the per-stack-object metadata the standard
// runtime emits for precise stack scanning. wasm3 doesn't scan
// stacks (host WasmGC owns roots), so the records are inert; the
// type must exist for the surviving stkframe.go declarations.
type stackObjectRecord struct {
	off       int32
	size      int32
	ptrBytes  int32
	gcdataoff uint32
}

// stackGuard is the per-stack guard distance used by morestack
// checks. wasm3 has no morestack — match the upstream value so the
// surviving stack.go-extern declarations agree on the constant.
const (
	stackSystem  = 0
	stackMin     = 2048
	stackNosplit = abi.StackNosplitBase * sys.StackGuardMultiplier
	stackGuard   = stackNosplit + stackSystem + abi.StackSmall
	fixedStack   = 4096

	// stackPreempt / stackForceMove / stackPoisonMin are the sentinel
	// stackguard0 values the standard runtime uses to signal preempt
	// requests, scheduled stack moves, and the lowest poison value
	// the stack debugger writes. wasm3 doesn't trigger preempt via
	// stackguard0 (the wasm engine handles stack frames), but
	// preempt.go and debug.go still test for these values.
	uintptrMask     = 1<<(8*goarch.PtrSize) - 1
	stackPreempt    = uintptrMask & -1314
	stackForceMove  = uintptrMask & -275
	stackPoisonMin  = uintptrMask & -4096
)

// maxstacksize is the per-goroutine stack-size cap the standard
// runtime enforces in newstack. wasm3 has no growing stacks; the
// value is unread but proc.go references it.
var maxstacksize uintptr = 1 << 20

// maxstackceiling is the upper bound on maxstacksize. wasm3 same as
// maxstacksize.
var maxstackceiling = maxstacksize

// startingStackSize is the initial Go-stack allocation for a new
// goroutine. wasm3 doesn't grow Go stacks; the value (8 KiB) just
// drives proc.go's `stack` width calculations.
var startingStackSize uint32 = 8 * 1024

// stackFork is the synthetic stack-marker value the standard runtime
// uses for fork-style stack tracking. Unused on wasm3.
const stackFork = uintptr(0)

// stackDebug is the per-subsystem debug-logging level constant
// stkframe.go gates verbose prints on. wasm3 disables.
const stackDebug = 0

// progToPointerMask is the standard runtime's helper for expanding
// a compact GC pointer-map program into a bitvector. wasm3 doesn't
// scan stacks but symtab.go's runtime.func name resolution still
// touches it during reflection.GCData walks; return an empty
// bitvector to short-circuit.
func progToPointerMask(prog *byte, size uintptr) bitvector {
	_ = prog
	_ = size
	return bitvector{}
}

// stack_wasm3.go provides minimal stand-ins for the Go-managed-stack
// machinery from stack.go (excluded on wasm3). The wasm3 target uses
// wasm function-call frames stored in wasm locals — there is no
// Go-side stack to allocate, grow, shrink, or scan. The runtime's
// stack* APIs are still referenced from proc.go (g0 / m0 bootstrap,
// goroutine creation, deferred-call argument scratch) so this file
// supplies the symbols those call sites need.
//
// The plan (doc/wasm3-design.md §M2): all of the stack growth machinery
// in stack.go is part of the exclusion set, with the bootstrap
// re-implemented as a degenerate "single frame, never grows" model.
// Every function here is either a no-op or, where a return value is
// required, returns the empty/zero analogue of what the standard
// runtime would compute.

// stackinit is the runtime-init hook the standard runtime uses to
// build the stackpool / stackLarge freelists. wasm3 has no freelists
// — no-op.
func stackinit() {}

// stackalloc returns a freshly-allocated stack of n bytes. The
// standard runtime allocates from a per-mcache stack cache or, for
// large stacks, from the heap. wasm3 has no Go-managed stack — wasm
// runtime owns the call frame — but proc.go calls stackalloc to
// hand back a `stack` to a newly-created g. Return a zero-sized
// stack pointing at a bump-allocated buffer so any read/write
// hits real memory and any pointer comparison against gp.stack.lo
// or .hi gives plausible answers.
func stackalloc(n uint32) stack {
	if n == 0 {
		return stack{}
	}
	p := wasm3BumpAlloc(uintptr(n))
	base := uintptr(p)
	return stack{lo: base, hi: base + uintptr(n)}
}

// stackfree releases a previously-allocated stack. wasm3's bump
// allocator is one-way — no-op.
func stackfree(stk stack) { _ = stk }

// shrinkstack is the GC-side check for whether a stack can be
// reduced. wasm3 has no growable stacks; no-op.
func shrinkstack(gp *g) { _ = gp }

// newstack is the morestack handler invoked when a function detects
// its frame won't fit. wasm3's frames are sized at compile time
// (wasm locals), so morestack is unreachable. Trap if reached.
func newstack() {
	throw("wasm3: newstack called — wasm locals shouldn't trigger morestack")
}

// morestackc is the cgo-context morestack handler. wasm3 doesn't
// support cgo; trap if reached.
func morestackc() {
	throw("wasm3: morestackc called — cgo unsupported")
}

// freeStackSpans is the GC-side hook for reclaiming entire stack
// span pages. wasm3 has no spans — no-op.
func freeStackSpans() {}

// gcComputeStartingStackSize is the GC pacer's hook for tuning the
// initial-stack-size heuristic. wasm3 has no pacer and no growing
// stacks — no-op.
func gcComputeStartingStackSize() {}

// gostartcallfn pushes a synthetic call frame onto gobuf so the
// scheduler can launch a fresh goroutine. wasm3 starts goroutines
// via a different lowering (TBD in M4 stack-switching) but proc.go's
// newproc1 still calls this. The minimal correct behavior: store the
// function pointer in gobuf.pc and leave the rest as the caller set.
// At M4 this will be replaced by a wasm-cont.new-driven shim.
func gostartcallfn(gobuf *gobuf, fv *funcval) {
	var fn unsafe.Pointer
	if fv != nil {
		fn = unsafe.Pointer(fv.fn)
	}
	gobuf.pc = uintptr(fn)
	gobuf.ctxt = unsafe.Pointer(fv)
}

// isShrinkStackSafe answers whether it is safe to shrink gp's stack
// right now. wasm3 never shrinks — return false to short-circuit
// callers.
func isShrinkStackSafe(gp *g) bool { _ = gp; return false }

// adjustpointer / adjustpointers / adjustframe / adjustctxt /
// adjustdefers / adjustpanics / adjustsudogs / syncadjustsudogs are
// stack-move pointer-fixup helpers the standard runtime uses when
// copystack relocates a goroutine's stack. wasm3 doesn't copy
// stacks — provide no-ops so any stray caller (e.g. through
// channel/select reflection) links.
func adjustpointer(adjinfo *adjustinfo, vpp unsafe.Pointer) {
	_ = adjinfo
	_ = vpp
}
func adjustpointers(scanp unsafe.Pointer, bv *bitvector, adjinfo *adjustinfo, f funcInfo) {
	_ = scanp
	_ = bv
	_ = adjinfo
	_ = f
}
func adjustframe(frame *stkframe, adjinfo *adjustinfo) {
	_ = frame
	_ = adjinfo
}
func adjustctxt(gp *g, adjinfo *adjustinfo)   { _ = gp; _ = adjinfo }
func adjustdefers(gp *g, adjinfo *adjustinfo) { _ = gp; _ = adjinfo }
func adjustpanics(gp *g, adjinfo *adjustinfo) { _ = gp; _ = adjinfo }
func adjustsudogs(gp *g, adjinfo *adjustinfo) { _ = gp; _ = adjinfo }
func syncadjustsudogs(gp *g, used uintptr, adjinfo *adjustinfo) uintptr {
	_ = gp
	_ = used
	_ = adjinfo
	return 0
}

// findsghi returns the highest stack address still referenced by an
// outstanding sudog. The standard runtime uses it to bound stack
// shrinking. wasm3 never shrinks; return 0.
func findsghi(gp *g, stk stack) uintptr { _ = gp; _ = stk; return 0 }

// copystack moves gp's stack to a freshly-allocated newsize-byte
// region. wasm3 doesn't relocate stacks; the call site is in dead
// code (newstack/morestack-driven) so this throws if reached.
func copystack(gp *g, newsize uintptr) {
	_ = gp
	_ = newsize
	throw("wasm3: copystack — wasm stacks do not relocate")
}

// adjustinfo is the metadata struct copystack threads through the
// pointer-adjustment helpers above. With the helpers reduced to
// no-ops, the struct only needs to exist for the parameter types.
type adjustinfo struct {
	old   stack
	delta uintptr
}

// fillstack writes b into every byte of stk for stack-poisoning
// debugging. wasm3 doesn't allocate Go stacks — no-op.
func fillstack(stk stack, b byte) { _ = stk; _ = b }

// nilfunc is the standard runtime's no-op function placeholder used
// by gostartcallfn for nil-function-value goroutines. wasm3 startup
// hits it through the gostartcallfn shim above; trapping behavior
// matches "ran a nil-fn goroutine".
func nilfunc() {
	throw("wasm3: nilfunc — fresh goroutine had nil func value")
}

// stacklog2 / round2 are the helper macros for stack-size class
// arithmetic. Provide the minimal pure-arithmetic versions.
func stacklog2(n uintptr) int {
	log2 := 0
	for ; n > 1; n >>= 1 {
		log2++
	}
	return log2
}
func round2(x int32) int32 {
	s := uint(0)
	for 1<<s < x {
		s++
	}
	return 1 << s
}

// stackpoolalloc / stackpoolfree / stackcacherefill /
// stackcacherelease / stackcache_clear are the per-mcache stack-
// cache helpers. wasm3 has no caches — no-ops; stackpoolalloc
// returns 0 (interpreted by callers as "out of pool, hit the heap").
func stackpoolalloc(order uint8) gclinkptr           { _ = order; return 0 }
func stackpoolfree(x gclinkptr, order uint8)         { _ = x; _ = order }
func stackcacherefill(c *mcache, order uint8)        { _ = c; _ = order }
func stackcacherelease(c *mcache, order uint8)       { _ = c; _ = order }
func stackcache_clear(c *mcache)                     { _ = c }
