# wasm3 Stage J — implementation progress

Live tracking for the Stage J cutover. The plan lives in
[`wasm3-stage-j-plan.md`](wasm3-stage-j-plan.md).

## Execution model

Stage J is too large for a single session. It lands as a sequence of
**pieces**, each one its own session. Pieces α, β, γ are
infrastructure-only — they add new ops, helpers, and shadows without
changing the wasm signature, so the test ladder stays green at every
commit. Piece δ is the atomic world-flip: a single commit that
switches `flatPrimitiveFields(TSTRING)`, the string-literal codegen,
and the SSA decomposition to wasmgc, and deletes the linear-memory
string shims (concatstrings_wasm3, gwrite3Scratch, …).

Before δ, wasm3 strings still travel as `(i64 data, i64 len)`.
After δ, they travel as `(anyref data, i64 len)` with the data ref
pointing at a wasmgc `(array i8)`.

## Lessons from the f8e3685877 attempt (reverted in 7b1ddbed2e)

Flipping `flatPrimitiveFields(TSTRING)` in isolation breaks every
string-handling helper at the wasm validator: the function signature
declares anyref but the body still emits i64 ops. The signature
flip has to be the **last** step, not the first.

The body-side work (SSA ops, runtime shadows, wasip1 scratch) is the
bulk of Stage J. Sequencing it before the sig flip keeps each
intermediate commit testable.

## Pieces

### Piece α — compiler infrastructure for wasmgc-string ops
Status: **pending**

Add new SSA ops the wasm3 backend can emit for wasmgc-backed strings,
**unused by default**. Existing string handling continues through the
i64 path.

- `OpWasm3StringNew(data anyref, len i32) string` — `struct.new $string`
- `OpWasm3StringData(s) anyref` — `struct.get $string 0`
- `OpWasm3StringLen(s) i32` — `struct.get $string 1`
- `OpWasm3StringIndex(s, i) byte` — `struct.get $string 0; array.get_u`
- Per-package `$string` type registration via the existing
  `typeCollector` (already used for slices).

Verification: a wasm3-only Go fixture (under `cmd/compile/internal/wasm3/testdata/`)
exercises each op via a hand-crafted SSA program and `wasm-tools
validate`s the resulting `.wasm`. No user-visible test changes.

### Piece β — wasip1 scratch helper
Status: **pending**

A new runtime function `wasip1.CopyStringToLinear(s string, buf
LinearPtr) (n int)` (and the analogous `[]byte` form) materialises
the bytes of a wasmgc-backed string into a linear-memory buffer at
`buf`, returns the byte count. The body uses `OpWasm3StringIndex`
+ `i32.store8` in a loop; once SSA gets an `array.copy` between
wasmgc and linear-mem (or once we wrap a helper bridge), the loop
becomes a single intrinsic.

`LinearPtr` is a new typed alias for an `unsafe.Pointer` that points
into the wasip1 boundary arena (`var wasip1.boundaryArena [...]byte`,
a package-level `[N]byte` whose backing is linear memory on wasm3).
Allocation is bump-style within the arena for the duration of a
single syscall, reset after.

Verification: a wasm3-only test that builds a wasmgc string, copies
it to scratch, and prints the linear-mem region's first byte.
Existing wasm3-empty / wasm3-concat / wasm3-map remain unaffected
(sig unchanged).

### Piece γ — runtime string-using helpers handle wasmgc strings
Status: **pending**

Each helper that today uses `unsafe.Pointer(unsafe.StringData(s))`
or otherwise reaches into a string's linear-memory backing learns a
wasm3 path that uses `wasip1.CopyStringToLinear` + `LinearPtr`
arithmetic instead. Still triggered through the i64 path (sig
unchanged) — the helpers internally treat the i64 as a placeholder
for an anyref ref that doesn't exist yet, materialising on demand.

Helpers in scope: `printstring`, `gwrite`, `printnum` (already uses a
scratch buffer; mostly already correct), `concatstrings`,
`runtime_map{access,assign}_faststr`.

Verification: the existing test ladder (wasm3-empty, wasm3-concat,
wasm3-map) keeps passing. New tests validate that the helpers emit
the same observable output as before.

### Piece δ — the atomic flip
Status: **pending** (blocked on α, β, γ)

Single commit that lands the wasmgc cutover for strings. Includes:

- `flatPrimitiveFields(TSTRING)` and `wasm3Fields(TSTRING)` →
  `(anyref, i64)`.
- `wasmFuncTypeStorage` handles `WasmAnyref`.
- `wasm3FieldIsAnyref(TSTRING, 0) → true`.
- String literal codegen: emit per-package `(data)` segments + each
  literal-use site becomes `array.new_data` (Stage J plan Piece 1).
- `OpStringMake` decomposes to wasmgc ops on wasm3 (per Piece α).
- `OpStringPtr` SSA value gets anyref classification.
- `OpStringIndex` lowering uses `OpWasm3StringIndex` from Piece α.
- Delete `runtime/string_concat_wasm3.go`,
  `runtime/string_concat_default.go`; the canonical concatstrings
  comes back to `runtime/string.go` (rewritten to use wasmgc ops).
- Delete or simplify `runtime/printstring_wasm3.go`,
  `runtime/gwrite_wasm3.go` (replaced by Piece γ shadows, then
  switched to wasmgc-native).
- Update the test ladder targets to validate wasmgc-string output.

This commit is large by design — atomic per the Stage J plan. The
preceding α/β/γ make it a refactor (deleting the bridges) rather
than a foundational re-architecture.

## Post-δ deletions

After Piece δ, walk the deprecation list in the Stage J plan and
remove every linear-memory shim it identifies:

- `runtime/memequal_wasm3.go` — replaced by wasmgc `memequalArray`
- `runtime/makeslice_wasm3.go`, `runtime/newobject_wasm3.go`,
  `runtime/mallocgc_wasm3.go` — bump heap retired
- `internal/runtime/maps/runtime_faststr_wasm3.go` — replaced by
  wasmgc-array-based map storage (Stage J plan Piece 4)

These are separate commits, one per file, post-δ.

## Test ladder

The bring-up tests that must pass at each piece:

| Test | α | β | γ | δ |
|---|---|---|---|---|
| `wasm3-empty` | ✓ | ✓ | ✓ | ✓ |
| `wasm3-concat` (`a + b`) | ✓ | ✓ | ✓ | ✓ |
| `wasm3-map` (`map[string]int`) | ✓ | ✓ | ✓ | ✓ |
| wasmgc-string fixture (new) | ✓ | ✓ | ✓ | ✓ |

Every commit on the wasm3 branch must leave all four passing. The
sig flip in δ is what permits the wasmgc-string fixture to exercise
the production string path rather than a synthetic one.

## Active session pointer

Next session should pick up **Piece α**. Suggested first commit:
register the per-package `$string` wasmgc type via `typeCollector`
and add `OpWasm3StringNew` with its SSA → obj backend lowering.
The rest of α's ops follow the same pattern as the `$slice` type
work that landed earlier.
