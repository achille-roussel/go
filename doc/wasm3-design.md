# GOARCH=wasm3 — A WebAssembly target backed by the host garbage collector

**Status:** Exploration / design draft
**Goal:** Reduce the binary size of Go programs compiled to WebAssembly by replacing
Go-runtime subsystems with native WebAssembly 3.0 facilities.

---

## 1. Motivation

Go produces large WebAssembly binaries. A program whose `main` is empty:

```
GOOS=wasip1 GOARCH=wasm, Go 1.26.2:  1,854,088 bytes
  Code section   1,139,840  (61.5%)
  Data section     675,195  (36.4%)   pclntab + type metadata + runtime static data
  name section      35,136  ( 1.9%)   strippable
```

Even an empty program links **1,252 functions**, all runtime.

### 1.1 Where the code goes

Breakdown of the code section by subsystem (an empty program):

| Subsystem | % of code | ~bytes |
|---|---|---|
| GC mark/sweep/scan/write-barriers | 11.3% | ~129 KB |
| Allocator / heap / pages / spans | 10.1% | ~115 KB |
| Scheduler / goroutine stacks | 9.2% | ~104 KB |
| trace / metrics / prof | 5.3% | ~60 KB |
| traceback / pclntab parsing | 3.9% | ~44 KB |
| panic / defer / throw | 2.5% | ~28 KB |
| runtime "other" (maps, slices, semaphores, finalizers, print, …) | ~42% | ~478 KB |
| internal/ + app + chan + types | ~13% | ~145 KB |

### 1.2 What this target removes

The GC and allocator together are **~21.4% of the code section ≈ 244 KB ≈ 13% of the
whole binary**, and they are the only subsystems that can be deleted *wholesale*
rather than trimmed. On the source side this is roughly **22.5k lines of `src/runtime`**.

Adding stack switching removes a further chunk of the scheduler/stacks bucket and a
cross-cutting amount of per-function compiler-emitted overhead (the resume-point
machinery, the `(i32)->i32` calling convention) that does not show up cleanly in the
table because it is spread across every function.

**Framing:** the WebAssembly size problem is "Go ships a large runtime," not "Go ships
a GC." `wasm3` is not a single dramatic win; it is the largest single removal *and* an
enabler for further runtime trimming. TinyGo (~10–100 KB for comparable programs) shows
the lever is a minimal runtime; the host GC lets us delete an entire subsystem cleanly,
which is otherwise hard.

---

## 2. Goals and non-goals

### Goals
- A new `GOARCH=wasm3` that uses the **WebAssembly 3.0 GC proposal** for heap memory,
  the **stack-switching proposal** for goroutines, and the **exception-handling
  proposal** for panics.
- Substantially smaller binaries than `GOARCH=wasm`.
- Keep the *exported* Go API surface working wherever feasible, including `reflect`.

### Non-goals
- **Full Go compatibility.** `wasm3` targets a **restricted subset**, TinyGo-style.
  `unsafe.Pointer` tricks, `uintptr`-as-pointer, `cgo`, hand-written assembly that
  assumes linear memory, and runtime type construction are explicitly out of scope.
- Replacing or competing with `GOARCH=wasm`. `wasm3` is an additional, opt-in target.
- Optimal performance in v1. Several representation choices below are deliberately
  "simple first, optimize later."

---

## 3. Background: the current `GOARCH=wasm` model

The existing port is built entirely around **linear memory as the heap**:

- A Go pointer is an integer address into the single linear memory.
- The Go GC, allocator, and write barriers are compiled *into* every module.
- Goroutine stacks are linear-memory regions; the GC scans them with per-PC pointer
  bitmaps (stack maps).
- WebAssembly has no threads, so goroutine switching **unwinds the entire wasm stack**
  and re-enters via a function-id/block-id replay scheme (`PC_F`/`PC_B`,
  `wasm_pc_f_loop`). Every Go function has wasm type `(i32) -> i32`: the argument is a
  block id, the result is an "unwind now" flag, and every call site spills state and
  checks that flag.
- The module uses **zero reference types** — only `i32/i64/f32/f64`.

This model is why the runtime is large and why the compiler backend
(`cmd/compile/internal/wasm`, `cmd/internal/obj/wasm`) is complex.

---

## 4. The fundamental constraint: WebAssembly references are opaque

A WasmGC `ref` is **not** an address. Given a `(ref $T)` you **cannot**:

