// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm3

package runtime

// concatstrings implements a Go string concatenation x+y+z+...
// The operands are passed in the slice a.
// If buf != nil, the compiler has determined that the result does not
// escape the calling function, so the string data can be stored in buf
// if small enough.
func concatstrings(buf *tmpBuf, a []string) string {
	idx := 0
	l := 0
	count := 0
	for i, x := range a {
		n := len(x)
		if n == 0 {
			continue
		}
		if l+n < l {
			throw("string concatenation too long")
		}
		l += n
		count++
		idx = i
	}
	if count == 0 {
		return ""
	}

	// If there is just one string and either it is not on the stack
	// or our result does not escape the calling frame (buf != nil),
	// then we can return that string directly.
	if count == 1 && (buf != nil || !stringDataOnStack(a[idx])) {
		return a[idx]
	}
	s, b := rawstringtmp(buf, l)
	for _, x := range a {
		n := copy(b, x)
		b = b[n:]
	}
	return s
}

// concatbytes implements a Go string concatenation x+y+z+... returning a slice
// of bytes.
// The operands are passed in the slice a.
func concatbytes(buf *tmpBuf, a []string) []byte {
	l := 0
	for _, x := range a {
		n := len(x)
		if l+n < l {
			throw("string concatenation too long")
		}
		l += n
	}
	if l == 0 {
		// This is to match the return type of the non-optimized concatenation.
		return []byte{}
	}

	var b []byte
	if buf != nil && l <= len(buf) {
		*buf = tmpBuf{}
		b = buf[:l]
	} else {
		b = rawbyteslice(l)
	}
	offset := 0
	for _, x := range a {
		copy(b[offset:], x)
		offset += len(x)
	}

	return b
}
