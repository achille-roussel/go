// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 mirror of env_posix.go. The only divergence from the
// !wasm3 original is setenv_c / unsetenv_c: cgo is unreachable on
// wasm3 (no C interop, no _cgo_setenv / _cgo_unsetenv ABI), so
// both functions are no-op stubs. On the shared !wasm3 path the
// asmcgocall(_cgo_setenv, unsafe.Pointer(&arg)) and its unsetenv
// counterpart trip wasm3check on the polyptr classifier — the
// `unsafe.Pointer(&arg)` materializes a stack-array address as
// i64 then passes it through anyref-classified per-value locals.
// Removing the dead branch removes the warnings cleanly.
//
// _cgo_setenv / _cgo_unsetenv declarations are preserved on the
// mirror so external linkname referrers
// (github.com/ebitengine/purego per the upstream comment) keep
// resolving against the runtime; the values stay nil forever on
// wasm3.

package runtime

import "unsafe"

func gogetenv(key string) string {
	env := environ()
	if env == nil {
		throw("getenv before env init")
	}
	for _, s := range env {
		if len(s) > len(key) && s[len(key)] == '=' && envKeyEqual(s[:len(key)], key) {
			return s[len(key)+1:]
		}
	}
	return ""
}

// envKeyEqual reports whether a == b, with ASCII-only case insensitivity
// on Windows. The two strings must have the same length.
func envKeyEqual(a, b string) bool {
	if GOOS == "windows" { // case insensitive
		for i := 0; i < len(a); i++ {
			ca, cb := a[i], b[i]
			if ca == cb || lowerASCII(ca) == lowerASCII(cb) {
				continue
			}
			return false
		}
		return true
	}
	return a == b
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// _cgo_setenv should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/ebitengine/purego
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname _cgo_setenv
var _cgo_setenv unsafe.Pointer // pointer to C function (always nil on wasm3)

// _cgo_unsetenv should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/ebitengine/purego
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname _cgo_unsetenv
var _cgo_unsetenv unsafe.Pointer // pointer to C function (always nil on wasm3)

// setenv_c / unsetenv_c are no-ops on wasm3 (cgo unreachable).
// The !wasm3 path would call asmcgocall on the cgo hook; here we
// just return without materializing the unsafe.Pointer{&arg}
// stack-array address that trips wasm3check.
func setenv_c(k string, v string)  {}
func unsetenv_c(k string)          {}

func cstring(s string) unsafe.Pointer {
	p := make([]byte, len(s)+1)
	copy(p, s)
	return unsafe.Pointer(&p[0])
}
