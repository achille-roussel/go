# wasm3 Stage J — implementation progress

Live tracking for the Stage J cutover. The plan lives in
[`wasm3-stage-j-plan.md`](wasm3-stage-j-plan.md).

## Checkpoints

### J.1 — TSTRING wasm signature anyref data
Status: **in progress**

Change `flatPrimitiveFields(TSTRING)` from `(i64, i64)` to
`(WasmAnyref, i64)`. Update `wasm3FieldIsAnyref` and
`wasm3NumFlatFields` to match. Update `wasm3SelectNIsAnyrefResult`
(it walks abstract Go types — already correct once wasm3FieldIsAnyref
is taught about TSTRING).

Verification:
- `GOOS=wasip1 GOARCH=wasm3 go build` of `runtime/` succeeds (toolchain rebuilds).
- Trivial tests still build; the linear-memory shims that move strings
  around now produce wasm validation errors at the data/anyref boundary.
- Collected list of breakage sites feeds J.2.

### J.2 — SSA value types for TSTRING data
Status: **pending** (blocked on J.1)

Teach `wasm3ValueType` to classify the "data" component of a string
as anyref. Sources of TSTRING-data values:
- `OpArgIntReg` for string-typed params (already handled by
  `wasm3OpArgIsRefParam`).
- `OpSelectN` from calls returning strings (already handled by
  `wasm3SelectNIsAnyrefResult`).
- `OpStringPtr` extracting the data field from a string SSA value
  (NEW — needs anyref classification).
- The data argument of `OpStringMake` (NEW — caller must produce
  an anyref value, not an i64 mallocgc ptr).

### J.3 — wasmgc string-literal data segments
Status: **pending** (blocked on J.2)

Replace the linker's current `.rodata`-style linear-memory emission of
string literals with passive `(data)` segments + per-package
`array.new_data` initialisation. A string-literal SSA value becomes
`array.new_data $bytes $segIdx 0 <len>` rather than an i64 linear-
memory address.

### J.4 — OpStringMake / OpStringPtr / OpStringLen on wasm3
Status: **pending** (blocked on J.3)

`OpStringMake` lowers to `struct.new $string` with the (anyref data,
i32 offset = 0, i32 length) fields. `OpStringPtr` lowers to
`struct.get $string 0` returning the array ref + offset packaged as
the `$go.box.scalar` interior pointer (same shape as `&buf[i]`).
`OpStringLen` lowers to `struct.get $string 2`.

### J.5 — runtime.printstring uses wasip1 scratch
Status: **pending** (blocked on J.4)

`printstring` currently does `write1(2, unsafe.Pointer(unsafe.StringData(s)), int32(len(s)))`.
After Stage J the `unsafe.StringData(s)` is an interior pointer (not
linear memory), so `write1`'s linear-memory iovec needs scratch
materialisation. Introduces the `wasip1.WithLinearBuffer` helper
described in plan Piece 5.

### J.6 — delete `runtime/string_concat_wasm3.go`
Status: **pending** (blocked on J.4)

Replace the wasm3-only `concatstrings` with the canonical body
(no `!wasm3` tag) reworked to use `array.copy` for the byte-by-byte
concatenation. Delete `string_concat_default.go` (becomes the only
copy) and `string_concat_wasm3.go`.

### J.7 — delete the bump heap
Status: **pending** (blocked on J.4, J.5, J.6)

Delete `runtime/wasm3Heap`, `runtime/mallocgc_wasm3.go`,
`runtime/newobject_wasm3.go`. Every remaining caller of `mallocgc`
on wasm3 is rewritten to a typed wasmgc allocation intrinsic.

### J.8 — wasmgc map storage
Status: **pending** (blocked on J.4, J.7)

Replace `runtime_faststr_wasm3.go` with a wasmgc-backed map: keys
`(ref (array $string))`, values typed per element. Drop the
`!wasm3` tag from `runtime_faststr.go` if the shared implementation
also works (revisit after J.4).

### J.9 — `unsafe.Pointer` discipline
Status: **pending** (final cleanup)

Restrict `unsafe.Pointer` arithmetic to wasip1-boundary code. Add a
vet check or equivalent that flags `unsafe.Add(p, n)` where `p` is
not derived from a `wasip1.LinearPtr`.

## Test ladder

The bring-up tests that must pass at each checkpoint:

| Test | J.1 | J.2 | J.3 | J.4 | J.5 | J.6 | J.7 | J.8 |
|---|---|---|---|---|---|---|---|---|
| `wasm3-empty` (empty main) | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `wasm3-print` (`print("ok\n")`) | — | — | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| `wasm3-concat` (`a + b`) | — | — | — | ✓ | ✓ | ✓ | ✓ | ✓ |
| `wasm3-map` (`map[string]int`) | — | — | — | — | — | — | — | ✓ |

`—` = expected to fail (or trap) until the dependency lands.
