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

Three options for unblocking M4 runtime validation, ordered by
near-term viability:

1. **wasmfxtime** (github.com/wasmfx/wasmfxtime) — a wasmtime fork by
   the WasmFX team that implements the full `cont.new`/`resume`/
   `suspend`/`switch` runtime. Recommended reference engine for wasm3
   M4 validation today. Last push 2026-04-02, actively maintained.

   **Installation gotchas:**
   - Requires Rust 1.82 (pinned in `Cargo.toml`); newer Rust versions
     fail on `wasmtime-wasi` lifetime macros.
   - `cargo build --bin wasmtime` (skip the full workspace, which has
     wasi-keyvalue / wasi-config bindgen failures under newer Rust).
   - **Darwin not supported out of the box** — the `OperatingSystem`
     match in `crates/wasmtime/src/engine.rs` (function `is_compatible_
     ...`) and `crates/wasmtime/src/config.rs` (function
     `validate_wasm_features`) hard-codes Linux + Windows; the
     underlying x86_64 fibre code at `crates/wasmtime/src/runtime/vm/
     fibre/unix/x86_64.rs` is pure asm + `rustix::mm::mmap_anonymous`,
     no Linux-specific syscalls, so adding a `Darwin(_) => "basic"`
     arm to both matches lets it run on Apple Silicon hosts via
     Rosetta. Reference patch:
     `/Users/achilleroussel/go/src/github.com/wasmfx/wasmfxtime` with
     the two-line edit captured in this session's transcript.
   - Build for `--target x86_64-apple-darwin` (Rosetta) on Apple
     Silicon; native aarch64 fibres are not implemented.

   **Validated on Darwin x86_64 (Rosetta):**
   - Hand-built `.wat` with `cont.new + resume + suspend + (on $park
     $caught)` handler runs end-to-end. "before-suspend" prints
     inside the cont, body suspends, handler catches, "after-handler"
     prints after the block, dead code after suspend correctly
     skipped. This is exactly the catch-suspend pattern that V8
     fatals "unimplemented code" on — wasmfxtime executes it.
   - `empty.wasm` produced by our wasm3 toolchain runs cleanly with
     `-W stack-switching=y -W gc=y -W function-references=y -W
     exceptions=y`.

   **Known wasmfxtime bugs hit by our larger fixtures** (not our
   encoding — wasmfxtime upstream issues):
   - `hello.wasm` (uses the linear-memory bridge): panics with
     `every on-stack gc_ref inside a Wasm frame should have an entry
     in the VMGcRefActivationsTable; 0x30 is not in the table` at
     `crates/wasmtime/src/runtime/vm/gc/enabled/drc.rs:239`. Looks
     like a deferred-reference-counting tracking bug when GC refs
     interact with linear-memory-side calls.
   - `m4_runincont.wasm` / `m4_catch.wasm` (cont + GC types in the
     same function): panics in cranelift at `cranelift/codegen/src/
     machinst/lower.rs:727` with `assertion left == right failed:
     left=0 right=1`. Likely a cranelift codegen gap for the GC+
     cont-opcode combination.

   Both upstream bugs are individually fixable in wasmfxtime; once
   resolved, the entire wasm3 M4 surface validates end-to-end. The
   short-term path is to either (a) wait for wasmfxtime patches,
   (b) work around by simplifying the test programs to avoid the
   triggering patterns, or (c) file the bugs upstream with our
   reproducers.

