// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

// wasm3 mirror of error.go. The only divergence from the !wasm3
// original is boundsError.Error and the boundsErrorFmts /
// boundsNegErrorFmts package-level [N]string arrays. The original
// indexes those arrays by abi.BoundsErrorCode at run time. The
// wasm3 SSA backend currently emits the indexed read as
// `(LoweredAddr {boundsErrorFmts}) + (OffPtr [code*16]) + i64.load`
// for the slice {array, len, cap} words. The boxed-globals
// rewrite at Wasm3.rules:116 only matches Load directly from
// LoweredAddr {sym} [0] (SB); the OffPtr-wrapped indexed-array
// shape doesn't match, so the loads emit i64 results into the
// anyref-classified per-value locals downstream — the documented
// #167-pending bridge-gap pattern.
//
// Rather than wait for #167 to migrate package-level array vars
// to WasmGC ref-globals + array.get codegen, the wasm3 boundsError
// .Error mirror sidesteps the indexed lookup with a switch over
// the format-code constants. The format strings are inlined per
// arm, so the read goes through `OpStringMake` (string-literal
// const) — already a typed-ref shape — rather than the indexed
// array read. Same output as the !wasm3 path. boundsErrorFmts /
// boundsNegErrorFmts are unreferenced on wasm3 and elide via DCE
// at link time.

package runtime

import (
	"internal/abi"
	"internal/bytealg"
	"internal/runtime/sys"
)

// Error identifies a runtime error used in panic.
//
// The Go runtime triggers panics for a variety of cases, as described by the
// Go Language Spec, such as out-of-bounds slice/array access, close of nil
// channels, type assertion failures, etc.
//
// When these cases occur, the Go runtime panics with an error that implements
// Error. This can be useful when recovering from panics to distinguish between
// custom application panics and fundamental runtime panics.
//
// Packages outside of the Go standard library should not implement Error.
type Error interface {
	error

	// RuntimeError is a no-op function but
	// serves to distinguish types that are runtime
	// errors from ordinary errors: a type is a
	// runtime error if it has a RuntimeError method.
	RuntimeError()
}

// A TypeAssertionError explains a failed type assertion.
type TypeAssertionError struct {
	_interface    *_type
	concrete      *_type
	asserted      *_type
	missingMethod string // one method needed by Interface, missing from Concrete
}

func (*TypeAssertionError) RuntimeError() {}

func (e *TypeAssertionError) Error() string {
	inter := "interface"
	if e._interface != nil {
		inter = toRType(e._interface).string()
	}
	as := toRType(e.asserted).string()
	if e.concrete == nil {
		return "interface conversion: " + inter + " is nil, not " + as
	}
	cs := toRType(e.concrete).string()
	if e.missingMethod == "" {
		msg := "interface conversion: " + inter + " is " + cs + ", not " + as
		if cs == as {
			// provide slightly clearer error message
			if toRType(e.concrete).pkgpath() != toRType(e.asserted).pkgpath() {
				msg += " (types from different packages)"
			} else {
				msg += " (types from different scopes)"
			}
		}
		return msg
	}
	return "interface conversion: " + cs + " is not " + as +
		": missing method " + e.missingMethod
}

// itoa converts val to a decimal representation. The result is
// written somewhere within buf and the location of the result is returned.
// buf must be at least 20 bytes.
//
//go:nosplit
func itoa(buf []byte, val uint64) []byte {
	i := len(buf) - 1
	for val >= 10 {
		buf[i] = byte(val%10 + '0')
		i--
		val /= 10
	}
	buf[i] = byte(val + '0')
	return buf[i:]
}

// An errorString represents a runtime error described by a single string.
type errorString string

func (e errorString) RuntimeError() {}

func (e errorString) Error() string {
	return "runtime error: " + string(e)
}

type errorAddressString struct {
	msg  string  // error message
	addr uintptr // memory address where the error occurred
}

func (e errorAddressString) RuntimeError() {}

func (e errorAddressString) Error() string {
	return "runtime error: " + e.msg
}

// Addr returns the memory address where a fault occurred.
// The address provided is best-effort.
// The veracity of the result may depend on the platform.
// Errors providing this method will only be returned as
// a result of using [runtime/debug.SetPanicOnFault].
func (e errorAddressString) Addr() uintptr {
	return e.addr
}

