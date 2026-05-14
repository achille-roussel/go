# wasm3 — M2 design: the WasmGC cutover

**Status:** Design sub-pass for milestone M2 of `doc/wasm3-design.md`. Revised after a
pressure-test that found the first draft under-scoped the runtime and SSA work.
**Scope:** M2 is the atomic cutover — pointer representation, calling convention, and
allocation model change together. After M2, single-goroutine programs (empty `main`,
arithmetic, structs, plain pointers, `println`) run on a real WasmGC engine under the
host GC, with no Go GC, no Go allocator, and no scheduler.

Not in M2: goroutines / stack-switching (M4), exception handling for panic/recover
(M5), reflect (M6), driving linear memory to zero (later optimisation).

**M2 is one indivisible change.** The pressure-test confirmed the bring-up "ladder"
cannot be a *landing* sequence — a typed calling convention removes the linear-memory
Go stack, which the stock allocator and scheduler require, so the ABI, the object
model, intrinsic allocation, and the minimal runtime must all land together. The
ladder in §9 is a *testing* aid only.

---

## 1. Backend structure: fork, including the SSA layer

The first draft said "fork `cmd/compile/internal/wasm`". That package is the small
part (`Init` + `ssaGenValue`). The backend that matters is **`cmd/compile/internal/ssa`**,
and it must be forked too:

- **`cmd/compile/internal/ssa/_gen/Wasm3Ops.go` + `Wasm3.rules`** → generated
  `rewriteWasm3.go`. The wasm3 lowering rules genuinely differ: no `OpWasmLoweredWB`
  (no write barriers), new `OpWasmStructNew`/`OpWasmArrayNew`/`OpWasmStructGet`/… ops,
  ref-typed values, and `OpWasmLoweredNilCheck` cannot be `AI64Eqz` on a ref — nil
  checks become `ref.is_null`. Sharing `Wasm.rules` is not viable.
- **`cmd/compile/internal/ssa/config.go`** gets a real `"wasm3"` arm with its own
  `lowerValue`/`lowerBlock` (`rewriteValueWasm3`), and its own register/local model
  (refs are a distinct, non-spillable class — see §2).
- **`cmd/compile/internal/wasm3/`** — the forked `ssaGenValue`/`ssaGenBlock`/`Init`.
- **obj and linker layers** stay as *new files alongside* the wasm ones
  (`cmd/internal/obj/wasm/wasm3obj.go`, `cmd/link/internal/wasm/asm3.go`): they share
  the genuinely-common code (the M1 `writeOpcode` table, SLEB/ULEB encoders, the
  section-writing helpers). But note `wasm3obj.go` is **not additive** — it is a
  near-total replacement of the obj backend's control-flow handling, because
  `wasmobj.go`'s `preprocess`/`assemble` are built end-to-end around inserting
  `ARESUMEPOINT` and PC_B juggling (wasmobj.go ~lines 295–545), all of which wasm3
  deletes.

`cmd/compile/main.go`: `archInits["wasm3"]` → `wasm3.Init`. The wasm (linear-memory)
backend is left completely untouched.

## 2. Calling convention and the ABI — the hard part, named honestly

Destination: a Go function compiles to an **ordinary typed wasm function** — Go
parameters/results (lowered by the §3 object-model rules) map directly to wasm
function parameters/results; wasm multi-value returns carry Go's multiple results; no
PC_B parameter, no unwind-flag result, no return address, no linear-memory Go-stack
frame, no prologue branch-table, no stack-overflow check.

But this is **not** "just signature lowering." The collisions with the compiler's
architecture, each of which is M2 work:

- **A new ABI.** `cmd/compile/internal/abi/abiutils.go` (`ABIAnalyzeFuncType`) today
  produces `ABIParamResultInfo` with frame offsets / register indices. wasm3 needs a
  new `ABIParamResultInfo` flavour where each Go param/result is a wasm value slot.
  The number of wasm params = number of Go params after the object-model explosion
  (a `string` param → `(ref $go.bytes), i32, i32`).
