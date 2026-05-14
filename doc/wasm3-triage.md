# wasm3 — `wasm` check triage list (M0 record)

This is the M0 deliverable from the implementation plan: an inventory of every place
the toolchain, runtime, or standard library tests `wasm`-ness in a way that does **not**
automatically apply to `GOARCH=wasm3`. For M0 every site listed here was widened so
`wasm3` behaves identically to `wasm`. Each remains an **M2 decision point** — when
wasm3 diverges from wasm, these are the sites to revisit.

**M0 status: COMPLETE.** `GOOS=wasip1 GOARCH=wasm3` builds the toolchain, compiles and
links Go programs to valid wasm that runs on wasmtime; `go run`, `go vet`, and `go test`
work; stdlib test suites (`strings`, `sort`, `strconv`, `bytes`, …) pass; output is
byte-near-identical to `GOARCH=wasm` (a few bytes of metadata differ from the longer
arch name).

## How wasm3 relates to wasm

- `wasm3` is a distinct GOARCH that **shares the `sys.Wasm` arch family**
  (`ArchWasm3.Family == sys.Wasm`), so `Family`-keyed checks fire unchanged.
- `wasm3` has a **distinct `*sys.Arch` value** (`sys.ArchWasm3`) and a distinct
  `Arch.Name == "wasm3"`, so pointer-equality and name-string checks needed widening.
- `wasm3` has its own `internal/goarch` identity: `goarch.GOARCH == "wasm3"`,
  `IsWasm3 == 1`, `IsWasm == 0`. A new `goarch.IsWasmFamily` (`IsWasm | IsWasm3`)
  covers "the wasm family".
- For the M0 toolchain wiring, the compiler/assembler/linker all use a new
  `wasm.Linkwasm3` `obj.LinkArch` (a clone of `Linkwasm` with `Arch: sys.ArchWasm3`)
  so all three tools agree on the arch identity in object files.

## A. `buildcfg.GOARCH == "wasm"` — compiler/linker string checks  ☑ widened

`cmd/internal/obj/fips140.go`, `cmd/compile/internal/noder/{writer,reader,noder,linker}.go`,
`cmd/compile/internal/ssagen/abi.go` (×2), `cmd/compile/internal/base/flag.go`
(`Flag.Dwarf`), `cmd/link/internal/ld/config.go`.

## B. `GOARCH == "wasm"` — runtime package string checks  ☑ widened

`runtime/debug.go`, `runtime/proc.go` (×8, incl. `haveSysmon`), `runtime/symtab.go` (×6).

## C. `goarch.IsWasm` — runtime/internal const arithmetic  ☑ switched to `IsWasmFamily`

`runtime/{runtime2,alg,panic,mpagealloc,malloc}.go`, `internal/runtime/maps/map.go`.
New file `internal/goarch/iswasm.go` defines `IsWasmFamily`.

## D. `sys.ArchWasm` — `*sys.Arch` pointer-equality checks  ☑ switched to `Family == sys.Wasm`

Discovered during M0 (these caused a ~150 KB code-section regression before being
fixed): `cmd/compile/internal/ssa/regalloc.go` (×5, was `Arch.Arch == sys.ArchWasm`),
`cmd/compile/internal/ssagen/intrinsics.go` (`hasCMOV` slice — added `sys.ArchWasm3`).

## E. Arch-name `switch`/`case` and arch→data maps  ☑ wasm3 case/entry added