// plainError represents a runtime error described a string without
// the prefix "runtime error: " after invoking errorString.Error().
// See Issue #14965.
type plainError string

func (e plainError) RuntimeError() {}

func (e plainError) Error() string {
	return string(e)
}

// A boundsError represents an indexing or slicing operation gone wrong.
type boundsError struct {
	x int64
	y int
	// Values in an index or slice expression can be signed or unsigned.
	// That means we'd need 65 bits to encode all possible indexes, from -2^63 to 2^64-1.
	// Instead, we keep track of whether x should be interpreted as signed or unsigned.
	// y is known to be nonnegative and to fit in an int.
	signed bool
	code   abi.BoundsErrorCode
}

// boundsErrorFmts / boundsNegErrorFmts are intentionally omitted on
// wasm3 — the indexed [N]string read is the bridge-gap pattern
// (#167). boundsError.Error inlines the per-code format string via
// a switch so the read goes through string-literal constants
// (typed-ref) instead. The non-wasm3 file keeps both arrays for the
// rest of the world.

func (e boundsError) RuntimeError() {}

func appendIntStr(b []byte, v int64, signed bool) []byte {
	if signed && v < 0 {
		b = append(b, '-')
		v = -v
	}
	var buf [20]byte
	b = append(b, itoa(buf[:], uint64(v))...)
	return b
}

// wasm3BoundsErrorFmt returns the format string for a bounds-error
// code (and the negative-x variant if signed && x < 0). Inlined as
// a switch instead of an [N]string index so the wasm3 SSA layer
// gets a string-literal const at each arm rather than the
// indexed-array read that's gated on #167.
func wasm3BoundsErrorFmt(code abi.BoundsErrorCode, negX bool) string {
	if negX {
		switch code {
		case abi.BoundsIndex:
			return "index out of range [%x]"
		case abi.BoundsSliceAlen:
			return "slice bounds out of range [:%x]"
		case abi.BoundsSliceAcap:
			return "slice bounds out of range [:%x]"
		case abi.BoundsSliceB:
			return "slice bounds out of range [%x:]"
		case abi.BoundsSlice3Alen:
			return "slice bounds out of range [::%x]"
		case abi.BoundsSlice3Acap:
			return "slice bounds out of range [::%x]"
		case abi.BoundsSlice3B:
			return "slice bounds out of range [:%x:]"
		case abi.BoundsSlice3C:
			return "slice bounds out of range [%x::]"
		}
		return ""
	}
	switch code {
	case abi.BoundsIndex:
		return "index out of range [%x] with length %y"
	case abi.BoundsSliceAlen:
		return "slice bounds out of range [:%x] with length %y"
	case abi.BoundsSliceAcap:
		return "slice bounds out of range [:%x] with capacity %y"
	case abi.BoundsSliceB:
		return "slice bounds out of range [%x:%y]"
	case abi.BoundsSlice3Alen:
		return "slice bounds out of range [::%x] with length %y"
	case abi.BoundsSlice3Acap:
		return "slice bounds out of range [::%x] with capacity %y"
	case abi.BoundsSlice3B:
		return "slice bounds out of range [:%x:%y]"
	case abi.BoundsSlice3C:
		return "slice bounds out of range [%x:%y:]"
	case abi.BoundsConvert:
		return "cannot convert slice with length %y to array or pointer to array with length %x"
	}
	return ""
}

func (e boundsError) Error() string {
	fmt := wasm3BoundsErrorFmt(e.code, e.signed && e.x < 0)
	// max message length is 99: "runtime error: slice bounds out of range [::%x] with capacity %y"
	// x can be at most 20 characters. y can be at most 19.
	b := make([]byte, 0, 100)
	b = append(b, "runtime error: "...)
	for i := 0; i < len(fmt); i++ {
		c := fmt[i]
		if c != '%' {
			b = append(b, c)
			continue
		}
		i++
		switch fmt[i] {
		case 'x':
			b = appendIntStr(b, e.x, e.signed)
		case 'y':
			b = appendIntStr(b, int64(e.y), true)
		}
	}
	return string(b)
}

type stringer interface {
	String() string
}