- **Ref-typed SSA values are non-spillable.** This is the sharpest issue and the
  first draft missed it. A `(ref $go.T)` *cannot* be stored to a linear-memory frame
  slot — `OpStoreReg`/`OpLoadReg`/`AddrAuto` and the wasm regalloc all assume any
  value can be spilled to the stack frame with `AI64Store`. wasm3 must introduce a
  **non-spillable value class**: ref values live only in wasm locals and on the wasm
  operand stack, never in memory. Either regalloc guarantees refs never spill, or
  there is a parallel "ref local" pool. This is core M2 work, not an ABI footnote.
- **`reflectcall`.** Works today only because args live in a linear-memory frame it
  can `memmove` into; with typed params there is no such frame, and wasm cannot call
  a function with a dynamically-computed signature (`call_indirect` needs a static
  type immediate). M2 must **prove `reflectcall` is unreachable from M2's kept
  runtime**, or M2 will not link. `reflect.Call` proper is M6, but `reflectcall` also
  appears on `runtime` internal paths — this must be audited before, not during, M2.
- **`//go:wasmimport` / `//go:wasmexport`.** M0 kept these working via the *memory*
  ABI (`abi.go` `paramsToWasmFields`, `p.FrameOffset`). The new typed ABI removes
  those frame offsets, so `setupWasmImport`/`setupWasmExport` and the wrapper bodies
  in `wasm3obj.go` must be re-derived against it. This is not optional: the WASI
  syscall wrappers in §6 *are* `//go:wasmimport`s.
- **`CTXT` (closure pointer)** becomes an ordinary ref-typed parameter on closure
  calls instead of the `REG_CTXT` wasm global. `OpWasmLoweredGetClosurePtr` and every
  closure body change how they source it — a cross-cutting but mechanical rename.
- **`morestack` references in the obj layer.** `wasmobj.go` `Init` unconditionally
  looks up `runtime.morestack`/`morestack_noctxt`; `wasm3obj.go` must not, and the
  `maymorestack` debug hook must be gated.
- **`getcallerpc` / `getcallerSP`** (`OpWasmLoweredGetCallerPC/SP`) read the
  linear-memory Go-stack frame (`NAME_PARAM`, offset −8). With a typed ABI there is no
  return address on a Go stack; kept runtime (`panic`, `print`, traceback) calls
  these, so M2 must stub them (e.g. return 0 / a sentinel) and accept degraded
  tracebacks until a later milestone.
- **Stack maps.** `pgen.go`/`wasmobj.go` emit `FUNCDATA_LocalsPointerMaps`. With
  engine-scanned refs there are no Go stack maps; M2 must stop emitting them (or emit
  empty) so the linker does not choke.

`g` is a module-global singleton in M2 (one goroutine); `RET0..3` disappear (results
are real wasm results).

**Deleted in M2:** the PC_F/PC_B scheme, `ARESUMEPOINT`/`ARETUNWIND`/`ACALLNORESUME`,
`wasm_pc_f_loop`, the `(i32)->i32` wrapper generation, the per-call SP-spill /
unwind-check, `funcValueOffset` and the table-replay indexing in the linker, write
barriers, the morestack prologue.

## 3. Object model and the type section

The design-doc §6 lowering rules (the `$go.object` supertype, the six rules, the
`string`/`[]T`/interface/struct representations) are the *what*. M2 implements:

- **A type-collection pass** — walk every reachable `*types.Type`, assign a wasm type
  index, group mutually-recursive Go types into a single wasm `rec` group
  (dependency-ordered; validate the ordering algorithm against a hand-built fixture
  early — it is fiddly).
- **`$go.object`** = the open base `(sub (struct))`; every Go heap struct is
  `(sub $go.object (struct ...))`.
- **The linker emits the type section** (`asm3.go`) before the function section,
  deduplicating per-package type tables. (This is the work the plan moved out of M1;
  it now has real lowering to test against.)
- Storage-type lowering per design-doc §6: scalars → `i32/i64/f32/f64`; pointers →
  `(ref null $go.T)`; composite-value fields flatten; `[]T` backing →
  `(array (mut <storage>))` or `(array (mut (ref $go.T)))`.