- read its numeric value, or convert it to/from an integer;
- perform pointer arithmetic;
- store it into linear memory;
- form a reference to a *part* of an object — there are **no interior references**.

Field access is `struct.get $T idx` / `struct.set $T idx`, where **`$T` and `idx` are
bytecode immediates** — compile-time constants. There is no "dynamic `struct.get`."
`array.get` / `array.set`, by contrast, **do** take a runtime index — arrays are the
only WasmGC aggregate with dynamic indexing.

A reference always denotes a *whole* engine-allocated object. This single property
drives the entire object model in §6–§8.

---

## 5. Approach: a three-proposal co-design

`wasm3` depends on three WebAssembly proposals that compose deliberately well:

| Proposal | Replaces | Deletes |
|---|---|---|
| **GC** (Wasm 3.0) | Go GC + allocator + write barriers | mgc*, mheap, mcache, mbitmap, mwbbuf, … |
| **Stack switching** (phase 3) | goroutine stacks + the unwind/replay scheme | stack growth, PC_F/PC_B, `(i32)->i32` ABI |
| **Exception handling** | the panic/recover unwind hack | the `RETUNWIND` return-flag protocol |

The GC proposal's heap types and the stack-switching proposal's continuations are
co-designed (continuations are GC heap objects), so they interoperate across suspension
points. Exception handling shares the tag mechanism with stack switching.

`wasm3` is registered like any GOARCH: `internal/buildcfg`, `cmd/dist/build.go`,
`internal/platform`, `cmd/internal/sys/arch.go`, `go/build`.

---

## 6. Object model

### 6.1 The common supertype

```wat
(type $go.object (sub (struct)))   ;; every Go heap object subtypes this
```

The interface `data` word, the values reachable from a suspended goroutine, and
`reflect.Value`'s container all reference `$go.object`, so downcasts are
`$go.object -> $go.T` rather than `any -> $go.T`.

### 6.2 Lowering rules

1. **Scalars** → `i32` / `i64` / `f32` / `f64` fields.
2. **Pointer `*T`** → `(ref null $go.T)` field.
3. **Composite *values*** (string, slice, nested struct, small array) → **flattened**
   into the enclosing struct's fields. Preserves Go value semantics; no extra
   allocation.
4. A value is **boxed** into its own `$go.object` subtype only when it must become a
   single `ref`: a slice element, interface `data`, or address-taken-and-escaping
   (§7).
5. **`[]T` backing** → `(array (mut <storage>))` if `T` is scalar/pointer;
   `(array (mut (ref null $go.T)))` — an **array of boxed `T`** — if `T` is composite.
6. **Multi-word values** (string/slice/interface) live **exploded as several wasm
   locals/params** when merely passed around; they become a struct *type* only when
   rule 3 flattens them or rule 4 boxes them.

### 6.3 Concrete representations

```wat
;; string: {backing, offset, length} — offset is required because substrings share backing
(type $go.bytes  (array (mut i8)))
(type $go.string (sub $go.object (struct
  (field (ref $go.bytes)) (field i32) (field i32))))

;; []int vs []Point — note the asymmetry from rule 5
(type $go.array.int (array (mut i64)))
(type $go.slice.int (sub $go.object (struct
  (field (ref $go.array.int)) (field i32) (field i32) (field i32))))   ;; backing, off, len, cap
(type $go.array.Point (array (mut (ref null $go.Point))))              ;; array of *boxed* Point

;; interface: {type descriptor, data}
(type $go.iface (sub $go.object (struct
  (field (ref $go.type)) (field (ref null $go.object)))))

;; type Point struct { X, Y float64; tag string; next *Point }
(type $go.Point (sub $go.object (struct
  (field (mut f64))                    ;; X
  (field (mut f64))                    ;; Y
  (field (mut (ref $go.bytes)))         ;; tag.backing  ┐
  (field (mut i32))                     ;; tag.offset   ├ string flattened in (rule 3)
  (field (mut i32))                     ;; tag.length   ┘
  (field (mut (ref null $go.Point))))))  ;; next
```

**Known consequence:** composite-element slices (`[]Point`, `[]string`) lose contiguous
value layout — one allocation per element, indirection on access. Accepted as the
default; a struct-of-arrays or packed escape hatch is a later optimization.

`{*type, data}` is an implementation detail, not a language guarantee, so the interface
representation may be revised; a fat pointer that must enter an interface is boxed into
a single `ref` at the conversion boundary.

---

