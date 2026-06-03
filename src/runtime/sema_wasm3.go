// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// wasm3 mirror of sema.go.
//
// sema.go is tagged !wasm3 because its address-keyed treap orders
// waiter lists by `uintptr(unsafe.Pointer(addr))`. anyref has no
// scalar form, so the treap ordering is impossible. The
// `maybeTraceablePtr.set/uintptr` write-barrier-bypass methods in
// runtime2.go that sema.go calls via `s.elem.set(...)` /
// `t.elem.uintptr()` also trip the wasm3 SSA-gen fatal.
//
// This file provides the same exported surface (semacquire /
// semrelease / sync_runtime_Sem* / internal_sync_runtime_* /
// notifyList*) backed by a simpler implementation that exploits
// wasm3 being single-threaded with cooperative scheduling:
//
//   - No atomicity for the address counter — plain *addr++ /
//     *addr-- (host has no other threads).
//   - No address-keyed treap. The per-address waiter list is
//     looked up by pointer equality through a global linked list
//     of (addr, head) entries. Wasm3 programs rarely have many
//     distinct synchronization addresses live at once, so the
//     linear scan is cheap in practice; a map can be added later
//     if measurements warrant it.
//   - No mutex — gopark / goready handle the wake without lock
//     contention, since the only "contention" is intra-goroutine.

// ---------------------------------------------------------------
// semaphore profile flags + readyWithTime (verbatim from sema.go)
// ---------------------------------------------------------------

type semaProfileFlags int

const (
	semaBlockProfile semaProfileFlags = 1 << iota
	semaMutexProfile
)

// readyWithTime wakes s.g. If s.releasetime > 0, sets it to the
// current time (so the unparker can attribute the wakeup time).
func readyWithTime(s *sudog, traceskip int) {
	if s.releasetime != 0 {
		s.releasetime = cputicks()
	}
	goready(s.g, traceskip)
}

// ---------------------------------------------------------------
// semaphore
// ---------------------------------------------------------------

// semaWaiterList is the head of a singly-linked list of
// (addr, sudog-list-head) entries.
var semaWaiterList *semaWaiters

type semaWaiters struct {
	addr *uint32
	head *sudog // first parked sudog (FIFO unless lifo=true)
	tail *sudog
	next *semaWaiters
}

// findOrAddSemaWaiters finds the waiter list for addr or creates
// one. Caller must be the only goroutine running (true on wasm3).
func findOrAddSemaWaiters(addr *uint32) *semaWaiters {
	for w := semaWaiterList; w != nil; w = w.next {
		if w.addr == addr {
			return w
		}
	}
	w := new(semaWaiters)
	w.addr = addr
	w.next = semaWaiterList
	semaWaiterList = w
	return w
}

// findSemaWaiters returns the waiter list for addr or nil.
func findSemaWaiters(addr *uint32) *semaWaiters {
	for w := semaWaiterList; w != nil; w = w.next {
		if w.addr == addr {
			return w
		}
	}
	return nil
}

func cansemacquire(addr *uint32) bool {
	for {
		v := atomic.Load(addr)
		if v == 0 {
			return false
		}
		if atomic.Cas(addr, v, v-1) {
			return true
		}
	}
}

// sync_runtime_Semacquire / semrelease and the rest of the
// linkname'd surface forward to semacquire1 / semrelease1, matching
// the !wasm3 file shape.

//go:linkname sync_runtime_Semacquire sync.runtime_Semacquire
func sync_runtime_Semacquire(addr *uint32) {
	semacquire1(addr, false, semaBlockProfile, 0, waitReasonSemacquire)
}

//go:linkname poll_runtime_Semacquire internal/poll.runtime_Semacquire
func poll_runtime_Semacquire(addr *uint32) {
	semacquire1(addr, false, semaBlockProfile, 0, waitReasonSemacquire)
}

//go:linkname sync_runtime_Semrelease sync.runtime_Semrelease
func sync_runtime_Semrelease(addr *uint32, handoff bool, skipframes int) {
	semrelease1(addr, handoff, skipframes)
}

//go:linkname internal_sync_runtime_SemacquireMutex internal/sync.runtime_SemacquireMutex
func internal_sync_runtime_SemacquireMutex(addr *uint32, lifo bool, skipframes int) {
	semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile, skipframes, waitReasonSyncMutexLock)
}

//go:linkname sync_runtime_SemacquireRWMutexR sync.runtime_SemacquireRWMutexR
func sync_runtime_SemacquireRWMutexR(addr *uint32, lifo bool, skipframes int) {
	semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile, skipframes, waitReasonSyncRWMutexRLock)
}

//go:linkname sync_runtime_SemacquireRWMutex sync.runtime_SemacquireRWMutex
func sync_runtime_SemacquireRWMutex(addr *uint32, lifo bool, skipframes int) {
	semacquire1(addr, lifo, semaBlockProfile|semaMutexProfile, skipframes, waitReasonSyncRWMutexLock)
}

//go:linkname sync_runtime_SemacquireWaitGroup sync.runtime_SemacquireWaitGroup
func sync_runtime_SemacquireWaitGroup(addr *uint32, synctestDurable bool) {
	semacquire1(addr, false, semaBlockProfile, 0, waitReasonSyncWaitGroupWait)
}

//go:linkname poll_runtime_Semrelease internal/poll.runtime_Semrelease
func poll_runtime_Semrelease(addr *uint32) {
	semrelease(addr)
}

//go:linkname internal_sync_runtime_Semrelease internal/sync.runtime_Semrelease
func internal_sync_runtime_Semrelease(addr *uint32, handoff bool, skipframes int) {
	semrelease1(addr, handoff, skipframes)
}