## 4. Interior pointers

Per design-doc §7, in priority order: **elide** non-escaping `&x` (mandatory load/
store-forwarding); **box** an escaping `&t.field` (promote the field to its own
`$go.object`); the **`(arrayref, index)` fat pointer** for interior pointers into
scalar slices/arrays. M2 implements elide + box; the fat-pointer representation is
fixed in M2 so headers are stable, even though slices are exercised in M3.

## 5. Allocation: intrinsics, not runtime calls

`newobject`/`newarray` stop being runtime calls — only the compiler knows a Go type's
wasm type index. `walk`/`ssagen` lowers the existing `ir.Syms.Newobject` etc. calls to
new SSA ops (`OpWasmStructNew`, `OpWasmArrayNew`) carrying the type index in `Aux`;
`wasm3obj.go` encodes them with the M1 opcode bytes plus the type-index immediate (the
immediate encoding M1 deferred to here). Write barriers are deleted; `typedmemmove` of
a ref-containing aggregate becomes compiler-emitted field-wise `struct.get`/`struct.set`.

## 6. Linear memory: a stack-discipline bump allocator

Linear memory does **not** go to zero in M2 — and not only for static data. **WASI I/O
requires it**: `fd_write`, `clock_time_get`, `random_get`, `args_get` take
*linear-memory pointers*. Go data lives in GC objects (`(array i8)` backings,
structs), so every syscall needs a **GC→linear copy in, syscall, copy out**. This is
intrinsic to running on `wasip1`.

**Design:** one minimal linear memory holding (a) the module's static data section
(runtime globals, type descriptors, the data/rodata the linker already emits) and
(b) a **stack-discipline bump allocator** for syscall scratch:

- A global `linearSP`; `linAlloc(n)` bumps it, a saved mark + restore frees. No free
  list, no spans, no metadata — a few instructions, near-zero runtime cost.
- I/O wrappers: `mark := linSP; buf := linAlloc(n); copy GC→buf; syscall(buf); copy
  buf→GC; linSP = mark`. Stack discipline matches the syscall lifetime exactly.
- This is the whole of `mem_wasm3.go`. It is *not* a general heap — the host GC is the
  heap. It replaces the sbrk allocator.

## 7. The runtime: a forked bootstrap, not a "degenerate" stock one

The first draft said "keep the bootstrap, stub `gopark`/`newproc`." The pressure-test
showed that is not reachable: **stock `schedinit` cannot run and cannot even compile**
under M2's exclusions. `schedinit` calls `mallocinit()` (needs the excluded
`mheap.go`/`mcache.go`/`mpagealloc*.go`), `procresize()`, `gcinit()` (excluded
`mgc*`), `stackinit()`/`stackalloc` (in the excluded `stack.go`). So M2 needs a
**forked, gutted bootstrap**:

- **`schedinit_wasm3`** (in `proc_wasm3.go`) — a new entry that calls only a
  hand-picked minimal subset: `moduledataverify`, `goargs`, `goenvs`, `cpuinit`,
  `alginit`, the godebug parse — and *skips* `mallocinit`, `procresize`, `gcinit`,
  `stackinit`, `stkobjinit`, `mProfStackInit`, the `gcrash` stack alloc.
- **`rt0_wasip1_wasm3.s` + `asm_wasm3.s`** — `rt0_go` is rewritten for M2: set up the
  singleton `g`/`m`, call `schedinit_wasm3`, call `main.main` directly. `asm_wasm3.s`
  is a near-total rewrite — the current `gogo`/`mcall`/`systemstack`/`morestack` are
  all in the old PC_B/unwind style. With a singleton `g` and the invariant **`g` is
  always `g0`**, `systemstack(fn)` becomes a straight `fn()` call and `mcall` is
  unreachable; that invariant must be stated and asserted, because kept runtime code
  (`print`, lock paths) calls `systemstack` pervasively.
- **`proc_wasm3.go`** — the singleton `g`/`m` so `getg()` and the fields kept code
  touches resolve; `gopark`/`goready`/`newproc`/`mcall` are `unreachable`-trapping
  stubs (reaching one in single-goroutine M2 means the work belongs in M4).