## 7. Interior pointers and address-taken values

Because there are no interior references, `&t.field`, `&arr[i]`, and `&local` need
explicit handling. Three mechanisms, in order of preference:

1. **Elide.** If the address-taken value does not escape, the compiler never
   materializes a pointer — `x := &t.field; *x = 42` rewrites to `struct.set`. This is
   load/store forwarding; escape analysis already computes the needed fact. Today it is
   an optimization; under `wasm3` it is **mandatory for the common case**.
2. **Selective boxing.** If `&t.field` escapes, the compiler promotes *that field* to
   its own boxed `$go.object`; `t` holds a `ref` to the box, and `&t.field` is that
   `ref` — a plain one-word pointer. The field is then accessed via the box everywhere,
   so it stays uniform. Only the few escaping fields/locals pay.
3. **The `(arrayref, index)` fat pointer.** For interior pointers into *scalar*
   slices/arrays (`&intSlice[i]`), where boxing a single element is not possible. This
   is the *cheap* fat-pointer form — a `ref` plus an `i32`, no accessor vtable, because
   `array.get`/`array.set` already take a dynamic index. It is also exactly the shape
   of the slice header.

`*T` therefore stays a plain `ref $go.T` in the common case. The expensive
"every pointer is a fat pointer with getter/setter closures" representation is
explicitly *avoided* — it would tax the most common operation in Go code and would
widen the interface `data` word.

---

## 8. Goroutine model: stack switching

### 8.1 Continuations replace the unwind/replay scheme

A goroutine becomes a **continuation** — an engine-allocated, GC-managed stack. Its
locals (scalars *and* refs) live in ordinary wasm locals/operands and are scanned by
the engine as GC roots whether the goroutine is running or suspended. Consequences:

- No shadow stacks. No `(array anyref)` ref-stack. No linear-memory goroutine stacks.
- Go functions compile to **normal typed wasm functions** `(params) -> (results)`.
- The entire `PC_F`/`PC_B` resume-point state machine, `wasm_pc_f_loop`, the
  per-call spill, and the `(i32)->i32` ABI are **deleted**.

### 8.2 The engine owns stack allocation and growth

