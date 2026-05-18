// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"unsafe"
)

// The compiler knows that a print of a value of this type
// should use printhex instead of printuint (decimal).
type hex uint64

// The compiler knows that a print of a value of this type should use
// printquoted instead of printstring.
type quoted string

func bytes(s string) []byte {
	// unsafe.Slice builds the slice header from the string's
	// underlying pointer and length without first writing through
	// `(*slice)(unsafe.Pointer(&ret))` to a stack-allocated slice
	// header. The original reflection-based version emitted
	// `Get $ret(SP)` which the wasm3 obj backend can't address
	// (no Go stack frame in linear memory), and the new form is
	// equally zero-allocation on every other arch — both lower to
	// a build-the-3-word-header instruction sequence with no heap
	// or stack-storage involvement.
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

var (
	// printBacklog is a circular buffer of messages written with the builtin
	// print* functions, for use in postmortem analysis of core dumps.
	printBacklog      [512]byte
	printBacklogIndex int
)

// recordForPanic maintains a circular buffer of messages written by the
// runtime leading up to a process crash, allowing the messages to be
// extracted from a core dump.
//
// The text written during a process crash (following "panic" or "fatal
// error") is not saved, since the goroutine stacks will generally be readable
// from the runtime data structures in the core file.
func recordForPanic(b []byte) {
	printlock()

	if panicking.Load() == 0 {
		// Not actively crashing: maintain circular buffer of print output.
		for i := 0; i < len(b); {
			n := copy(printBacklog[printBacklogIndex:], b[i:])
			i += n
			printBacklogIndex += n
			printBacklogIndex %= len(printBacklog)
		}
	}

	printunlock()
}

var debuglock mutex

// The compiler emits calls to printlock and printunlock around
// the multiple calls that implement a single Go print or println
// statement. Some of the print helpers (printslice, for example)
// call print recursively. There is also the problem of a crash
// happening during the print routines and needing to acquire
// the print lock to print information about the crash.
// For both these reasons, let a thread acquire the printlock 'recursively'.

// printlock and printunlock live in printlock.go (default) and
// printlock_wasm3.go (a wasm3-specific no-op pair the runtime fork
// uses while M2 is in flight; neither g0/m0 nor debuglock are
// initialized yet for wasm3, so the standard implementation would
// trap immediately).

// gwrite lives in gwrite.go (default) and gwrite_wasm3.go (a wasm3-
// specific simplification for the M2 cutover; the standard version
// dereferences getg().writebuf and getg().m.dying, neither of which
// is initialized at this stage of the cutover).

func printsp() {
	printstring(" ")
}

func printnl() {
	printstring("\n")
}

func printbool(v bool) {
	if v {
		printstring("true")
	} else {
		printstring("false")
	}
}

// printfloat*/printcomplex* live in printfloat.go (default) and
// printfloat_wasm3.go on wasm3. The default routes
// gwrite(strconv.AppendFloat(buf[:0], ...)) which pulls every
// internal/strconv helper into the link — and several of those
// trip wasm3 validation on the slice-in-arg ABI (a `[]byte` arg
// lowers to anyref-backing + i64-len + i64-cap but the helpers'
// internal slice-header rebuilds emit i64.store of an anyref).
// wasm3 prints a fixed `<float>`/`<complex>` placeholder until
// Stage E composite-marshalling lands.

// printuint and printint live in printnum.go (default) and
// printnum_wasm3.go (a wasm3-specific pair that uses a package-global
// buffer instead of a stack-allocated one; the M2 cutover does not yet
// box escaping &local addresses, so a stack buf would emit
// `Get $name(SP)` which the wasm3 obj backend bails on).

var minhexdigits = 0 // protected by printlock

func printhexopts(include0x bool, mindigits int, v uint64) {
	const dig = "0123456789abcdef"
	var buf [100]byte
	i := len(buf)
	for i--; i > 0; i-- {
		buf[i] = dig[v%16]
		if v < 16 && len(buf)-i >= mindigits {
			break
		}
		v /= 16
	}
	if include0x {
		i--
		buf[i] = 'x'
		i--
		buf[i] = '0'
	}
	gwrite(buf[i:])
}

func printhex(v uint64) {
	printhexopts(true, minhexdigits, v)
}

func printquoted(s string) {
	printlock()
	gwrite([]byte(`"`))
	for i, r := range s {
		switch r {
		case '\n':
			gwrite([]byte(`\n`))
			continue
		case '\r':
			gwrite([]byte(`\r`))
			continue
		case '\t':
			gwrite([]byte(`\t`))
			print()
			continue
		case '\\', '"':
			gwrite([]byte{byte('\\'), byte(r)})
			continue
		case runeError:
			// Distinguish errors from a valid encoding of U+FFFD.
			if _, j := decoderune(s, uint(i)); j == uint(i+1) {
				gwrite(bytes(`\x`))
				printhexopts(false, 2, uint64(s[i]))
				continue
			}
			// Fall through to quoting.
		}
		// For now, only allow basic printable ascii through unescaped
		if r >= ' ' && r <= '~' {
			gwrite([]byte{byte(r)})
		} else if r < 127 {
			gwrite(bytes(`\x`))
			printhexopts(false, 2, uint64(r))
		} else if r < 0x1_0000 {
			gwrite(bytes(`\u`))
			printhexopts(false, 4, uint64(r))
		} else {
			gwrite(bytes(`\U`))
			printhexopts(false, 8, uint64(r))
		}
	}
	gwrite([]byte{byte('"')})
	printunlock()
}

func printpointer(p unsafe.Pointer) {
	printhex(uint64(uintptr(p)))
}
func printuintptr(p uintptr) {
	printhex(uint64(p))
}

// printstring lives in printstring.go (default !wasm3) and
// printstring_wasm3.go (a wasm3-specific bypass that calls write1
// directly with the string's pointer/length, avoiding bytes() — the
// helper escapes its locals through getg-dependent boxing the M2
// cutover has not yet wired up).

func printslice(s []byte) {
	sp := (*slice)(unsafe.Pointer(&s))
	print("[", len(s), "/", cap(s), "]")
	printpointer(sp.array)
}

func printeface(e eface) {
	print("(", e._type, ",", e.data, ")")
}

func printiface(i iface) {
	print("(", i.tab, ",", i.data, ")")
}
