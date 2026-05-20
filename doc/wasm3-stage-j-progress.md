# wasm3 Stage J — implementation progress

Live tracking for the Stage J cutover. The plan lives in
[`wasm3-stage-j-plan.md`](wasm3-stage-j-plan.md).

## Current state of the wasm3 branch

| Test | Validates | Runs |
|---|---|---|
| `wasm3-empty` | ✓ | ✓ |
| `wasm3-concat` | ✗ | — |
| `wasm3-map` | ✗ | — |

The cutover is in progress. The signature flip has landed and
incremental piece-by-piece work to fix downstream breakage is
ongoing. The test ladder regressed deliberately — each broken test
is now a known piece of follow-up work, not a surprise.

## Landed commits

| Commit | Subject |
|---|---|
| 5fad563f7e | cmd/compile: Stage J.1 — TSTRING wasm signature carries data as anyref |
| (this commit) | runtime,cmd/compile: stub string helpers + box []string elem on wasm3 |

## What changed in this commit

After the J.1 sig flip, three downstream breakages had to be
patched to keep `wasm3-empty` building and to land partial progress:

- **`wasm3FlatStride(TSTRING) → 0`**: the slice-of-string backing
  used to be `(array i64)` with stride 2, holding the old
  `(i64 data, i64 len)` headers inline. After J.1 strings are
  `(anyref data, i64 len)`, mixed types that can't share an i64
  backing. Box each element via `(array (ref $go.box.string))`.
  Per-element allocation overhead, revisited in Piece γ/δ.

- **`runtime.printstring` no-op stub** (`runtime/printstring_wasm3.go`):
  the prior body did `write1(2, unsafe.Pointer(unsafe.StringData(s)),
  int32(len(s)))`, but after J.1 `unsafe.StringData(s)` is an anyref
  ref that `write1` (linear-memory i64) can't consume. The
  materialisation path needs SSA support for byte-level indexing on a
  wasmgc-backed string, not yet wired up.

- **`runtime.concatstrings` / `runtime.concatbytes` no-op stubs**
  (`runtime/string_concat_wasm3.go`, split from
  `string_concat_default.go`): the prior bodies iterated a
  `[]string` and used `unsafe.StringData` / `memmove` to copy
  bytes — both broken on wasmgc strings, plus `[]string`'s backing
  is now boxed (`(array (ref $go.box.string))`) which the SSA
  backend's `OpWasm3ArrayGet` doesn't yet decode for ref-typed
  elements.

## Outstanding work

Functions known to break on `wasm3-concat` / `wasm3-map` after these
patches; each requires the same general fix (teach the SSA backend to
handle wasmgc-string-shaped values + materialise to linear-mem
scratch at wasip1 boundaries):

- `cmd/compile/internal/wasm3.ssaGenValue` for `OpWasm3ArrayGet`
  with ref-typed elements (currently fatalfs on elem sizes > 8).
- Several runtime helpers downstream (the func-6 / func-3 validation
  errors in wasm3-concat / wasm3-map respectively — both touch
  string-typed values whose decomposition now produces anyrefs the
  body can't load through).

This is the open-ended portion of the Stage J cutover. Each fix
exposes one or two more.

## Realistic timeline

Earlier in this session I sketched a four-piece (α/β/γ/δ)
sequencing that supposedly let the test ladder stay green between
pieces. Investigation showed this was wishful — the sig flip is
load-bearing and *must* land first, then every downstream caller
gets patched one by one.

The honest estimate based on this session's progress:

- Sig flip and immediate fan-out (this commit): **landed**.
- Patching every string-using helper (concatbytes, concatstring*,
  printstring, gwrite, panic message formatters, map faststr keys,
  string equality, …): **probably a week of focused work** to get
  back to the test ladder passing.
- Literal-codegen change (string literals as wasmgc `(data)`
  segments + `array.new_data`): **another major piece, multi-day**.
- Allocator cutover (delete bump heap, all mallocgc call sites to
  typed `array.new` / `struct.new`): **another major piece**.

So the whole of Stage J is realistically a **multi-week dedicated
project**, sequenced as continuous progress from this committed
intermediate state. Sessions in between will leave the branch in
known-partial-cutover form; the test ladder doesn't fully recover
until Piece γ wraps up.

## Active session pointer

Next session picks up from the current commit. The first concrete
fix is extending `cmd/compile/internal/wasm3/ssa.go`'s
`OpWasm3ArrayGet` case to handle ref-typed elements (the
`(array (ref $go.box.string))` case the boxing change introduced).
That unblocks the runtime helpers' string-iteration paths.

After that, walk the wasm3-concat / wasm3-map func-N validation
errors in order, fixing each helper's body to materialise via wasmgc
ops where it used to do linear-memory pointer arithmetic.
