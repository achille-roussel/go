// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wasm

import (
	"cmd/internal/sys"
	"cmd/link/internal/ld"
	"internal/buildcfg"
)

func Init() (*sys.Arch, ld.Arch) {
	theArch := ld.Arch{
		Funcalign: 16,
		Maxalign:  32,
		Minalign:  1,

		Archinit:      archinit,
		AssignAddress: assignAddress,
		Asmb:          asmb,
		Asmb2:         asmb2,
		Gentext:       gentext,
	}

	// M0: wasm3 uses the wasm linker backend but must report its own arch so
	// that buildcfg.GOARCH matches arch.Name. See doc/wasm3-design.md.
	arch := sys.ArchWasm
	if buildcfg.GOARCH == "wasm3" {
		arch = sys.ArchWasm3
	}
	return arch, theArch
}

func archinit(ctxt *ld.Link) {
	if *ld.FlagRound == -1 {
		*ld.FlagRound = 4096
	}
	if *ld.FlagTextAddr == -1 {
		*ld.FlagTextAddr = 0
	}
}