- **`malloc_wasm3.go`** — `mallocgc` mostly unreachable (allocation intrinsified);
  keep only non-intrinsifiable paths.
- **`mbarrier_wasm3.go`** — field-wise `typedmemmove`, no write barriers.
- **`mem_wasm3.go`** — the §6 bump allocator + linear-memory init.
- **`os_wasm3.go`, `sys_wasm3.go`/`.s`** — WASI wrappers using the §6 copy path; these
  re-derive the `//go:wasmimport` wrappers against the new ABI (§2).

**Excluded entirely** (build tag stays `wasm`): `mgc*.go`, `mheap.go`, `mcache.go`,
`mcentral.go`, `mbitmap.go`, `mwbbuf.go`, `mfixalloc.go`, `mpagealloc*.go`,
`mpallocbits.go`, `mranges.go`, `mspanset.go`, `msize.go`, `arena.go`, `mgcsweep.go`,
`mgcscavenge.go`, `mgcpacer.go`, `mgcwork.go`, `mgcstack.go`, `mgclimit.go`,
`stack.go`'s growth machinery. A link error against any of these is the forcing
function that something still references the old model — but note that link error
will fire from `schedinit` first, which is why `schedinit` must be forked, not stubbed.

## 8. Compiler-side blockers that gate M2 *linking* (audit before coding)

These are not M4/M6 deferrals — if any is wrong, M2 does not link or does not run:

1. **Ref values must never spill to memory** (§2) — new non-spillable value class.
2. **`reflectcall` must be proven dead** in M2's kept runtime (§2).
3. **Stop emitting `FUNCDATA_LocalsPointerMaps`** / stack maps (§2).
4. **Stub `getcallerpc`/`getcallerSP`** (§2).
5. **Gate every obj-layer `morestack` reference** and `maymorestack` (§2).
6. **`schedinit` must be forked, not kept** (§7).

## 9. Bring-up order — a *testing* aid, not a landing sequence

M2 lands as one change. To test it incrementally:

1. **`.wat` spike — DONE.** `doc/wasm3-m2-spike.wat` is the hand-written target
   WasmGC module for a representative program (a self-recursive `$point` struct,
   `struct.new`/`struct.get`, a GC `(array i8)` string, the bump allocator, the
   GC→linear copy, WASI `fd_write`, typed functions, the degenerate `_start`). It
   passes `wasm-tools validate` and runs on `wasmtime run -W gc` (prints its message,
   exits 0 — exit code is `p.x+p.y-42`, so 0 confirms `struct.new`+`struct.get`
   worked). It pins the shape M2 must produce; nothing in it contradicted this
   design. Findings: the bump allocator is genuinely a single global plus a
   ~3-instruction function; `array.new_data` from a passive data segment is the
   clean lowering for a string constant; and **`wasmtime` needs `-W gc`** — so the
   `lib/wasm/go_wasip1_wasm3_exec` wrapper must add `-W gc` once M2 emits real
   WasmGC binaries (it is a harmless no-op for the M0/M1 non-GC binaries, so it can
   be added at the start of M2).
2. Backend fork compiles and `GOARCH=wasm3` still builds via the forked path *still
   emitting the old scheme* — pure-refactor checkpoint.
3. The cutover lands. Test ladder: empty `main` → arithmetic → one struct → plain
   pointer → `println`. Each rung is a test, not a separate commit.

## 10. Residual uncertainties

- The exact minimal subset `schedinit_wasm3` needs — derive empirically from link
  errors once the excluded files are out.
- Whether `g`-as-module-global vs. a threaded value is cheapest given the kept call
  sites. (The spike used no `g` at all — its degenerate entry calls `main` directly;
  the real M2 entry needs whatever the kept runtime's `getg()` sites require.)
- Rec-group dependency ordering for the type section — validate against a fixture.
  (The spike's single self-recursive `rec` group validates; multi-type dependency
  ordering is still untested.)
- Whether any kept stdlib package M2 must build pulls in `reflectcall` transitively —
  audit item #2 in §8.
