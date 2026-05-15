# wasm3 — M2 cutover notes: backend, linker, and runtime maps

**Status:** Working notes for the indivisible part of milestone M2 (the
"divergent cutover" of `doc/wasm3-m2-design.md` §9 step 3). The
isolatable, individually-testable parts of M2 are now built and
committed (see "Built so far" below). What remains is the interconnected
change with no working intermediate state. This file maps the three
subsystems that change — the obj backend, the linker, the runtime — so
the cutover can be executed without re-deriving the layout each time.

It complements `doc/wasm3-m2-design.md` (the design) with concrete
file:line anchors gathered from a full read of the relevant packages.

---

## Built so far (committed, isolation-tested, not yet wired)

These compile cleanly and have unit tests; `GOARCH=wasm3` still builds
and runs identically to `GOARCH=wasm` because nothing emits or calls
them yet.

- **GC SSA ops** (`33781fc89a`) — `OpWasm3StructNew`/`StructGet`/`StructSet`/
  `ArrayNew`/`ArrayGet`/`ArraySet`/`ArrayLen`/`RefNull`/`RefIsNull`/
  `RefCast`/`RefTest` in `_gen/Wasm3Ops.go` + `ssaGenValue` lowering in
  `wasm3/ssa.go`.
- **Type model + rec-group ordering** (`deef8bffca`) — `wasm3/wasmtype.go`:
  `wasmType`/`wasmField`/`wasmStorage`, the prelude (`go.object`,
  `go.bytes`, `go.string`), `recGroups` (Tarjan SCC; output order = the
  type-section emit order).
- **Type-collection walk** (`74d97f3f29`) — `typeCollector` in
  `wasm3/wasmtype.go`: `*types.Type` → `wasmTable`, following the six §6
  lowering rules; reserves struct entries before recursing.
- **Typed-ABI signature lowering** (`e9c6acbf19`) — `wasm3/wasmabi.go`:
  `loweredSignature(*types.Type) obj.WasmFuncType` using `obj.WasmRef`
  fields whose `Offset` carries the wasm type index.
- **Type-section encoder** (`fb3bf0b62a`) — `wasm3/wasmtypesec.go`:
  `wasmTable.encodeTypeSection() []byte`, validated end-to-end with
  `wasm-tools validate --features gc`.
- **Per-function `obj.WasmType` aux** (`02b1868f0a`) — `cmd/internal/obj`:
  `obj.WasmType`, parallel to `WasmImport`/`WasmExport`, holding a
  `WasmFuncType` that serializes to a `goobj.AuxWasmType` aux symbol.
  Wired into the four object-file plumbing sites (`writeAux`, `nAuxSym`,
  `genFuncInfoSyms`, `traverseAuxSyms`). Nothing attaches one yet — the
  compiler-side attachment lands with the wasm3 obj backend, because the
  *current* linker would mis-read it (its `fieldsToTypes` has no
  `WasmRef` case; see §2).
- **Shared `cmd/internal/wasmgc` package** (`91d7f25f6b`) — the
  Go-type-agnostic half of the model (`Type`/`Field`/`Storage`/`Table`,
  `PreludeTypes`, `RecGroups`, `EncodeTypeSection`) moved out of
  `cmd/compile/internal/wasm3` so the linker can import it too. The
  `*types.Type` walk (`typeCollector`) stays in the compiler.
- **`wasmgc.KindFunc`** (`a9e018c98f`) — function types modelled in the
  same `Table` as the GC types (`Params`/`Results` storage, dependency
  edges, `0x60` composite encoding), so a function can share a recursion
  group with the struct/array types it mentions.
- **`wasmgc.Table` serialization** (`845ad4092d`) — `Table.Write` /
  `ReadTable`, the object-file format each wasm3 package's compiler uses
  to hand its table to the linker for merging. Round-trip tested.
- **`typeCollector.collectSignature`** (`ee6285e4a5`) — lowers a Go
  `*types.Type` function signature to a `wasmgc.KindFunc` table entry
  (registering every GC type it references), memoized per func type.
  This is the per-function index a wasm3 function will reference.