Per the proposal and the `bag-o-stacks` design note: the engine allocates continuation
stacks (`cont.new` / `stack.new`) and is solely responsible for growth (copying or
segmented — engine's choice). There is **no `stack.resize` hook**. Therefore:

- `runtime/stack.go`'s `morestack` / `newstack` / `copystack` / segmented-stack logic
  is **deleted** — it is not merely unnecessary, it is unimplementable in this model.
- The compiler **stops emitting the stack-overflow prologue check**.

Costs that move to **engine-controlled and unspecified** (not compiler concerns, but
viability items to measure on target engines):

- stack-overflow behavior and diagnostics (no Go-shaped "stack exceeds limit" fatal);
- per-goroutine memory footprint — Go's millions-of-goroutines model depends on this
  and the proposal guarantees nothing;
- `debug.SetMaxStack` becomes a no-op.

### 8.3 Continuations are one-shot; the scheduler threads a linear resource

`resume` / `switch` / `cont.bind` / `resume_throw` **destructively consume** the
continuation reference; reusing one traps. This is not a blocker — it is the standard
pattern — but it shapes the scheduler:

- A goroutine is a *chain* of one-shot continuations: each `suspend` yields a fresh
  reference for the now-suspended state.
- `g` holds "this goroutine's current continuation," **replaced on every suspend**.
  The reference must never be aliased.

Primitive mapping onto the scheduler:

| Primitive | Use |
|---|---|
| `switch` | direct goroutine→goroutine handoff (scheduler fast path) |
| `resume` / `suspend` + tags | scheduler-mediated blocking (channel ops, netpoll) |
| `resume_throw` | kill a goroutine / inject a panic at its suspension point (`goexit`) |

Switch *performance* is the engine's responsibility. Switch *frequency* is a
runtime-design lever Go owns (how preemption and blocking points are structured).

---

## 9. Memory management

- **Allocation.** `newobject` / `newarray` are no longer runtime calls; the compiler
  emits `struct.new` / `array.new` **intrinsics** directly. `malloc.go` collapses to a
  thin shim for paths that cannot be intrinsified.
- **Write barriers** (`gcWriteBarrier1..8`, `wbBufFlush`, `bulkBarrierPreWrite`) are
  **deleted** — the host GC tracks references; `struct.set` is sufficient.
- **`typedmemmove`** of a ref-containing aggregate becomes compiler-emitted field-wise
  `struct.get`/`struct.set`, not a `memmove`.
- **Stack scanning** is deleted: the engine traces each goroutine's continuation and
  the `(array …)` objects automatically. Per-PC pointer bitmaps / stack maps go away.
- **Linear memory** shrinks toward zero. Whether any linear-memory scratch region
  remains is **open question §12.6**.

---

## 10. Reflection

`reflect`'s public API is the contract; the internals are reimplemented for `wasm3`.
The implementation cannot use address arithmetic (`reflect.Value` today is
`{*abi.Type, unsafe.Pointer}` with 152 `unsafe.Pointer` uses in `value.go` alone), and
no assembly can express `struct.get` with a runtime field index.

### 10.1 Compiler-generated per-type accessors

For each reflectable **struct** type, the compiler emits accessor functions
(a `switch` over field index with static `struct.get`/`struct.set` arms) plus an
allocator. These hang off the type descriptor:

```wat
;; added to $go.type:
(field $fieldGet (ref null $accessorGet))   ;; (ref $go.object, i32) -> anyref
(field $fieldSet (ref null $accessorSet))   ;; (ref $go.object, i32, anyref) -> ()
(field $new      (ref null $allocator))     ;; () -> (ref $go.object)
```

`reflect.Value.Field(i)` becomes a `call_ref` through `v.typ_.fieldGet`. **The type
descriptor *is* the dispatch table.** Only *struct* types need accessors — slices and
arrays use dynamic `array.get`/`array.set`; scalars, pointers, maps, and channels need
none.

### 10.2 The reflectable-type set is the linker's existing analysis

`interface{}` universality is bounded: to reach `reflect.ValueOf` a value must be an
`interface{}`, which requires its `*abi.Type` descriptor to be reachable. The set of
types with reachable descriptors is exactly what `cmd/link/internal/ld/deadcode.go`
already computes — it already carries `reflectSeen` and the per-type `usedInIface`
attribute. Accessor generation extends that pass: accessors are dead-code-eliminated
the same way type descriptors, `==`/hash `alg` functions, and method wrappers already
are. Cost is **+3 funcrefs per interface-converted struct type**, and **zero** if the
program never imports `reflect`.

### 10.3 Casualties

- **Runtime type construction** — `reflect.StructOf`, `reflect.FuncOf`,
  `reflect.ArrayOf` — cannot be supported: WasmGC struct/array types are static, declared
  in the module's type section, and there is no runtime type-minting instruction.
- **Address/pointer escapes** — `Value.Addr`, `Value.CanAddr` (always false),
  `Value.Pointer`, `Value.UnsafePointer` — demand an interior pointer or
  ref-as-integer as a *return value*; unsupported.
- **Settability survives** without addressability: a `Value` that carries its place as
  `(container ref, accessor)` rather than an address can still `Set`. Most callers
  (e.g. `encoding/json` decoding) need only this.

The same "compiler emits a per-type helper, runtime dispatches by type descriptor"
mechanism also serves `reflect.New`, interface dispatch, and `reflect.Call`
trampolines.

---

## 11. Runtime / compiler / toolchain split

### Deleted outright

*Runtime (~22.5k lines + more):* `mgc*.go`, `mheap.go`, `mcache.go`, `mcentral.go`,
`mbitmap.go`, `mwbbuf.go`, `mfixalloc.go`, `mpagealloc*.go`, `mpallocbits.go`,
`mranges.go`, `mspanset.go`, `msize.go`, `arena.go`, `mgcstack.go`, `mgclimit.go`,
`mgcscavenge.go`, `mgcpacer.go`, `mgcwork.go`; `stack.go` growth machinery;
`mem_wasm.go` / `mem_js.go` / `mem_wasip1.go`; most of `mstats.go`.

*Compiler / linker / obj:* the resume-point state machine in
`cmd/compile/internal/wasm/ssa.go`; the `PC_F`/`PC_B` scheme and `funcValueOffset` in
`cmd/link/internal/wasm/asm.go`; unwind/resume wrapper generation and `RESUMEPOINT` /
`RETUNWIND` / `CALLNORESUME` in `cmd/internal/obj/wasm/wasmobj.go`; `gcWriteBarrier`
emission; `wasm_pc_f_loop` in `rt0_*_wasm.s`.

### Replaced / rewritten

`malloc.go` → allocation-intrinsic shim; `mbarrier.go` → field-wise copies, no barriers;
`proc.go` → continuation-based scheduler; `reflect` → WasmGC-native + generated
accessors; `mfinal.go` / `mcleanup.go` → dropped or host-backed; `asm_wasm.s` /
`sys_wasm.s` → largely gone; panic/defer *mechanism* → `throw` / `try_table`.

### New

*Compiler:* `wasm3` GOARCH plumbing; WasmGC/stack-switching/EH opcodes in
`cmd/internal/obj/wasm/a.out.go`; SSA lowering of Go types → wasm GC types
(`$go.object` supertype, per-type struct/array declarations); `struct.new`/`array.new`
allocation intrinsics; the normal typed-function calling convention; per-type accessor
generation; `(arrayref, index)` fat-pointer lowering; the selective field/local boxing
pass; type-section emission.

*Runtime:* allocation shim; continuation-based scheduler; `string`/`[]byte` backing as
`(array i8)`; slice/interface headers per §6.3.

### Kept (logic, not representation)

`chan.go`, `select.go`, `map.go` / `internal/runtime/maps`, semaphores/mutex,
`print.go`, panic/defer *logic*, most of the standard library.

---

## 12. Open questions

1. **Stack growth ownership** — *Resolved (§8.2):* engine-owned; `stack.go` deletes.
2. **Interface representation** — mostly settled as
   `{(ref $go.type), (ref null $go.object)}`; subject to revision since it is not a
   language guarantee.
3. **Reflectable-type set** — *Resolved (§10.2):* the linker's existing reachable-
   descriptor analysis.
4. **Finalizers / cleanups** — WasmGC has no finalizers. `runtime.SetFinalizer` and the
   cleanup API are either dropped (restricted subset) or host-backed via
   `FinalizationRegistry` (js host only). Decision pending.
5. **`string`/`[]byte`/slice/interface representations** — *Resolved (§6.3).*
6. **Linear memory: zero, or scratch region?** — *Resolved (M3.5):* a small bridge
   arena, grown on demand via `memory.grow`, with the bump pointer in wasm `global 0`.
   The arena is the only linear-memory content after M3.5; programs that never touch
   linear memory have zero pages allocated. Accessed exclusively through three
   compiler-intrinsic primitives (`runtime/wasm.WriteLinearMemory` /
   `ReadLinearMemory` / `ResetLinearMemory`); every other runtime helper that
   previously used linear memory (write1, printstring, printnum, gwrite, the
   `string`↔`[]byte` bridge) is now plain Go over the boxed prelude types.
   See `.claude/plans/retire-go-runtime-wat-static-link.md`.

---

## 13. Risks and maturity

- **Proposal maturity is asymmetric.** GC is in Wasm 3.0 and shipping broadly. Stack
  switching is phase 3 — implemented in some engines, not universal, and its surface
  is **not frozen** (the main Explainer uses `cont.new` / `(cont $ft)`; the
  `bag-o-stacks` design note uses `stack.new` / `(ref $stack)`). `wasm3` would track a
  moving target.
- **Engine-dependent viability.** Per-goroutine memory footprint and stack-overflow
  behavior are unspecified; whether `wasm3` can run a program with millions of
  goroutines depends entirely on the engine's continuation implementation. Measure
  early on target engines.
- **Binary-size tax.** Per-type reflect accessors and array-of-boxed composite slices
  add code/allocations that partially offset the GC/allocator removal. The net is
  expected positive but should be measured, not assumed.
- **Compatibility surface.** The restricted-subset decision must be documented as an
  enumerated list of unsupported APIs (`unsafe` patterns, `cgo`, `reflect.StructOf` and
  friends, `reflect.Value.Addr`/`Pointer`, `debug.SetMaxStack`, finalizers if dropped).

---

## 14. Suggested sequencing

1. Resolve open question §12.6 (linear memory) — it gates the allocator design.
2. Land `GOARCH=wasm3` plumbing + WasmGC opcodes; get an empty `main` to compile and
   run with `struct.new`-based allocation and the host GC, *keeping* the existing
   linear-memory scheduler initially.
3. Object-model lowering: §6 type representations, §7 interior-pointer handling.
4. Switch the goroutine model to continuations (§8); delete the unwind/replay scheme.
5. Exception handling for panic/recover.
6. `reflect` reimplementation + accessor generation (§10).
7. Measure against the §1 audit; enumerate the final compatibility surface.
