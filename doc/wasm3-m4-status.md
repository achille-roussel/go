# GOARCH=wasm3 M4 status — stack-switching toolchain ready, engine-blocked

## Summary

Milestone M4 of the wasm3 plan (`.claude/plans/goroutines-continuations-bag-of-stacks.md`)
asks for goroutines via WebAssembly 3.0 stack switching. The Go-side
toolchain to emit every byte the proposal requires is now in place:
opcodes, operand encoding, SSA ops, intrinsics, type-section cont type,
tag section, runtime/wasm exports. End-to-end Go programs build,
validate (`wasm-tools validate --features all`), and — for the
non-suspending subset — execute on Node V8.

The remaining phases (gopark/goready, channels, scheduler, bridge save/
restore, footprint measurement) need the engine to actually execute
suspend / resume-with-handler. **V8 December 2025 and wasmtime 44 do
not yet implement that path.** Until engine support lands, real wasm3
goroutines cannot run end-to-end.

## What landed

| Phase | Commits | Status |
|---|---|---|
| 0 — autopsy + baseline | (task #125) | done |
| 1 — opcodes + SSA op declarations | `0fb1453c5f`, `056061dc4a` | done |
| Pre-Phase-1 — pure-Go wasip1 entry (retire `rt0_wasip1_wasm3.s`) | `0e14a87afc` | done |
| 2 — minimum cont infrastructure (`RunInCont`) | `7b3cdedfe1`, `bec31906ad` | done |
| 3 — tag section + `Suspend` intrinsic | `f0b3be1d2c` | **toolchain done; engine-blocked at runtime** |
| 4 — channels/select | — | engine-blocked |
| 5 — bridge save/restore | — | engine-blocked |
| 6 — per-g footprint measurement | — | engine-blocked |
| 7 — milestone close | — | engine-blocked |

### Phases 0–3 deliverables

- **Encoder.** `cmd/internal/obj/wasm` knows every Wasm 3.0 stack-
  switching opcode (`cont.new`/`cont.bind`/`suspend`/`resume`/
  `resume_throw`/`switch` = 0xE0/E1/E2/E3/E4/E6 — note 0xE5 reserved).
  Operand encoding for `cont.new` (R_WASMTYPE typeidx), `resume`
  (R_WASMTYPE typeidx + ULEB handler-vec count), and `suspend` (ULEB
  tagidx). `TestWasm3Opcodes` pins each opcode byte.
- **WasmGC type model.** `cmd/internal/wasmgc` adds `KindCont` and the
  `(cont $T)` encoding (composite op `0x5D`). Two prelude entries:
  `TypeGoGoroutineEntry: (func)` and `TypeGoCont: (cont $TypeGoGoroutineEntry)`.
- **Linker.** `cmd/link/internal/wasm/asm3.go` writes the tag section
  (id 13) with one tag (`Wasm3TagIndexPark=0`, typed `(func)` — zero
  payload).
- **SSA & codegen.** Six SSA op placeholders declared in
  `_gen/Wasm3Ops.go`. Two of them have full codegen in `wasm3/ssa.go`
  (must live in `ssaGenValue`, NOT `ssaGenValueOnStack` — mem-typed
  values bypass the on-stack path and the default-mem branch silently
  returns otherwise):
  - `OpWasm3RunInCont`: composite emit of `ref.func $sym; cont.new
    $go.cont; resume $go.cont {}`.
  - `OpWasm3Suspend`: emit `suspend $Wasm3TagIndexPark`.
- **Pre-Phase-1 retirement.** `rt0_wasip1_wasm3.s` deleted; entry is a
  pure-Go `_rt0_wasm3_wasip1()` in `runtime/rt0_wasip1_wasm3.go`. The
  obj-level `assembleWasm3Entry` substitution that overrode the .s body
  with `call main.main` is gone. `cmd/link/internal/ld/lib.go` and
  `cmd/link/internal/wasm/asm.go` now look up the entry under its
  package-qualified name (`runtime._rt0_wasm3_wasip1`) at
  `sym.SymVerABIInternal`.
- **`runtime/wasm` surface.**
  ```go
  func RunInCont(fn func())  // Phase 2: cont.new + resume; fn must be PFUNC
  func Suspend()             // Phase 3: suspend $Wasm3TagIndexPark
  ```
  Both intrinsified at SSA. The runtime bodies trap if the intrinsic
  ever fails to fire.

### Verified end-to-end

Running on Node 25.9 with `--experimental-wasm-custom-descriptors
--experimental-wasm-wasmfx --experimental-wasm-exnref`:

| Program | Bytes | Result |
|---|---|---|
| empty `main` | 2647 | OK |
| `println("hello, world")` | 9411 | hello, world |
| `map[int]int` with delete/range/clear | 14112 | 1,10 / 2,20 / 3,30 |
| `map[string]int` | 14820 | 1 2 3 |
| string concat / substring | (varies) | golden output |
| `func body() { println("from inside cont") }; wasm.RunInCont(body)` | 9766 | before / from inside cont / after |

The bridge bump pointer, the WasmGC heap, every M3.5 invariant continues
to hold.

### NOT verified (engine-blocked)

Running a program that calls `wasm.Suspend()` inside a `wasm.RunInCont`
body — the natural "cont yields back to scheduler" pattern — fails on
Node with:

```
# Fatal error in , line 0
# unimplemented code
```

The fatal originates in V8's `GetContinuationResumeDescriptor` and
related codegen paths. Both (a) an uncaught suspend (resume with empty
handler vec) and (b) a caught suspend (resume with `(on $tag $label)`
handler) trigger the same fatal. A hand-written `.wat` fixture
exhibits the same behavior, so the failure is not specific to Go's
emission.

`wasmtime 44.0.1 (2026-04-30)` returns

```
Error: the wasm_stack_switching feature is not supported on this
compiler configuration
```

and refuses to instantiate any module declaring a continuation type.

## Engine path forward

The wasmfx-style host runtime for V8 is under active development
upstream; track [v8.dev wasm stack-switching status]. Wasmtime's
implementation is gated on the proposal stabilising (`-W
stack-switching=y` is not yet wired to compiler support as of 44.0.1).
For our purposes:

- Re-test quarterly: a `wasm.RunInCont(body)` where body calls
  `wasm.Suspend()` is the canary. If that program runs (and the
  suspend is caught by the enclosing resume), the rest of M4 unblocks.
- Until then, **GOARCH=wasm3 single-goroutine programs ship** —
  print, maps, slices, strings, the bridge — and produce the full
  binary-size win the milestone targets.

## What deferred phases need (when engine support lands)

- **Resume with handler vec encoding.** `OpWasm3Resume` currently emits
  `resume $typ 0` (empty handler vec). Real Phase 4 needs an inline
  `block` + `resume` with `(on $park $label)` + drop pattern. The
  obj-level `wasm3CFInline` machinery already exists for similar
  inline structured control flow (used by `OpWasm3WriteLinearMemory`
  byte-copy loops).
- **`g.wasm3Cont` field** holding the goroutine's current
  continuation, written at every `gopark`, read at every `goready`/
  `gogo`. Add to `g` via either a wasm3-shadowed `runtime2.go` or an
  opaque `extra unsafe.Pointer` on the shared `g`; doc/wasm3-design.md
  §8 design decision deferred to implementation.
- **`proc_wasm3.go`** — `mcall(park_m)`, `goready`, `schedule` over
  the cont-ref representation. The standard `chan.go` / `select.go`
  paths are stock and reach these hooks.
- **Bridge save/restore** at every `gopark`: snapshot wasm global 0
  into `g.bridgeBump`; restore at resume. Document the "no `gopark`
  between `WriteLinearMemory` and matching `ResetLinearMemory`" rule;
  assert in debug builds.
- **`asm_wasm3.s` retirement.** The 600-line file becomes mostly Go
  in `proc_wasm3.go`; only no-op pseudo-ops (`publicationBarrier`,
  `asminit`, `breakpoint`/`abort`) remain — and those can collapse to
  Go stubs too, finishing the "zero wasm3 .s files" trajectory.
- **Per-goroutine footprint measurement.** Spawn N ∈ {100, 1k, 10k,
  100k} parked goroutines; record `process.memoryUsage().rss /
  .heapUsed / .external`. Slope per goroutine is the binary-size /
  wasm-stack-tradeoff data point the milestone description asks for.

## References

- `.claude/plans/goroutines-continuations-bag-of-stacks.md` — the M4
  plan this status doc supersedes once M4 truly closes.
- `doc/wasm3-design.md` §8 (stack switching).
- [WebAssembly stack-switching proposal](https://github.com/WebAssembly/stack-switching).
- V8 wasm-opcodes-inl.h (December 2025) for the opcode byte map.
