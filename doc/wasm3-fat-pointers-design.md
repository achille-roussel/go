# wasm3 fat pointers — design

## Goal

Define a uniform, SSA-level **interior pointer** primitive for
`GOARCH=wasm3` that lets Go code address (and read/write through) a
field inside a wasmgc struct, an element inside a wasmgc array, or a
component of a multi-field wasmgc composite. Today the wasm3 backend
has only a partial, ad-hoc scheme for this (the `$go.box.scalar`
selective boxing of `&buf[i]` for `[]byte`), and every other
"take-address-then-load/store" pattern in the Go source either
mis-compiles or falls back to a stub.

Fat pointers are the keystone primitive Stage J's downstream pieces
(boxed string-slice element load, `&runtime.localTmpBuf.field`,
wasip1 scratch handles) have been blocked on. Adopting a uniform
representation now retires several Stage J shims as side effects.

## Why now

The wasm3 cutover keeps hitting the same shape of bug:

- `[]string` element load after the J.1 sig flip: `(array (ref
  $go.box.string))` produces a ref the SSA backend cannot decompose
  into the (data, offset, length) tuple the rest of the program
  expects. Today's `OpWasm3ArrayGet` returns a single value; the SSA
  model needs a tuple-producing op that internally does
  `array.get; struct.get; struct.get; struct.get` and yields three
  decomposed sub-values.

- `runtime.printstring` materialising a wasmgc string to linear
  scratch: needs to read the bytes one at a time via `array.get_u`
  on the string's `(ref (array i8))` backing. The standard
  `for i; i < len(s); i++ { buf[i] = s[i] }` lowers to
  `OpStringPtr + OpAddPtr + OpLoad` — pointer arithmetic on a
  ref, which doesn't translate.

- `runtime.gwrite` materialising a wasmgc `[]byte`: same shape.

- `internal/runtime/maps.runtime_mapassign_faststr` taking `&group`
  for a pointer-receiver method call: a stack-allocated
  `groupReference{...}` doesn't have an SP-relative address.

- Future: `reflect.Value.Field(i)` returning an addressable view of
  a struct field; `binary.Write` casting a struct to bytes.

Every one of these is "take the address of an interior piece of a
wasmgc container, hand it to a helper, dereference it there". A
single primitive solves them all.

## The primitive

An **interior pointer** is a `(container, offset)` pair where:

- `container` is a wasmgc reference to a struct, array, or other
  composite that holds the pointee.
- `offset` is an i32 describing the pointee's position within the
  container — for an `(array T)` it's the element index; for a
  `(struct ...)` it's the field index; for nested composites it
  describes the path.

The pair is itself a wasmgc struct, allocated when an interior
pointer is materialised. The wasm3 type table grows a small set of
**interior-pointer wrapper structs**, keyed on the pointee's
*storage class*:

```
(type $go.iptr.i8     (struct (field (ref any)) (field i32)))
(type $go.iptr.i32    (struct (field (ref any)) (field i32)))
(type $go.iptr.i64    (struct (field (ref any)) (field i32)))
(type $go.iptr.f32    (struct (field (ref any)) (field i32)))
(type $go.iptr.f64    (struct (field (ref any)) (field i32)))
(type $go.iptr.ref    (struct (field (ref any)) (field i32)))
```