func semacquire(addr *uint32) {
	semacquire1(addr, false, 0, 0, waitReasonSemacquire)
}

func semacquire1(addr *uint32, lifo bool, profile semaProfileFlags, skipframes int, reason waitReason) {
	gp := getg()
	if gp != gp.m.curg {
		throw("semacquire not on the G stack")
	}

	if cansemacquire(addr) {
		return
	}

	s := acquireSudog()
	s.releasetime = 0
	s.acquiretime = 0
	s.ticket = 0

	for {
		// Re-check; wasm3 is single-threaded so a successful
		// cansemacquire here means the addr was non-zero from the
		// start. Keep the loop for sema_handoff scenarios where
		// semrelease hands us a ticket.
		if cansemacquire(addr) {
			break
		}

		// Park ourselves on addr's waiter list. wasm3 is single-
		// threaded, so no lock is needed around the list.
		w := findOrAddSemaWaiters(addr)
		s.next = nil
		s.parent = nil
		if lifo {
			s.next = w.head
			w.head = s
			if w.tail == nil {
				w.tail = s
			}
		} else {
			if w.tail == nil {
				w.head = s
			} else {
				w.tail.next = s
			}
			w.tail = s
		}

		gopark(nil, nil, reason, traceBlockSync, 4+skipframes)

		if s.ticket != 0 || cansemacquire(addr) {
			break
		}
	}
	releaseSudog(s)
}

func semrelease(addr *uint32) {
	semrelease1(addr, false, 0)
}

func semrelease1(addr *uint32, handoff bool, skipframes int) {
	atomic.Xadd(addr, 1)

	w := findSemaWaiters(addr)
	if w == nil || w.head == nil {
		return
	}

	// Pop the head waiter.
	s := w.head
	w.head = s.next
	if w.head == nil {
		w.tail = nil
	}
	s.next = nil

	if handoff && cansemacquire(addr) {
		s.ticket = 1
	}

	readyWithTime(s, 5+skipframes)
}

// ---------------------------------------------------------------
// notifyList — sync.Cond support
//
// The notifyList code in sema.go has no ptr-arith (it uses a plain
// linked list, no uintptr storage), so we mirror it verbatim here.
// Keeping it under the wasm3 build means the type matches what
// sync.Cond's go:linkname'd surface expects; layout is identical.
// ---------------------------------------------------------------

type notifyList struct {
	wait   atomic.Uint32
	notify uint32

	lock mutex
	head *sudog
	tail *sudog
}

func less(a, b uint32) bool {
	return int32(a-b) < 0
}

//go:linkname notifyListAdd sync.runtime_notifyListAdd
func notifyListAdd(l *notifyList) uint32 {
	return l.wait.Add(1) - 1
}

//go:linkname notifyListWait sync.runtime_notifyListWait
func notifyListWait(l *notifyList, t uint32) {
	lockWithRank(&l.lock, lockRankNotifyList)

	if less(t, l.notify) {
		unlock(&l.lock)
		return
	}

	s := acquireSudog()
	s.g = getg()
	// Skip `s.elem.set(unsafe.Pointer(l))` — the elem tracking is
	// used for goroutine-leak debugging on !wasm3 and not needed
	// here; setting it would re-introduce the maybeTraceablePtr
	// ptr-arith we gated for.
	s.g.waiting = s
	s.ticket = t
	if l.tail == nil {
		l.head = s
	} else {
		l.tail.next = s
	}
	l.tail = s
	goparkunlock(&l.lock, waitReasonSyncCondWait, traceBlockCondWait, 3)
	s.g.waiting = nil
	releaseSudog(s)
}

//go:linkname notifyListNotifyAll sync.runtime_notifyListNotifyAll
func notifyListNotifyAll(l *notifyList) {
	if l.wait.Load() == atomic.Load(&l.notify) {
		return
	}

	lockWithRank(&l.lock, lockRankNotifyList)
	s := l.head
	l.head = nil
	l.tail = nil

	atomic.Store(&l.notify, l.wait.Load())
	unlock(&l.lock)

	for s != nil {
		next := s.next
		s.next = nil
		readyWithTime(s, 4)
		s = next
	}
}

//go:linkname notifyListNotifyOne sync.runtime_notifyListNotifyOne
func notifyListNotifyOne(l *notifyList) {
	if l.wait.Load() == atomic.Load(&l.notify) {
		return
	}

	lockWithRank(&l.lock, lockRankNotifyList)

	t := l.notify
	if t == l.wait.Load() {
		unlock(&l.lock)
		return
	}

	atomic.Store(&l.notify, t+1)

	for p, s := (*sudog)(nil), l.head; s != nil; p, s = s, s.next {
		if s.ticket == t {
			n := s.next
			if p != nil {
				p.next = n
			} else {
				l.head = n
			}
			if n == nil {
				l.tail = p
			}
			unlock(&l.lock)
			s.next = nil
			readyWithTime(s, 4)
			return
		}
	}
	unlock(&l.lock)
}

//go:linkname notifyListCheck sync.runtime_notifyListCheck
func notifyListCheck(sz uintptr) {
	if sz != unsafe.Sizeof(notifyList{}) {
		print("runtime: bad notifyList size - sync=", sz, " runtime=", unsafe.Sizeof(notifyList{}), "\n")
		throw("bad notifyList size")
	}
}

//go:linkname internal_sync_nanotime internal/sync.runtime_nanotime
func internal_sync_nanotime() int64 {
	return nanotime()
}