// printpanicval prints an argument passed to panic.
// If panic is called with a value that has a String or Error method,
// it has already been converted into a string by preprintpanics.
//
// To ensure that the traceback can be unambiguously parsed even when
// the panic value contains "\ngoroutine" and other stack-like
// strings, newlines in the string representation of v are replaced by
// "\n\t".
func printpanicval(v any) {
	switch v := v.(type) {
	case nil:
		print("nil")
	case bool:
		print(v)
	case int:
		print(v)
	case int8:
		print(v)
	case int16:
		print(v)
	case int32:
		print(v)
	case int64:
		print(v)
	case uint:
		print(v)
	case uint8:
		print(v)
	case uint16:
		print(v)
	case uint32:
		print(v)
	case uint64:
		print(v)
	case uintptr:
		print(v)
	case float32:
		print(v)
	case float64:
		print(v)
	case complex64:
		print(v)
	case complex128:
		print(v)
	case string:
		printindented(v)
	default:
		printanycustomtype(v)
	}
}

// Invariant: each newline in the string representation is followed by a tab.
func printanycustomtype(i any) {
	eface := efaceOf(&i)
	typestring := toRType(eface._type).string()

	switch eface._type.Kind() {
	case abi.String:
		print(typestring, `("`)
		printindented(*(*string)(eface.data))
		print(`")`)
	case abi.Bool:
		print(typestring, "(", *(*bool)(eface.data), ")")
	case abi.Int:
		print(typestring, "(", *(*int)(eface.data), ")")
	case abi.Int8:
		print(typestring, "(", *(*int8)(eface.data), ")")
	case abi.Int16:
		print(typestring, "(", *(*int16)(eface.data), ")")
	case abi.Int32:
		print(typestring, "(", *(*int32)(eface.data), ")")
	case abi.Int64:
		print(typestring, "(", *(*int64)(eface.data), ")")
	case abi.Uint:
		print(typestring, "(", *(*uint)(eface.data), ")")
	case abi.Uint8:
		print(typestring, "(", *(*uint8)(eface.data), ")")
	case abi.Uint16:
		print(typestring, "(", *(*uint16)(eface.data), ")")
	case abi.Uint32:
		print(typestring, "(", *(*uint32)(eface.data), ")")
	case abi.Uint64:
		print(typestring, "(", *(*uint64)(eface.data), ")")
	case abi.Uintptr:
		print(typestring, "(", *(*uintptr)(eface.data), ")")
	case abi.Float32:
		print(typestring, "(", *(*float32)(eface.data), ")")
	case abi.Float64:
		print(typestring, "(", *(*float64)(eface.data), ")")
	case abi.Complex64:
		print(typestring, *(*complex64)(eface.data))
	case abi.Complex128:
		print(typestring, *(*complex128)(eface.data))
	default:
		print("(", typestring, ") ", eface.data)
	}
}

// printindented prints s, replacing "\n" with "\n\t".
func printindented(s string) {
	for {
		i := bytealg.IndexByteString(s, '\n')
		if i < 0 {
			break
		}
		i += len("\n")
		print(s[:i])
		print("\t")
		s = s[i:]
	}
	print(s)
}

// panicwrap generates a panic for a call to a wrapped value method
// with a nil pointer receiver.
//
// It is called from the generated wrapper code.
func panicwrap() {
	pc := sys.GetCallerPC()
	name := funcNameForPrint(funcname(findfunc(pc)))
	// name is something like "main.(*T).F".
	// We want to extract pkg ("main"), typ ("T"), and meth ("F").
	// Do it by finding the parens.
	i := bytealg.IndexByteString(name, '(')
	if i < 0 {
		throw("panicwrap: no ( in " + name)
	}
	pkg := name[:i-1]
	if i+2 >= len(name) || name[i-1:i+2] != ".(*" {
		throw("panicwrap: unexpected string after package name: " + name)
	}
	name = name[i+2:]
	i = bytealg.IndexByteString(name, ')')
	if i < 0 {
		throw("panicwrap: no ) in " + name)
	}
	if i+2 >= len(name) || name[i:i+2] != ")." {
		throw("panicwrap: unexpected string after type name: " + name)
	}
	typ := name[:i]
	meth := name[i+2:]
	panic(plainError("value method " + pkg + "." + typ + "." + meth + " called using nil *" + typ + " pointer"))
}