The first field is the container ref (typed as anyref so the same
wrapper handles every container's storage type); the second is the
offset. Reading through an interior pointer is

```
local.get $iptr
ref.cast (ref $go.iptr.<class>)
struct.get $go.iptr.<class> 0    ;; container ref
ref.cast (ref $container_type)   ;; recover the typed container
... struct.get $iptr 1            ;; offset
array.get_u $container_type      ;; or struct.get with the offset
                                 ;; as the field index
```

Writing follows the dual path. The wrapper struct is allocated once
per `&` operation (`struct.new $go.iptr.<class>`) and lives as long
as any holder of the interior pointer.

This is the same shape as the existing `$go.box.scalar`, but with a
crucial difference: today's box *contains* the value, so updates
through `&buf[i]` write to the box rather than the original `buf`.
The new wrapper *refers* to the original container, so writes
propagate through.

## SSA-level model

Three new ops:

- `OpWasm3InteriorPtr(container, offset) iptr` — materialises a
  fat pointer. `Aux` carries the storage class (the wrapper
  type index). Container is an anyref; offset is i32.
- `OpWasm3LoadInterior(iptr) value` — reads through a fat
  pointer. `Aux` carries the elem/field wasm type so the backend
  knows which `array.get*` or `struct.get*` to emit. Type-tuples
  for multi-component reads (a string-typed `OpWasm3LoadInterior`
  returns a 3-tuple decomposed downstream by `OpSelectN`).
- `OpWasm3StoreInterior(iptr, value) mem` — writes through a fat
  pointer. Same `Aux` mechanics, multi-component stores accept N
  value args.

The existing `$go.box.scalar` path is **subsumed** by
`OpWasm3InteriorPtr` with a storage-class-i8 wrapper:
`&buf[i]` for `[]byte` becomes
`OpWasm3InteriorPtr(buf.array, i)` with `Aux=storageI8`. The
helper that takes `unsafe.Pointer` reads through it with
`OpWasm3LoadInterior`.

The selective-boxing scheme stays as a fast path for `&local`
*non*-escaping cases (where load/store forwarding can elide the
pointer entirely), but the general escaping case goes through fat
pointers.

## ABI / call boundary

A fat pointer is one wasm value (a `(ref $go.iptr.<class>)`),
travelling as anyref in the call signature. Existing wasm3 ABI
infrastructure already handles anyref params and returns — the
call signature for any function taking a `*byte`, `*int32`, etc.
on wasm3 changes from i64 to anyref.

`unsafe.Pointer` is a special case: it's an opaque interior pointer
with unknown storage class. The wrapper type is
`$go.iptr.unsafe` — a struct with both a container ref AND a
runtime-tagged class byte, dispatched on at load/store time.
Slower than the typed variants but only used in code that already
trades safety for genericity (reflection, encoding helpers).

`wasip1.LinearPtr` (from the Stage J plan) is the orthogonal case:
it lives entirely in linear memory and uses i64 addresses; it's
explicitly *not* a fat pointer. The two coexist; fat pointers can
never become `LinearPtr` and vice versa without an explicit copy.

## Backend lowering

`OpWasm3InteriorPtr(container, offset)` lowers to

```
<container>
<offset>
struct.new $go.iptr.<Aux>
```

`OpWasm3LoadInterior(iptr)` lowers to (for a 1-component class):

```
<iptr>
ref.cast (ref $go.iptr.<Aux>)
struct.get $go.iptr.<Aux> 0       ;; container
ref.cast (ref $container_type)
<iptr>
ref.cast (ref $go.iptr.<Aux>)
struct.get $go.iptr.<Aux> 1       ;; offset
array.get_u $container_type
```

For multi-component (e.g. interior-pointer to a string-typed
struct field), the load emits `struct.get` N times against the
unboxed value with consecutive AuxInt field indices, producing N
wasm values on the stack — consumed by N `OpSelectN`s.

Tuple-producing op handling is the one new piece of SSA backend
infrastructure: the existing per-value-local allocation already
supports tuple ops (used by `OpStringMake`'s reverse), so this is
plumbing rather than design.

## Pieces / sequencing

The fat-pointer migration is independent of Stage J — it can land
on master before, after, or interleaved with the Stage J cutover.
But landing it **before** finishing Stage J retires the boxed-elem
load shim in a uniform way rather than ad-hoc.

### Piece 1: define the wrapper types and SSA ops

- Register the seven `$go.iptr.<class>` types in the wasmgc prelude
  (per-module, fixed indices like `TypeGoBytes` / `TypeGoObject`).
- Add `OpWasm3InteriorPtr`, `OpWasm3LoadInterior`,
  `OpWasm3StoreInterior` to `_gen/Wasm3Ops.go`.
- Regenerate `opGen.go`.

### Piece 2: backend lowering for the three ops

- `cmd/compile/internal/wasm3/ssa.go` cases for each op, emitting
  the wasm sequences above.
- Tuple-producing load handling.

### Piece 3: rewrite `&` on wasmgc storage to use the new ops

- `cmd/compile/internal/ssa/rewriteWasm3.go` (or
  `cmd/compile/internal/wasm3/Wasm3.rules`): rewrite `OpAddr` of a
  wasmgc-backed variable to `OpWasm3InteriorPtr`.
- Rewrite `OpAddPtr(OpWasm3InteriorPtr(...), n)` to
  `OpWasm3InteriorPtr(container, off+n)` so pointer arithmetic
  on a fat pointer rebases instead of erroring.

### Piece 4: switch helpers that consume `unsafe.Pointer`

- `runtime.memmove`, `runtime.memequal`, `runtime.memclr` on wasm3
  take `*go.iptr.unsafe` (or a typed variant) and dispatch on the
  storage class. Existing linear-memory shims (`memequal_wasm3.go`)
  become callable with both kinds of pointer.
- `runtime.printstring`, `runtime.gwrite` materialise via fat
  pointers + the wasip1 scratch boundary.

### Piece 5: subsume `$go.box.scalar`

- The selective-boxing scheme (doc/wasm3-design.md §7) continues
  to handle non-escaping `&local` via load/store forwarding (no
  pointer materialised), but escaping cases go through
  `OpWasm3InteriorPtr`. The `$go.box.scalar` type entry is
  retired once no remaining op references it.

## Relationship to Stage J

Fat pointers unblock these specific Stage J pieces:

- **`OpWasm3ArrayGet` for ref-typed elements**: gone. Iteration
  over `[]string` becomes `for i := range a; ... = &a[i]` where
  `&a[i]` is an `OpWasm3InteriorPtr(a.array, i)` with
  `Aux=storageRef`. Reading the string through it produces the
  decomposed tuple directly.

- **`runtime.printstring` / `runtime.gwrite` materialisation**:
  `for i; i < len(s); i++ { buf[i] = s[i] }` becomes valid Go
  again because `s[i]` lowers to `OpWasm3LoadInterior` of a
  byte-class fat pointer derived from `s`'s backing.

- **Map storage `groupReference` pointer-receiver methods**:
  `g := groupReference{data}; g.ctrls()` no longer needs `&g`
  spilled to a stack auto — value-receiver semantics are
  preserved, and any method that takes pointer-receiver
  parameters receives a fat pointer to a heap-allocated boxed
  groupReference.

Each is a downstream consumer of the new ops; once Pieces 1-3 are
in place the Stage J downstream patches collapse to one-line
intrinsic emissions per helper.

## Cost / risk

- Per-`&` allocation cost: every materialised interior pointer is
  a `struct.new`. Escape analysis already identifies non-escaping
  `&local` for the load/store-forwarding fast path; the new ops
  only fire on escaping pointers. Net cost is comparable to the
  existing `$go.box.scalar` overhead, which production code
  tolerates.

- Wrapper-type table growth: seven new prelude types. Trivial.

- Tuple-producing `OpWasm3LoadInterior` for multi-component values
  is the only genuinely new SSA pattern. The existing
  `OpStringMake` decomposition is a tuple *producer*; consuming a
  tuple via `OpSelectN` is already supported by the SSA framework.

## Open question

Whether the wrapper types should be **typed per container** (one
`$go.iptr.<containerType>` per actual container type — fewer
ref.casts at load time, more types) or **typed per storage class**
(one per wasm primitive — fewer types, more casts). The design as
written picks storage-class for table-size reasons; the trade-off
deserves a benchmark once both Stage J and fat pointers are
landable end to end.
