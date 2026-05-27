// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// Single-threaded lock / notify primitives for GOOS=js GOARCH=wasm3.
// Mirrors lock_wasip1.go in shape — both targets run a single OS
// thread under the host. The notetsleepg path uses Gosched to let
// the scheduler run other goroutines; Phase 3 hooks this into the
// JSPI park path. Filename intentionally lacks an `_GOOS_GOARCH`
// suffix (`jswasm3`, not `js_wasm3`) so the explicit build tag is
// the only constraint — implicit filename suffixes can only
// narrow, not widen.

package runtime

const (
	mutex_unlocked = 0
	mutex_locked   = 1

	active_spin     = 4
	active_spin_cnt = 30
)

type mWaitList struct{}

func lockVerifyMSize() {}

func mutexContended(l *mutex) bool { return false }

func lock(l *mutex) { lockWithRank(l, getLockRank(l)) }

func lock2(l *mutex) {
	if l.key == mutex_locked {
		throw("self deadlock")
	}
	gp := getg()
	if gp.m.locks < 0 {
		throw("lock count")
	}
	gp.m.locks++
	l.key = mutex_locked
}

func unlock(l *mutex) { unlockWithRank(l) }

func unlock2(l *mutex) {
	if l.key == mutex_unlocked {
		throw("unlock of unlocked lock")
	}
	gp := getg()
	gp.m.locks--
	if gp.m.locks < 0 {
		throw("lock count")
	}
	l.key = mutex_unlocked
}

// note primitives: note's full struct comes from note_js.go (which
// is included for GOOS=js regardless of arch). We use only the
// .status field for Phase 0 — 0=cleared, 1=woken. Phase 3 wires
// notesleep/notetsleep through the JSPI park path and uses .gp /
// .deadline / .allprev / .allnext like lock_js.go does.

func noteclear(n *note) { n.status = 0 }

func notewakeup(n *note) {
	if n.status != 0 {
		print("notewakeup - double wakeup (", n.status, ")\n")
		throw("notewakeup - double wakeup")
	}
	n.status = 1
}

func notesleep(n *note) { throw("notesleep not supported by js/wasm3") }

func notetsleep(n *note, ns int64) bool {
	throw("notetsleep not supported by js/wasm3")
	return false
}

// notetsleepg: js/wasm3 uses Gosched to let the scheduler run
// other goroutines. Phase 3 will hook this into the JSPI park path.
func notetsleepg(n *note, ns int64) bool {
	gp := getg()
	if gp == gp.m.g0 {
		throw("notetsleepg on g0")
	}
	deadline := nanotime() + ns
	for {
		if n.status != 0 {
			return true
		}
		Gosched()
		if ns >= 0 && nanotime() >= deadline {
			return false
		}
	}
}

func beforeIdle(int64, int64) (*g, bool) { return nil, false }

func checkTimeouts() {}
