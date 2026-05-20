# wasm3 Stage J — implementation progress

Live tracking for the Stage J cutover. The plan lives in
[`wasm3-stage-j-plan.md`](wasm3-stage-j-plan.md).

## Status: paused pending fat-pointer foundation

Stage J's downstream pieces (boxed `[]string` element decomposition,
runtime helper materialisation of wasmgc strings, pointer-receiver
methods on stack-allocated structs) all share the same shape: take
the address of an interior piece of a wasmgc container, hand it to a
helper, dereference there. The existing `$go.box.scalar` selective-
boxing scheme covers only `&buf[i]` for primitive `[]T`; the general
case has no uniform primitive and every helper falls back to a stub.

The decision after two cutover attempts (see history below): land a
uniform interior-pointer primitive first in a dedicated workstream,
*then* resume Stage J from a baseline where the downstream patches
are one-line intrinsic emissions rather than open-ended SSA backend
rewrites.

Design: [`wasm3-fat-pointers-design.md`](wasm3-fat-pointers-design.md).

## Current state of the wasm3 branch

| Test | Validates | Runs |
|---|---|---|
| `wasm3-empty` | ✓ | ✓ |
| `wasm3-concat` | ✓ | ✓ (`hello, world`) |
| `wasm3-map` | ✓ | ✓ (`ok`) |

The branch is back to the pre-Stage-J baseline — full test ladder
green, all linear-memory shims in place, no partial-cutover
intermediate state.

## Cutover attempts (history)

### Attempt 1 (f8e3685877, reverted in 7b1ddbed2e)

Flipped `flatPrimitiveFields(TSTRING)` to `(anyref, i64)` as a
"keystone first" change. Validation immediately failed on every
string-handling helper: the function signature declares anyref but
the body still emits i64 ops on string data. Reverted same session.

### Attempt 2 (5fad563f7e + b30deef7f8, reverted in f72db44356 + 0c92e5daa5)

Re-applied the sig flip, then patched immediate downstream
breakages: `wasm3FlatStride(TSTRING) → 0` to box `[]string`
elements, plus no-op stubs for `printstring`, `concatstrings`,
`concatbytes` whose bodies couldn't survive the cutover.

The next blocker surfaced: `OpWasm3ArrayGet` on the boxed-elem
`(array (ref $go.box.string))` rejects elem sizes > 8. Fixing it
requires making the op tuple-producing (return decomposed string
components), which is a non-trivial SSA backend rewrite — the same
shape as several other downstream blockers that all reduce to
"interior pointer + read through it".

Reverted to restore the test ladder before pivoting to the
fat-pointer foundation work.

## What to do next

1. **Implement fat pointers** per
   [`wasm3-fat-pointers-design.md`](wasm3-fat-pointers-design.md).
   Five pieces (wrapper types + SSA ops + backend lowering + `&`
   rewrite + helper migration). Each is independently testable
   because the wasm3 signature stays the same throughout.

2. **Resume Stage J** from the post-fat-pointer baseline. The
   `[]string` boxed-elem load, `runtime.printstring`
   materialisation, `runtime.gwrite` materialisation, and
   `groupReference` pointer-receiver-method paths all become
   one-line intrinsic emissions on top of the new
   `OpWasm3InteriorPtr` / `OpWasm3LoadInterior` /
   `OpWasm3StoreInterior` ops.

The Stage J plan itself
([`wasm3-stage-j-plan.md`](wasm3-stage-j-plan.md)) doesn't need
revision — only the implementation order. Pieces 1-5 of Stage J
still describe what lands at cutover time; they're just preceded by
the fat-pointer Pieces 1-5.

## Open items deferred from Stage J cutover

Recorded so they aren't lost when the cutover resumes:

- String-literal codegen (passive `(data)` segments +
  `array.new_data` per literal-use site). Independent of fat
  pointers — can land on either side of the Stage J sig flip,
  but only delivers value after both.
- Allocator deletion (`mallocgc_wasm3.go`, `newobject_wasm3.go`,
  `makeslice_wasm3.go`, the `wasm3Heap` bump arena). Depends on
  fat pointers being available so helpers that used the bump heap
  for scratch can switch to wasmgc backing via interior pointers.
- wasip1 boundary scratch (`wasip1.WithLinearBuffer` /
  `LinearPtr`). Depends on fat pointers — the materialisation
  helpers copy bytes from wasmgc backings via interior pointers
  into linear-memory scratch buffers.

All three retire linear-memory shims when they land. They're
independent commits within the eventual Stage J cutover.
