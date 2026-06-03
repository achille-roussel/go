// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// On wasm3 the guintptr/muintptr/puintptr write-barrier-bypass that
// the other arches need is moot: every reference is a WasmGC ref
// managed by the host GC, there are no Go-side write barriers, and
// uintptr cannot store a Go pointer at all (anyref has no scalar
// form). The barrier-bypass machinery is replaced by direct typed
// pointers, while the type names and method surface (ptr / set /
// cas / setGNoWB / setMNoWB / *g.guintptr) are kept so that
// runtime2.go's field declarations (Gobuf.g, p.runnext, p.runq,
// schedlink, lockedm, lockedg, p, nextp, oldp, ...) compile
// unchanged on wasm3.
//
// Comparisons in the upstream scheduler code that exploit the
// uintptr representation — `pp.runnext != 0`, `gp.lockedm != 0`,
// `pp.runnext.cas(next, 0)` — do NOT compile against these
// pointer-typed wrappers. Rung 2 of the wasm3 runtime port
// (doc/wasm3-runtime-rung0-inventory.md) replaces those idioms
// with wasm3-friendly forms (e.g. `pp.runnext.ptr() != nil`,
// `pp.runnext.cas(next, guintptr{})`). Until Rung 2 lands the
// wasm3 build of proc.go / sema.go remains red — that is the
// expected mid-port state per the build-forward strategy.

type guintptr struct{ p *g }

//go:nosplit
func (gp guintptr) ptr() *g { return gp.p }

//go:nosplit
func (gp *guintptr) set(g *g) { gp.p = g }

//go:nosplit
func (gp *guintptr) cas(old, new guintptr) bool {
	// wasm3 is single-threaded (continuation-based scheduler);
	// no real atomicity is required. A plain compare-and-set
	// preserves the observable semantics of the original
	// atomic.Casuintptr-backed implementation.
	if gp.p == old.p {
		gp.p = new.p
		return true
	}
	return false
}

//go:nosplit
func (gp *g) guintptr() guintptr {
	return guintptr{p: gp}
}

// setGNoWB performs *gp = new. There are no Go-side write
// barriers on wasm3, so the "no write barrier" qualifier is
// vacuously satisfied; the host GC still tracks the ref via
// the *g slot.
//
//go:nosplit
//go:nowritebarrier
func setGNoWB(gp **g, new *g) { *gp = new }

type puintptr struct{ p *p }

//go:nosplit
func (pp puintptr) ptr() *p { return pp.p }

//go:nosplit
func (pp *puintptr) set(p *p) { pp.p = p }

// muintptr is the wasm3 counterpart of the non-wasm3 muintptr
// (see scheduler_intptr.go). The two constraints upstream lists
// for muintptr — "never hold across a safe point" and "must be
// owned by the M itself" — are also vacuously satisfied on
// wasm3: there is no safepoint-driven GC, and the host GC tracks
// every *m ref slot.
type muintptr struct{ p *m }

//go:nosplit
func (mp muintptr) ptr() *m { return mp.p }

//go:nosplit
func (mp *muintptr) set(m *m) { mp.p = m }

//go:nosplit
//go:nowritebarrier
func setMNoWB(mp **m, new *m) { *mp = new }

// noG, noM, noP are zero-value sentinels matching the non-wasm3
// constants of the same name in scheduler_intptr.go. They exist so
// scheduler code can write `pp.runnext = noG` / `gp.lockedm != noM`
// portably across both representations.
//
// On non-wasm3 these are typed-zero uintptr consts; here they are
// zero-value struct-wrapper vars. They are never mutated — the
// `var` form is dictated by Go disallowing struct const-expressions,
// not by intentional runtime state.
var (
	noG guintptr
	noM muintptr
	noP puintptr
)
