# wasm3 M3 — Ref-typed SSA values: design

Date: 2026-05-16
Status: design pre-implementation
Context: M3 Stage B–C (see `doc/wasm3-m3-notes.md`)

## Problem

`struct.new $T` produces a value of wasm type `(ref $T)`. The wasm
spec rules out any conversion between ref types and integer/float
types — there is no `ref.to_i64` opcode. A wasm local declared with
type `i64` cannot hold a ref; a local declared `(ref null $T)` cannot
hold an integer. wasm3 today funnels every SSA value into one of the
GP integer registers (R0–R15, i64-wide) or one of the float registers
(F0–F31). That register set can't hold a `(ref $T)`.

To make M3 work — strings as `(ref $go.bytes)` arrays, slices
holding such backings, structs allocated via `struct.new` and accessed
via `struct.get` — refs need to be first-class SSA values with their
own storage class. This document picks the design.

## Constraints

- **64-register hard cap.** `regMask` is a `uint64`, one bit per
  register; the wasm3 backend already uses 51 of those slots (R0–R15,
  F0–F31, SP, g, SB). Whatever we add must fit.
- **Per-function wasm-local typing.** Each wasm local declared in the
  function body has a single, fixed wasm value type — `i64`, `f32`,
  `(ref null $T)`, etc. Once declared, you can't store an
  incompatibly-typed value into it.
- **WasmGC subtyping.** `(ref null any)` is the topmost ref type;
  `(ref $T)` is a subtype. Upcasting from `(ref $T)` to
  `(ref null any)` is implicit at the spec level (no opcode); the
  reverse direction requires `ref.cast`, which traps on type
  mismatch.
- **SSA regalloc is type-aware via register classes.** An SSA op's
  `regInfo.outputs[i].regs` mask determines which registers regalloc
  picks. Operations with a `gpRef` output mask get a ref register; no
  changes to the regalloc core are needed.

## The four options considered

**(A) One anyref class, 8 registers.** Add `Ref0..Ref7` register
names. Define a `gpRef` mask. Declare each ref register as a wasm
local of type `(ref null any)`. struct.new $T pushes `(ref $T)`;
`local.set N` implicitly upcasts to anyref. Reads pop anyref and
`ref.cast (ref $T)` before use.

  - Pros: smallest set of new things; no type-tracking per register;
    register reuse across types is fine (anyref local holds anything).
  - Cons: every use of a ref register emits a `ref.cast`. Casts can
    trap on type confusion (shouldn't happen in well-typed code but
    adds a runtime check). Extra bytes per use.

