// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !wasm3

package runtime

import "unsafe"

// memmove copies n bytes from "from" to "to".
//
// memmove ensures that any pointer in "from" is written to "to" with
// an indivisible write, so that racy reads cannot observe a
// half-written pointer. This is necessary to prevent the garbage
// collector from observing invalid pointers, and differs from memmove
// in unmanaged languages. However, memmove is only required to do
// this if "from" and "to" may contain pointers, which can only be the
// case if "from", "to", and "n" are all be word-aligned.
//
// Implementations are in memmove_*.s.
//
// Outside assembly calls memmove.
//
// memmove should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/bytedance/sonic
//   - github.com/cloudwego/dynamicgo
//   - github.com/ebitengine/purego
//   - github.com/tetratelabs/wazero
//   - github.com/ugorji/go/codec
//   - gvisor.dev/gvisor
//   - github.com/sagernet/gvisor
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname memmove
//go:noescape
func memmove(to, from unsafe.Pointer, n uintptr)