So the compiler-side type & ABI machinery is essentially complete: the
model, the binary encoder, function types, the compiler→linker
serialization format, and the `*types.Type` collection walk for structs
*and* signatures all exist and are unit-tested. What remains is the
entangled cutover — *emitting* this from the compile pipeline,
*consuming* it in the obj/link layers, and replacing the runtime
bootstrap — which the authoring order below frames as landing together.

---

## 1. The obj backend — `cmd/internal/obj/wasm/wasmobj.go` (1498 lines)

`Linkwasm` and `Linkwasm3` currently share `instinit`/`preprocess`/
`assemble` (M0 wiring, `wasmobj.go:119-136`). The cutover adds
`wasm3obj.go` and re-points `Linkwasm3` at it. `wasm3obj.go` is a
*near-total replacement*, not an extension: `wasmobj.go`'s `preprocess`
is built end-to-end around the Go-stack-in-linear-memory model and the
PC_F/PC_B virtual-PC resume scheme.

### Keep (genuinely generic — reuse or copy)

- `writeOpcode` (`wasmobj.go:1377-1427`) — already encodes the 0xFB GC
  opcodes (M1 work). Reusable as-is.
- `writeUleb128` / `writeSleb128` / `align` (`1468-1497`, `1453-1466`).
- `appendp` (`168-194`); the `ABlock/ALoop/AIf`/`AEnd` depth tracking
  and the **branch-depth resolution pass** (`737-758`) — wasm3 wants the
  same relative-depth computation, just without the resume-table blocks.
- The `varDecls` locals-declaration emission (`1159-1170`).
- `AF32Const`/`AF64Const` encoding (`1322-1330`); the `ACall` `R_CALL`
  reloc mechanism (`1272-1299`).

### Drop entirely (the Go-stack + virtual-PC layer)

- The SP-global prologue/epilogue: `SP -= framesize` (`218-225`),
  `SP += framesize` teardown (`519-527`).
- The PC_F/PC_B numbering, `ARESUMEPOINT` → `AEnd` rewrite,
  `tableIdxs` branch table, `numResumePoints` (`274-335`).
- The morestack prologue: `G.stackguard0` linear-memory load + the
  resume-less `CALL morestack` (`337-389`).
- `ACALL`/`ACALLNORESUME` lowering: the `SP -= 8` return-address spill
  of the encoded `{PC_F, PC_B}` virtual PC, the PC_B param push, the
  PC_F-extract for indirect, the unwind `ABrIf`/`AIf` (`449-513`).
- `ARET`/`ARETUNWIND`: the `SP += 8` return-slot pop and the `0/1`
  unwind-flag result (`515-555`).
- `AJMP` lowering through `REG_PC_B` + `entryPointLoop` (`405-447`).
- `NAME_AUTO`/`NAME_PARAM` SP-relative rewriting (`558-573`); the
  `AGet TYPE_ADDR` / load / `AMOV*` lowering through `REG_SP`
  (`575-698`).
- The unwindExit/entryPointLoop/`BrTable`-on-`REG_PC_B` dispatcher
  (`701-735`); `updateLocalSP` after every call; `REG_PC_B`-as-parameter.
- `genWasmImportWrapper`/`genWasmExportWrapper` (`762-1010`) — both are
  saturated with linear-memory frame offsets; wasm3 needs GC-ABI
  versions (the WASI syscall wrappers in §3 below *are* `//go:wasmimport`s).

### New (the wasm3 obj backend)

- Functions compile to ordinary typed wasm functions: Go params/results
  → wasm params/results, sourced by `OpArg` → `local.get i`. No PC_B
  param, no SP frame, no prologue branch table, no morestack check.
- **The immediate-encoding gap M1 deferred:** the operand-encoding
  switch (`wasmobj.go:1250-1362`) has *no case* for the GC opcodes —
  `writeOpcode` emits their 0xFB byte but no immediate follows. Add
  cases for `AStructNew`/`AStructGet`/`AStructSet`/`AArrayNew`/… that
  emit the ULEB128 type-index (and, for the struct field accessors, the
  field-index) immediate. The SSA ops built in `33781fc89a` carry the
  `*types.Type` in `Aux` and the field index in `AuxInt`; the obj
  backend resolves the type to its wasm index. Model the inline path on
  the `writeSleb128` case at `1320`, or the extern-reloc path at
  `1310-1317` if type indices need linker fixup (they do — see §2).