`cmd/compile/internal/ssa/config.go` (`NewConfig`, the `default` is "arch not
implemented"), `cmd/compile/internal/ssa/rewrite.go` (rotate-bits switch),
`cmd/compile/internal/noder/reader.go` (`Arch.Name != "wasm"`),
`cmd/compile/internal/types2/{sizes,gccgosizes}.go`, `go/types/{sizes,gccgosizes}.go`
(the last two are used by `go vet`; missing entries panicked vet),
`cmd/asm/internal/arch/arch.go` (`Set`).

## F. The entry-symbol convention  ☑ followed

The linker derives the entry symbol as `_rt0_<GOARCH>_<GOOS>`
(`cmd/link/internal/ld/lib.go`). So `rt0_wasip1_wasm3.s` defines `_rt0_wasm3_wasip1`
(not a byte-identical copy — the symbol name encodes the arch), and
`cmd/link/internal/wasm/asm.go` constructs the export entry name from `buildcfg.GOARCH`
and lists the `_rt0_wasm3_*` names in `wasmFuncTypes`. The wasm assembler
(`cmd/internal/obj/wasm/wasmobj.go`) also lists the `_rt0_wasm3_*` entry functions in
its `notUsePC_B` map and special-calling-convention `switch` (these give entry
functions the no-PC_B convention; missing them caused "unknown local" validation
failures).

## G. Explicit `//go:build` constraints mentioning `wasm`  ☑ widened

Runtime: `mem_sbrk.go`, `mem_nonsbrk.go`, `lock_spinbit.go`, `stubs_nonwasm.go`,
`cputicks.go`, `mpagealloc_32bit.go`, `tagptr_64bit.go`. Stdlib:
`internal/runtime/atomic/{atomic_andor_generic,types_64bit,stubs}.go`,
`internal/bytealg/{compare,indexbyte}_{native,generic}.go`,
`internal/runtime/maps/runtime_hash32.go`, `math/floor_{asm,noasm}.go`,
`net/http/transport_default_other.go`,
`crypto/internal/fips140/{bigmod/nat_noasm.go,drbg/entropy_fips140.go,check/checktest/asm.s}`,
`encoding/binary/native_endian_little.go`, `vendor/golang.org/x/sys/cpu/endian_little.go`,
`os/exec/internal/fdtest/exists_unix.go`.
`(js && wasm)`-tagged files were **not** touched — `wasip1/wasm3` is covered by their
`|| wasip1` term; they matter only for the (deferred) `js/wasm3` GOOS.

## H. Filename-suffixed `*_wasm.{go,s}` files  ☑ duplicated as `*_wasm3.{go,s}`

The `_wasm` filename suffix is an implicit `GOARCH==wasm` constraint a `//go:build` line
cannot override. Per the chosen approach, each was duplicated to `*_wasm3.{go,s}`
(byte-identical for M0, except `rt0_wasip1_wasm3.s` — see F — and six files whose
copied explicit `//go:build wasm` line was changed to `wasm3`). Files: `runtime/*`
(asm, mem, memclr, memmove, os, preempt, rt0_wasip1, stubs, sys.go, sys.s),
`internal/cpu/cpu_wasm`, `internal/runtime/atomic/atomic_wasm.{go,s}`,
`internal/bytealg/{compare,equal,indexbyte}_wasm.s`, `reflect/asm_wasm.s`,
`math/floor_wasm.s`, `math/big/arith_wasm.s`, `os/{executable,pipe}_wasm.go`,
`os/exec/lp_wasm.go`, `runtime/cgo/asm_wasm.s`, `net/http/transport_default_wasm.go`,
`crypto/internal/fips140/{bigmod/nat_wasm.go,drbg/entropy_wasm.go}`,
`crypto/x509/root_wasm.go`, `testing/run_example_wasm.go`,
`vendor/golang.org/x/sys/cpu/cpu_wasm.go`. `internal/goarch/goarch_wasm.go` got a
hand-written companion; `zgoarch_wasm3.go` is generated.

## Bootstrap note

New `_wasm3` files in bootstrap packages (`internal/goarch`) must carry an explicit
`//go:build wasm3` line: a pre-wasm3 bootstrap toolchain treats the unknown `wasm3` tag
as false (correctly excluding the file), whereas an unknown *filename suffix* imposes no
constraint and the file would wrongly compile for the host. Non-bootstrap `_wasm3`
files rely on the filename suffix alone, matching the surrounding `_wasm.go` idiom.

## Toolchain entry points added

`cmd/dist/build.go` (`okgoarch`, `cgoEnabled`), `internal/syslist/syslist.go`,
`internal/goarch/goarch.go` (`WASM3`) + `goarch_wasm3.go`, `cmd/internal/sys/arch.go`
(`Wasm3`, `ArchWasm3`), `internal/buildcfg/cfg.go` (`gogoarchTags`),
`cmd/compile/main.go` + `cmd/link/main.go` (`archInits` / arch switch, delegating to the
wasm backend), `cmd/internal/obj/wasm/wasmobj.go` (`Linkwasm3`),
`cmd/link/internal/wasm/obj.go` (`Init` returns `ArchWasm3` for wasm3).
Regenerated: `internal/goarch/zgoarch_*.go`, `internal/platform/zosarch.go`.
`lib/wasm/go_wasip1_wasm3_exec` added (copy of the wasip1 wrapper, GOARCH-agnostic).
