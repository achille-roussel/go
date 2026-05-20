// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 || arm64 || 386

package maps

import (
	"unsafe"
)

const memHashAESImplemented = true

func MemHash(p unsafe.Pointer, h, s uintptr) uintptr {
	if UseAeshash {
		return memHashAES(p, h, s)
	}
	return memHashFallback(p, h, s)
}

func MemHash32(p unsafe.Pointer, h uintptr) uintptr {
	if UseAeshash {
		return memHash32AES(p, h)
	}
	return memHash32Fallback(p, h)
}

func MemHash64(p unsafe.Pointer, h uintptr) uintptr {
	if UseAeshash {
		return memHash64AES(p, h)
	}
	return memHash64Fallback(p, h)
}

func StrHash(p unsafe.Pointer, h uintptr) uintptr {
	if UseAeshash {
		return strHashAES(p, h)
	}
	return strHashFallback(p, h)
}

// StrHashByValue is StrHash but takes the string by value. Avoids
// the `&local` pattern at call sites that the wasm3 obj backend
// can't model (no Go stack frame in linear memory). On AES-capable
// arches a stack slot is unavoidable for the AES path's `(*string)
// (p)` reinterpretation, so we spill back through the same shape
// strHashAES expects — the wasm3 build skips this path entirely
// (UseAeshash is false on wasm3).
func StrHashByValue(s string, h uintptr) uintptr {
	if UseAeshash {
		return strHashAES(unsafe.Pointer(&s), h)
	}
	return strHashByValueFallback(s, h)
}
