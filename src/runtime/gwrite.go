// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// write to goroutine-local buffer if diverting output,
// or else standard error.
func gwrite(b []byte) {
	if len(b) == 0 {
		return
	}
	recordForPanic(b)
	gp := getg()
	// Don't use the writebuf if gp.m is dying. We want anything
	// written through gwrite to appear in the terminal rather
	// than be written to in some buffer, if we're in a panicking state.
	// Note that we can't just clear writebuf in the gp.m.dying case
	// because a panic isn't allowed to have any write barriers.
	if gp == nil || gp.writebuf == nil || gp.m.dying > 0 {
		writeErr(b)
		return
	}

	n := copy(gp.writebuf[len(gp.writebuf):cap(gp.writebuf)], b)
	gp.writebuf = gp.writebuf[:len(gp.writebuf)+n]
}