- `Init` keeps the `morestack` lookups out (no prologue) and gates the
  `maymorestack` hook off.
- Ref-typed values must never spill to a linear-memory frame slot
  (`doc/wasm3-m2-design.md` §8 blocker 1) — this constrains the SSA
  backend's regalloc/`OpStoreReg`, not just the obj layer.

---

## 2. The linker — `cmd/link/internal/wasm/{asm.go (712 lines), obj.go}`

`obj.go:28-31` already selects `sys.ArchWasm3` when `GOARCH==wasm3`, but
`asm.go` keys nothing on GOARCH — so `asmb`/`asmb2` run unchanged today.
The cutover adds `asm3.go` and conditionally re-points
`theArch.Asmb`/`Asmb2`/`AssignAddress` in `obj.go:Init()`.

### Module section order (`asmb2`, `asm.go:146-264`)

magic+version → type(1) → import(2) → function(3) → table(4) →
memory(5) → global(6) → export(7) → element(9) → code(10) → data(11) →
custom(producers) → custom(name).

### Type section — the extension point

`writeTypeSec` (`asm.go:297-317`) today emits a flat `vec` of `0x60`
functypes; index 0 is seeded as the `(i32)->(i32)` Go-function signature
(`asm.go:149-155`). `lookupType` (`asm.go:266-274`) interns by
`bytes.Equal`.

For wasm3 the type section must also carry the GC struct/array types.
The encoder is already written (`wasm3/wasmtypesec.go`,
`encodeTypeSection`) and validated. The open architecture question is
**ownership**: per-package compilers each build a `wasmTable`, but the
linker merges packages, so the *merged, deduplicated* type table — and
therefore the final type indices — must be resolved at link time. Two
viable shapes:

1. **Linker owns the table.** The compiler emits each function's
   `obj.WasmFuncType` with `WasmRef` fields referencing *Go types*
   (e.g. by a per-package type symbol); the linker reconstructs the
   merged `wasmTable`, dedups, runs `recGroups`, and calls
   `encodeTypeSection`. This needs the `wasmtype.go` model duplicated or
   shared into `cmd/link/internal/wasm`.
2. **Compiler emits encoded fragments, linker concatenates + dedups.**
   Harder to dedup correctly across packages.

Shape 1 is cleaner. It implies a small refactor so the `wasmType` model
+ `recGroups` + `encodeTypeSection` are importable by the linker (move
to a shared `cmd/internal/...` package, or duplicate — the linker
already duplicates `valueType` constants).

### Per-function type index (`asm.go:224-236`)