**(B) Per-type ref register pool.** For each distinct wasm GC type
the function uses, allocate a separate ref register pool. R0_Point,
R0_Triangle, etc. Each pool's wasm local has the specific ref type.

  - Pros: no casts; direct typed access.
  - Cons: register-name explosion (many functions use few types but
    we'd need to enumerate all possibilities); doesn't fit the
    fixed-size register-name table; regalloc would need to partition
    by type.

**(C) One ref register per SSA value.** Don't use the regalloc for
refs at all. At codegen time, allocate a fresh wasm local per ref-
producing SSA value, typed with its specific ref type. The value's
"register" is just a synthetic identifier; no sharing.

  - Pros: precise typing; no casts; no class explosion.
  - Cons: bypasses the SSA register allocator entirely for refs;
    needs separate plumbing for ref liveness; the SSA infrastructure
    assumes every value has a register or stack slot.

**(D) Treat refs as on-wasm-stack only — never assign a register.**
Mark Wasm3LoweredStructNew (and friends) as always-OnWasmStack. The
value stays on the wasm operand stack and must be consumed by the
immediately-next instruction.

  - Pros: zero new registers, zero new locals.
  - Cons: works only when the SSA happens to schedule the producer
    and consumer adjacently. Any control-flow split or call between
    producer and consumer breaks it. Doesn't generalise.

## Decision: **(A) — anyref class with 8 registers**

`gpRef` register class, `Ref0..Ref7` register names, each backed by a
wasm local of type `(ref null any)`. The bookkeeping is minimal; the
cost is a `ref.cast (ref $T)` per ref access. Casts are 3-byte
opcodes (`0xFB 0x16` + typeIdx leb128), small enough to ignore for
the M3 deliverable.

The 8-register count is a guess at the right balance. Most functions
hold one or two refs live simultaneously; eight is overkill for
typical programs and well under the 64-register cap (we'd land at
51 + 8 = 59). If a future workload runs out, raising the count is
a one-line change.

`(ref null any)` rather than `anyref` because nullability matters
for Go `nil` pointer values. A `(ref any)` local couldn't hold nil,
which would break the common `if p != nil` pattern. The cast
opcode used is `ref.cast (ref $T)` (non-null) when the SSA knows the
value is non-nil, or `ref.cast (ref null $T)` when nullability is
preserved. For the M3 starting point we use the nullable cast
universally; the SSA can prove non-nullability later as an
optimisation.

## Implementation steps

**Step 1 — register names + masks.** In `_gen/Wasm3Ops.go`:
add `"Ref0"..."Ref7"` to `regNamesWasm3`; define `gpRef = buildReg(...)`.
Define common regInfo patterns: `gpRef01` (no inputs, ref output),
`gpRef11` (ref in, ref out), `gpRefSt` (ref in for store), etc.

**Step 2 — config wiring.** `config.go` for the `"wasm3"` arch:
no new field needed in `*Config` — regalloc reads the mask from
each op's regInfo directly. The new register names appear in
`registersWasm3` (regenerated by `go generate`).

**Step 3 — wasm3Locals handles ref registers.** In
`wasm3obj.go`'s `wasm3Locals`, when a register in the Ref0..Ref7
range is `declare()`'d, emit a wasm-local declaration of type
`(ref null any)` (encoded as `0x63 0x6E` — opRefNull + heapAnyRef).
The local's index follows the same `next` counter as other ref/
spill locals.

**Step 4 — first SSA op: `Wasm3LoweredStructNew`.**
- argLength: 0 (no SSA inputs; type is in Aux).
- aux: `*types.Type` (the Go type being allocated).
- reg: `outputs: []regMask{gpRef}`.
- `rematerializeable: false`, `call: false`, no side effects beyond
  allocating.

**Step 5 — ssaGenValue for StructNew.** Look up the per-package
wasm type index via the function's typeCollector (shared with
`attachWasmType`). Emit `AStructNew` prog with
`p.From = constAddr(perPkgIdx)`. The encoder (Stage A, done)
handles AStructNew with R_WASMTYPE relocation; the linker (Stage
A, done) remaps to module-global index.

**Step 6 — newobject intrinsic.** In `ssagen.go`'s call-lowering
path, when a call to `runtime.newobject(typ)` has a statically-
known `typ` argument, replace with `Wasm3LoweredStructNew` instead
of the call. Drop the bump-allocator runtime stub for these cases.

**Step 7 — verification.** A program like
`func main() { p := new(int); *p = 42; print(*p, "\n") }` should
compile to `struct.new $boxed_int; local.set; local.get;
ref.cast; struct.set; ...; local.get; ref.cast; struct.get; print`
and produce `42`.

## What this design defers

- **`struct.get` / `struct.set` codegen.** Subsequent SSA ops
  (`Wasm3LoweredStructGet`, `Wasm3LoweredStructSet`) follow the
  same pattern as StructNew but consume a ref input. Reads emit
  the downcast `ref.cast (ref $T)` then `struct.get $T n`.
- **Array ops.** `array.new`, `array.get`, `array.set`,
  `array.len` for slice/string backings — separate stage.
- **Function refs / call_ref.** Closure dispatch — different
  register class? Or reuse gpRef? Probably reuse, with `ref.cast`
  to the specific function type at the call site.
- **The eventual SSA-level "ref type" annotation.** Today the
  output mask on each op tells regalloc to use gpRef; the SSA
  values are still typed with their Go types (`*Point`,
  `unsafe.Pointer`). When ref values flow through, e.g., a
  function parameter, we'll need either a new SSA type kind or a
  carefully-managed `Aux` annotation.

## What this design rules out (or accepts as cost)

- **No ref-typed SSA values escape into wasm linear memory.** WasmGC
  refs can't be stored in `i64` locals or in linear memory; if Go
  code stores a `*Point` into a `[]byte`-backed buffer via unsafe,
  that breaks. The restricted-subset compatibility level (see
  `doc/wasm3-design.md` §1) already accepts this.
- **`ref.cast` traps replace the design's "boxed scalar" path** in
  some cases. The cost is a runtime cast vs. a static type test.
