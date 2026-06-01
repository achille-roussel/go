# Fat-pointer-with-class prototype

De-risk for the proposed `*T` representation on wasm3:

```
$go.ptr.<T>         polymorphic pointer base (carries class field)
  $go.ptr.array.<T>   subtype: backing array + element index
  $go.ptr.struct.<T>  subtype: container ref (field encoded in class)
  $go.ptr.ref.<T>     subtype: direct ref (for *struct/*string/*slice/...)
```

The `class` field is a `(ref $go.ref.<T>)` holding `{get, set}` funcrefs.
Polymorphic deref reads `ptr.class.get(ptr)`. Variant impls downcast
internally and operate on the concrete type.

## Files

| File           | Role                                                         |
|----------------|--------------------------------------------------------------|
| `prototype.wat` | Hand-written prelude + 2 variants (array, struct) + tests.  |
| `prototype.wasm`| Compiled output (915 bytes total).                          |
| `run.js`        | Loads on V8 (Node), runs the three tests.                   |

## Tests

| Name                | What it proves                                                                |
|---------------------|-------------------------------------------------------------------------------|
| `test-array`        | Build `[10,20,30,40,50]`, construct `&arr[2]`, polymorphic deref → `30`.      |
| `test-struct`       | Build `S{99}`, construct `&s.f0`, polymorphic deref → `99`.                   |
| `test-chan-transit` | Store a fat ptr through an `anyref` local, recover via `ref.cast`, polymorphic *store* `123`, verify the backing was updated. Proves the class dispatch works without any compile-time variant info at the deref site — i.e., the chan-transit case. |

## Reproduce

```sh
wasm-tools parse prototype.wat -o prototype.wasm
wasm-tools validate --features gc,function-references prototype.wasm
node run.js
```

Expected:
```
test-array           got=30	want=30	OK
test-struct          got=99	want=99	OK
test-chan-transit    got=123	want=123	OK
```

## What's de-risked

1. **WasmGC subtype graph compiles.** A recursive type group with
   pointer subtypes, a class type referencing function-ref types
   that themselves reference the pointer base, all in one `rec`.
   V8 8.x accepts it.
2. **Field inheritance at offset 0.** Subtypes redeclare the inherited
   `$class` field alongside their new fields; reads of `$class` on a
   supertype-typed ref work transparently on any subtype instance.
3. **Class-method dispatch via `call_ref`.** The polymorphic deref
   reads `ptr.class.get` (a typed funcref) and `call_ref`s it with
   the polymorphic ptr — no `ref.cast` at the deref site. The variant
   impl casts internally.
4. **`anyref` round-trip preserves the concrete type.** Storing a
   `(ref $go.ptr.array.i32)` into an `anyref` local and recovering it
   via `ref.cast (ref $go.ptr.i32)` keeps its real variant identity,
   so polymorphic dispatch still picks the right impl. This is the
   property that lets fat pointers travel through channel slots.

## What's NOT covered (intentional for de-risk scope)

- Reference variant (`$go.ptr.ref.<T>`) — trivial follow-on once the
  array/struct cases work; same shape with a single `(ref T)` field.
- Float / non-i32 leaf classes — same pattern, different return types.
- The runtime cost of the extra `struct.get $class` per deref.
  Suspected to be ~1 instruction in the cold path and 0 in the hot
  path (the SSA optimizer should constant-fold `class` lookup when
  the variant is statically known via the producing Op). Worth
  measuring against the existing single-call_ref scheme once
  integrated.
- Binary-size budget for the prelude type explosion (per-leaf × per-
  variant). User mentioned type erasure (`type S string` → use
  `$go.ptr.<string>`) as a mitigation; not exercised here.

## Implementation order this enables

1. Land the prelude types + per-variant runtime infra (the wasm-level
   pieces; small, isolated).
2. Migrate `new(scalar T)` to the new model — Issue 1 fix, no existing
   code to break.
3. Migrate `OpWasm3InteriorPtr` (slice/array element access).
4. Migrate `OpWasm3MakeFieldPtr` (struct field).
5. Delete the old `$go.ptr.<class>` / `$go.iptr.<class>` machinery.

Each step is independently testable.
