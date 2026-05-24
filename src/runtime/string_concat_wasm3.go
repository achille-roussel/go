// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build wasm3

package runtime

// concatstrings on wasm3 folds the source slice through the
// go_runtime.stringConcat2 wat primitive — a binary concat that
// allocates a fresh (ref $go.bytes) and array.copies both operands
// in (see runtime/wasm/go_runtime.wat). The compiler-side `a + b`
// lowering chains stringConcat2 calls already; runtime.concatstrings
// is the slice-variadic entry that runtime/string.go's "concat3+"
// path eventually reaches, so the same fold goes here.
//
// This bypasses the original linear-memory-buffer scheme entirely —
// no mallocgc, no memmove of i64 pointers, no unsafe.String wrap of
// linear memory. Every intermediate string is a WasmGC $go.string
// whose backing is a $go.bytes array on the host GC heap, matching
// the boxed-string ABI [[wasm3-boxed-bulkops]].
//
// The count==0 and count==1 fast paths preserve the original
// allocation-free semantics: zero operands → "", one non-empty
// operand → that operand (shared backing, no copy). The tmpBuf
// argument is ignored on wasm3 — its purpose (avoiding a heap
// allocation for small results) doesn't apply when the result
// backing lives on the host GC heap rather than in linear memory.
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
	if count == 1 {
		return a[idx]
	}
	// Fold non-empty operands through the binary wat primitive.
	// stringConcat2's own 0-length fast path returns the other
	// operand unchanged, so we needn't filter empties first.
	var r string
	first := true
	for _, x := range a {
		if len(x) == 0 {
			continue
		}
		if first {
			r = x
			first = false
			continue
		}
		r = wasm3StringConcat2(r, x)
	}
	return r
}

// wasm3StringConcat2 bridges to the go_runtime.stringConcat2 wat
// primitive — binary string concatenation. The compiler's `a + b`
// lowering on wasm3 will eventually call this directly; for now
// concatstrings folds through it.
//
//go:wasmimport go_runtime stringConcat2
func wasm3StringConcat2(a, b string) string