The linker reads `ldr.WasmTypeSym(fn)` per function, `o.Read`s the
`obj.WasmFuncType`, converts via `fieldsToTypes` (`asm.go:694-711` —
**must be extended** to handle `WasmRef` → a `0x63/0x64` heaptype) and
interns via `lookupType`. Today only `//go:wasmimport`/`//go:wasmexport`
functions carry a `WasmTypeSym`; for wasm3 *every* function carries one
(see §1 / task #18 — generalize `objfile.go`'s `AuxWasmType` emission).
The function section (`writeFunctionSec`, `asm.go:341-350`) then emits
each function's type index.

### Drop (the PC_F replay scheme)

- `funcValueOffset = 0x1000` (`asm.go:44`) and the `assignAddress`
  `s.Value = (funcValueOffset+va/MINFUNC)<<16` encoding (`asm.go:100-119`).
- `writeTableSec` (`asm.go:355-365`) — the `funcref` replay table.
- `writeElementSec` (`asm.go:481-499`) — the table-init segment.
- The `R_CALL` reloc rewrite (`asm.go:212-213`) stays in spirit but
  computes a *direct* function index, since wasm3 uses real `call` /
  `call_ref`, not `CallIndirect(PC_F)`.

### Reuse

`writeSecHeader`/`writeSecSize` (`276-288`), `writeUleb128`/`writeSleb128`
(`649-692`), `writeName` (`644`), `writeI32Const`/`writeI64Const`
(`634-642`). The entry symbol is already derived as `_rt0_<GOARCH>_wasip1`
(`asm.go:431`) → `_rt0_wasm3_wasip1` with no code change.

---

## 3. The runtime — a forked, gutted bootstrap

The fork already has `_wasm3.s`/`_wasm3.go` copies of every wasm runtime
file, but they are byte-identical to the `wasm` versions (only
`rt0_wasip1_wasm3.s` differs, in the entry symbol name). The cutover
diverges them.

### `rt0_go` and the bootstrap (`asm_wasm.s` / `asm_wasm3.s:10-26`)

`runtime·rt0_go` sets up `g0`/`m0`, then
`check → osinit → schedinit → newproc(&runtime.main) → mstart`. On
wasip1 `runtime·args` is *not* called (arg/env setup is in `goenvs`).
`rt0_wasip1_wasm3.s` sets SP to the top of `runtime.wasmStack` and calls
`rt0_go`. For wasm3 the entry is rewritten: set up the singleton `g`/`m`,
call `schedinit_wasm3`, call `main.main` directly. `wasm_pc_f_loop` and
the whole PC_F/PC_B `CallIndirect` resume trampoline (`asm_wasm.s:500-591`,
plus `gogo`/`mcall`/`systemstack`/`morestack` at `:35-271`) are deleted —
they are all in the old unwind/replay style. With a singleton `g` that
is always `g0`, `systemstack(fn)` becomes a straight `fn()` call.

### `schedinit` (`runtime/proc.go:837-960`) — what `schedinit_wasm3` keeps

The full call tree, in order: ~14 `lockInit` calls; `lockVerifyMSize`;
`sched.midle.init`; `worldStopped`; `getGodebugEarly`; `ticks.init`;
**`moduledataverify`**; **`stackinit`**; `randinit`; **`mallocinit`**;
**`cpuinit`**; `maps.AlgInit`; `mcommoninit`; `modulesinit`;
`typelinksinit`; `itabsinit`; **`stkobjinit`**; `sigsave`; `goargs`;
**`goenvs`**; `secure`; `checkfds`; `parseRuntimeDebugVars`;
`finishDebugVarsSetup`; **`gcinit`**; `gcrash.stack = stackalloc(...)`;
`mProfStackInit`; `defaultGOMAXPROCSInit`; **`procresize`**;
`worldStarted`.

Under M2's file exclusions, the **bold** calls reach excluded files
(`mallocinit`→`mheap`/`mcache`/`mpagealloc`; `gcinit`→`mgc*`;
`stackinit`/`stackalloc`→`stack.go`; `procresize`→scheduler) — so stock
`schedinit` cannot even compile. `schedinit_wasm3` calls only a
hand-picked safe subset (`moduledataverify`, `goargs`, `goenvs`,
`cpuinit`, `alginit`, the godebug parse) and skips the rest. Derive the
exact minimal set empirically from link errors once the excluded files
are out (`doc/wasm3-m2-design.md` §10).

### WASI syscalls (`runtime/os_wasip1.go`, shared by wasm and wasm3)

`//go:wasmimport wasi_snapshot_preview1` wrappers: `proc_exit`,
`args_get`/`args_sizes_get`, `clock_time_get`, `environ_get`/
`environ_sizes_get`, `fd_write`, `random_get`, `poll_oneoff`. They take
**linear-memory pointers** (`uintptr32`), so wasm3 needs the §6
GC→linear copy path: `mark := linSP; buf := linAlloc(n); copy GC→buf;
syscall(buf); copy buf→GC; linSP = mark`. The `//go:wasmimport` wrappers
must be re-derived against the typed ABI (§1, `setupWasmImport`).

### New `//go:build wasm3` files

`schedinit_wasm3` (in `proc_wasm3.go`), the gutted `rt0_wasip1_wasm3.s` +
`asm_wasm3.s`, `proc_wasm3.go` (singleton `g`/`m`; `gopark`/`goready`/
`newproc`/`mcall` as `unreachable`-trapping stubs), `malloc_wasm3.go`
(`mallocgc` mostly unreachable — allocation is intrinsified to
`struct.new`), `mbarrier_wasm3.go` (field-wise `typedmemmove`, no write
barriers), `mem_wasm3.go` (the stack-discipline bump allocator: a global
`linSP`, `linAlloc(n)` bumps it, a saved mark restores). Excluded
entirely (build tag stays `wasm`): `mgc*.go`, `mheap.go`, `mcache.go`,
`mcentral.go`, `mbitmap.go`, `mwbbuf.go`, `mfixalloc.go`,
`mpagealloc*.go`, `mpallocbits.go`, `mranges.go`, `mspanset.go`,
`msize.go`, `arena.go`, `mgcsweep.go`, `mgcscavenge.go`, `mgcpacer.go`,
`mgcwork.go`, `mgcstack.go`, `mgclimit.go`, and `stack.go`'s growth
machinery.

---

## 4. Remaining cutover order

The pieces below have no working intermediate state — they land
together — but this is a sensible authoring order:

1. **Generalize `objfile.go` `AuxWasmType`** to every wasm3 function —
   ✅ DONE (`02b1868f0a`): the obj-side plumbing (`obj.WasmType` + the
   4 serialization sites) is committed. REMAINING: the compiler-side
   attachment — call `loweredSignature` during wasm3 codegen and set
   `fn.LSym.Func().WasmType`. This is *not* independently committable:
   the current linker's `asm.go:228` would read the new aux and feed it
   to `fieldsToTypes`, which has no `WasmRef` case, so the attachment
   must land together with the linker reroute (step 2). (task #18)
2. **Make the type model linker-importable** — ✅ DONE: extracted to
   `cmd/internal/wasmgc` (`91d7f25f6b`), with function types
   (`a9e018c98f`) and a compiler→linker serialization format
   (`845ad4092d`). REMAINING: write `asm3.go` — the linker reads each
   wasm3 package's serialized `wasmgc.Table` (`wasmgc.ReadTable`) plus
   each function's `collectSignature` index, concatenates the tables
   (sharing the one prelude) and remaps the per-package indices, emits
   the merged GC+func type section via `wasmgc.EncodeTypeSection`, drops
   the table/element sections, and direct-indexes calls; reroute
   `obj.go Init`. The compiler still has to *emit* the per-package table
   aux + per-function index — that emission is part of step 1's
   remainder and the obj backend (step 3), which is why asm3.go cannot
   be finished and verified independently. (task #19)
3. **`wasm3obj.go`** — the obj backend per §1, including the GC-opcode
   immediate encoding. (task #21)
4. **Diverge `Wasm3.rules` / `Wasm3Ops.go`** — lower `newobject`/
   `newarray` to `OpWasm3StructNew`/`ArrayNew`; pointers to refs; nil
   checks to `ref.is_null`; delete write barriers; `OpArg`/result to
   typed locals. (task #20 + the rules half of the SSA fork)
5. **The runtime fork** per §3 — `schedinit_wasm3`, the asm rewrites,
   `proc/malloc/mbarrier/mem_wasm3.go`, the file exclusions. (task #22)
6. **The 6 compiler-side blocker fixes** (`doc/wasm3-m2-design.md` §8):
   non-spillable ref class, keep `mfinal.go` excluded, suppress
   `FUNCDATA_LocalsPointerMaps`, stub `getcallerpc/SP`, gate obj-layer
   morestack refs, fork `schedinit`. (task #23)
7. **Wire + walk the bring-up ladder** — re-point `Linkwasm3`, reroute
   the linker `Init`, add `-W gc` to `lib/wasm/go_wasip1_wasm3_exec`;
   test empty `main` → arithmetic → struct → pointer → `println` on
   `wasmtime -W gc`. (task #24)

---

## 5. Stage C — the codegen ladder (in progress)

Stages A and B are done: an empty `main` links into a valid module
(`wasm-tools validate --features gc`) and runs to exit 0 on
`wasmtime -W gc`. Stage C climbs from "empty main runs" to real
codegen, rung by rung, on the `wasm3-m2-cutover` branch.

### Rung 0 — prog-stream walker ✅ DONE (`53a7e52375`)

`assemble3` no longer hardcodes the `()->()` stub. `encodeWasm3Body`
walks the `obj.Prog` stream and encodes it; this rung handles the
trivial body — the no-op progs threaded through every function
(TEXT/FUNCDATA/PCDATA/NOP/RESUMEPOINT) and a terminating `RET`, which
emits a real `return`. Any unsupported prog makes `encodeWasm3Body`
return `ok=false` and `assemble3` falls back to the degenerate stub,
so an empty `main` stays buildable while later rungs add encodings.
`main.main` is now genuinely walked (`(func $main.main return)`), not
a hardcoded constant.

### The arithmetic rung — ✅ DONE (`52085564ce`, `fbdc85ccef`)

A leaf arithmetic function now compiles to a genuine typed WasmGC body
and runs end to end:

	//go:noinline
	func add(a, b int) int { return a + b }
	func main() { add(2, 3) }

	(func $main.add (param i64 i64) (result i64)
	  local.get 0  local.get 1  i64.add  return)
	(func $main.main (local i64)
	  i64.const 2  i64.const 3  call $main.add  local.set 0  return)

builds, validates with `wasm-tools --features gc`, and runs to exit 0
on `wasmtime -W gc`. C.2a (`52085564ce`) flipped the SSA backend to the
register ABI; C.2b (`fbdc85ccef`) graduated `encodeWasm3Body` to a real
encoder. The scoping notes below are kept as the record of how it was
mapped. The remaining bring-up rungs (linear-memory frame access:
`Get SP`, loads, stores; branches; indirect calls; `0xFB` GC opcodes)
reuse this same encoder.

### The arithmetic rung — scoping

The wasm/wasm3 backend uses the **memory ABI**: arguments and results
travel through the Go stack in linear memory, not wasm params/results.
`Wasm3.rules` has no `OpArg`-to-register lowering — `(Addr {sym} base)
=> (LoweredAddr ...)` and args are reached with `I64Load` off `SP`.
The morestack prologue and the `(i32)->i32` PC_B block-id ABI are
*not* in the SSA backend or `ssagen`; they are synthesized by
`wasmobj.go`'s `preprocess`, and `preprocess3` already omits them. So
the divergence for typed args/results is purely: arrive-as-locals and
return-on-stack, plus the encoder.

This rung is one interlocking change with no working intermediate
state *within itself*, but it decomposes into two independently
landable commits:

- **C.1 — typed signatures + unreachable stub.** ✅ DONE
  (`9ffa8cd069`). Added `ssagen.ArchInfo.PrepareFunc`, a per-function
  hook called after `genssa`; the wasm3 backend wires it to
  `attachWasmType`, which lowers an all-scalar Go signature with
  `loweredSignature` and attaches it as an `obj.WasmType` aux —
  `asm3.go` already consumes that aux. `preprocess3` now also
  materializes the `WasmType` aux symbol. The `encodeWasm3Body`
  fallback stub became `localcount=0; unreachable; end`
  (`0x00 0x00 0x0b`): `unreachable` validates against *any* declared
  signature. `encodeWasm3Body` also declines any function whose
  declared signature has results, since the trivial walked body
  leaves the stack empty. Verified: empty main still runs; a
  `//go:wasmexport` scalar function is now declared and validates
  with its real `(i32,i32)->i32` type. Reference-typed signatures
  (pointers/strings/slices/structs) are still left as `()->()` — they
  need the per-package `wasmgc.Table` emission, which is the larger
  §4 step-1/step-2 remainder and is deferred past the arithmetic rung.

- **C.2 — the arithmetic body.** A probe (made and reverted; the
  branch stays at the C.1 state) turned the rough plan into a precise
  blocker chain. C.2 is "give wasm3 a register ABI whose registers are
  wasm locals", and it is genuinely *one* interlocking change — every
  step below is required before the compiler stops crashing on a
  wasm3 program, so there is no safe partial commit:

    1. **The register ABI is gated by a GOEXPERIMENT.** `abiForFunc`
       only hands a function `ABI1` when `buildcfg.Experiment.RegabiArgs`
       is set; `internal/buildcfg/exp.go` enables it only for
       amd64/arm64/loong64/ppc64*/riscv64/s390x. Add `wasm3` to that
       `regabiSupported` switch (it also grants `RegabiWrappers`, which
       `RegabiArgs` requires). Without this, setting the param-reg
       lists has no effect — `ABI1` is still computed but never chosen.
    2. **`config.go`** — in the `wasm3` case set `c.intParamRegs =
       paramIntRegWasm3` / `c.floatParamRegs = paramFloatRegWasm3`.
    3. **`_gen/Wasm3Ops.go` — two changes, then regenerate `opGen.go`
       (`go run -C=_gen .`):**
         a. Add `ParamIntRegNames` / `ParamFloatRegNames` to the
            `Wasm3` arch entry ("R0…R15" / "F0…F31"). This makes the
            generator emit `paramIntRegWasm3` / `paramFloatRegWasm3`
            (today both are `[]int8(nil)`).
         b. Change the call ops (`LoweredStaticCall`, `LoweredTailCall`,
            `LoweredClosureCall`, `LoweredInterCall`,
            `LoweredTailCallInter`) from `argLength: 1` to
            `argLength: -1`. They are a copy of the memory-ABI wasm
            ops; `expand_calls` does `v.AddArgs(…)` to append the
            register-arg values as explicit args to the call value,
            which requires a variadic op (cf. AMD64 `CALLstatic`,
            `argLength: -1`). With `argLength: 1` the appended args
            corrupt the value.
    4. **`wasm3/ssa.go` — `OpArgIntReg`/`OpArgFloatReg`.** These appear
       once the register ABI is on. Add a `ssaGenValue` case: a no-op
       for codegen (the value is already in its register = wasm local),
       just `ssagen.CheckArgReg(v)` and clear `v.Block.Func.RegArgs` —
       unlike the register arches there is no morestack entry wrapper
       to feed spill info into.
    5. **`wasm3/ssa.go` — `BlockRet` vs. the `OnWasmStack` optimization.**
       `regalloc.go` marks single-use non-generic wasm values as
       `OnWasmStack` (their value is deferred and regenerated inline at
       the consumer). The result value (e.g. `I64Add`) feeding the
       `OpMakeResult` gets marked, but `OpMakeResult` is codegen-nothing,
       so the value is never consumed → `s.OnWasmStackSkipped` is left
       non-zero → `panic("wasm: bad stack")`. Fix: `BlockRet` must walk
       `b.Controls[0]` (the `OpMakeResult`) and `getValue*` each result
       arg onto the wasm stack (width per `scalarPrim`), then `RET`.
    6. **`wasm3/ssa.go` — the call site (the register↔stack-machine
       bridge).** Go's register ABI passes args "in registers"
       implicitly; wasm's `call` takes them from the operand stack.
       (This is the original reason GOARCH=wasm chose the memory ABI.)
       After step 3b the register args are explicit `v.Args` of the
       call op: `ssaGenValue` for the call ops must `getValue*` each
       arg onto the wasm stack before `ACALL`, then `setReg` each
       result register afterward (in reverse — wasm leaves the last
       result on top of the stack).
    7. **`encodeWasm3Body`** — graduate from the trivial walk to a real
       (still SP-frame-free) encoder: local declarations (params are
       locals `0..nparams-1`, spill regs follow — no PC_B param, no
       local-SP, unlike `assemble`'s map), `Get`/`Set`/`Tee` of
       registers → `local.get/set/tee`, `I64Const` + the arithmetic
       opcodes, and a typed `return` leaving the result on the stack.

  Verify with `add` called from `main`, observed via the `os.Exit`
  exit code on `wasmtime -W gc`. Likely further whack-a-mole once the
  ABI is on (other `OpArgIntReg`/`MakeResult`-shaped breakage, the
  call ops' `regInfo`, `checkLower`); budget for it.

Then the struct and pointer rungs reuse the C.2 encoder, adding the
`0xFB` GC-opcode immediates and `struct.new`/`struct.get` lowering
(§4 steps 3–4), and the deferred reference-typed-signature work brings
in the per-package `wasmgc.Table` (§4 steps 1–2).