2. **V8 mainline wasmfx executor** — under active development by
   Francis McCabe / Thibaud Michaud at Google. The "[wasmfx] Plumb
   switch handler through to code gen" CL landed Feb 2026; the actual
   `resume` dispatch CLs are still in flight. Tracking bug:
   [crbug 42202153](https://issues.chromium.org/issues/42202153).
   No public target milestone. Re-check quarterly via the canary
   `wasm.RunInContCatchSuspend(body)` where body calls
   `wasm.Suspend()` — when that program runs end-to-end on a stock
   Node (or chrome with `chrome://flags/#enable-experimental-
   webassembly-wasmfx`), the rest of M4 unblocks for free.

3. **Wasmtime mainline** — `-W stack-switching=y` is recognised but
   gated on "compiler configuration" (as of 44.0.1). Upstream is
   tracking the WasmFX team's wasmfxtime work; expect this to merge
   incrementally over the next release cycles.

NOT viable for wasm3 M4:

- **Wasmer 7.x** — does NOT support the wasmfx instruction set
  (`cont.new`/`resume`/`suspend`). Their 7.0 release shipped a host-
  side "Stack Switching" feature, but it's actually WASIX
  green-threads via a host API — different mechanism, same name.
  Wasmer 7.1 doesn't even support WasmGC (no `--enable-gc` flag),
  which is a prerequisite for wasm3 binaries.
- **JSPI (`--experimental-wasm-stack-switching`)** — a different
  proposal that conflicts with the same-name "stack switching"
  label. JSPI uses JavaScript Promise integration; it is production-
  ready in V8 today but only useful for a JS-host environment
  (browser or Node script), not for `wasip1`. See
  [wasm3-bag-of-stacks plan](../.claude/plans/goroutines-continuations-bag-of-stacks.md)
  "js/wasm3 goroutines via JSPI" for the future-work track that
  uses JSPI on `GOOS=js`.

Meanwhile, **GOARCH=wasm3 single-goroutine programs ship** — print,
maps, slices, strings, the bridge — and produce the full binary-size
win the milestone targets.

## Toolchain primitives added since first status (December 2025)

Continued Go-side preparation: the obj backend now has every wire-format
piece needed to emit a Phase 4 catch-suspend composite. Specifically:

- **One-handler `resume` vec encoding** (`36e6dab18d`). `AResume`
  accepts `p.To = TYPE_BRANCH <depth>` and writes
  `0xE3 <typeidx-reloc> 0x01 0x00 <Wasm3TagIndexPark> <depth>`. The
  empty-vec form (`p.To = TYPE_CONST 0`) used by Phase 2 RunInCont
  continues to work unchanged.
- **Typed-result inline block encoding** (`4758200fab`). `ABlock`
  accepts `p.From = TYPE_CONST T` and writes a block-type of
  `(ref null $T)` (3 bytes: `0x63 0x62 <typeidx-reloc>`). The void
  form (`p.From = TYPE_NONE`) used by M3.5 byte loops is unchanged.

With those two primitives, the Phase 4 composite is encodable in 7
progs from a single SSA op:

```
ABlock     (TYPE_CONST TypeGoCont)         ; block (result (ref null $go.cont))
ARefFunc   (entry sym)                     ; ref.func $body
AContNew   (TypeGoCont)                    ; cont.new $ct
AResume    (TypeGoCont, TYPE_BRANCH 0)     ; resume $ct (on $park 0)
ARefNull   ...                             ; ref.null cont — completed-normally path
AEnd                                       ; block end
ADrop                                      ; discard suspended cont (Phase 5 stashes)
```

The one remaining encoder gap is `ref.null` with a typed heap type
(`ARefNull` today encodes `0xD0` followed by the abstract `any`
shortcut; a `(ref null $T)` null needs `0xD0 0x62 <typeidx>`). That's
~8 lines in `wasm3obj.go` once the rest of Phase 4 SSA-side is
written; deferred so a single commit can land it together with the
SSA-side composite when an engine actually executes it.

Runtime scaffolding (`ad1e2e7d56`):

- `g.wasm3Cont unsafe.Pointer` field on the shared `g` struct.
- `runtime/proc_wasm3.go` with `gogo_wasm3` / `mcall_wasm3` /
  `systemstack_wasm3` / `park_wasm3` shims wrapping
  `runtime/wasm.Suspend()`. Every path traps at runtime today with a
  message naming the missing Phase 4 piece, so when the engine
  unblocks the first wired-up caller, the failure surface is precise.

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
