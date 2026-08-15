# Plan: vectorizing slices package operations

Status: draft plan, not for submission.
Related: golang/go#80857 (min/max reduction loop recognition),
prototype at branch `simd-minmax-idiom`.

## Background and division of labor

The discussion on #80857 drew a line between two mechanisms:

- **Loop-shaped operations** are idiomatically written inline (min/max
  reductions), so a function-level fast path cannot reach them. These
  are the domain of compiler pattern recognition, kept to a single
  memclr-style range form per operation.
- **Call-shaped operations** are always invoked through the `slices`
  API (`Sort`, `IsSorted`, `Reverse`, `Index`, ...). Nobody writes a
  sort inline. For these, the generic function dispatches internally
  to specialized code; the compiler is not involved.

This document plans the second track: library-internal dispatch in the
`slices` package. It is independent of the outcome of #80857, needs no
compiler review, and can proceed function by function.

## Shared infrastructure

### Kind dispatch

A type switch on `any(x)` inside a generic function misses named types
(`type Temps []int32`, `type Celsius int32`), which the `~[]E`
constraints of the `slices` API explicitly permit. The correct
std-internal idiom is:

```go
switch abi.TypeFor[E]().Kind() {
case abi.Int32:
	fast32(unsafe.Slice((*int32)(unsafe.Pointer(unsafe.SliceData(x))), len(x)))
...
```

`internal/abi.TypeFor[E]().Kind()` dispatches on the element kind
(covering all named variants, exactly as compiler-side kind dispatch
would), and `unsafe.Slice` + `unsafe.SliceData` reinterprets the slice
to the sized type. This helper is written once and reused by every
phase.

### Kernel package

New package `internal/simdslices` holding the archsimd kernels, gated
`//go:build goexperiment.simd && amd64` with stub files for other
configurations. Unlike the min/max kernels (which live in `runtime`
because the compiler emits calls to them), nothing here touches the
runtime: `slices` imports the package directly. This eliminates the
entire pkgspecial / `_builtin` / sanitizer-interaction surface — the
kernels are ordinary library code and instrumented builds instrument
them normally.

## Phase 0: bytealg routing (no GOEXPERIMENT, every architecture)

Two dispatches route to `internal/bytealg`, which is already
hand-vectorized assembly on amd64 and arm64:

- **`slices.Equal`, integer element kinds**: after the existing length
  check, integer equality is bitwise equality. Reinterpret both slices
  as bytes and call `bytealg.Equal`. All integer kinds, all
  architectures, no experiment gate.
- **`slices.Index` / `slices.Contains`, byte-sized kinds**: route to
  `bytealg.IndexByte`. (`Contains` is `Index(s, v) >= 0` and inherits.)

Floats are excluded from the `Equal` routing: bit-identical NaNs would
compare equal where `!=` reports unequal. They are covered exactly in
Phase 2 by a vector ordered-compare kernel.

These are small, independently mailable CLs that establish the
dispatch pattern with zero SIMD dependency.

## Phase 1: kernel package + IsSorted

`IsSorted` is the vehicle for standing up `internal/simdslices`:

- Kernel: compare each vector against a one-element-shifted window,
  any-violation mask. The parquet-go archsimd port's `orderOf*` kernels
  are a proven reference for this exact shape, including tuning
  history (single-load + permute scan).
- Integers first. Float `IsSorted` goes through `cmp.Less`'s NaN
  ordering (NaNs first), reproducible with explicit compare-and-merge;
  it can follow in the same file once the integer path settles.

## Phase 2: Reverse + wider search kernels

- **`Reverse`**: lane-reversal shuffles working inward from both ends.
  Dispatch covers fixed-width kinds; the swap loop remains otherwise.
  Expectation-setting: load/store bound, so the win is largest for
  byte-sized elements and modest for 64-bit ones.
- **`Index` / `Contains`, 16/32/64-bit kinds**: broadcast the needle,
  vector compare, find-first-set on the mask. Semantics are exact for
  every element type including floats (vector ordered-equality
  reproduces NaN-never-matches and -0 == +0).
- **`Equal`, float kinds**: vector ordered-compare, closing the Phase 0
  exclusion exactly.

## Phase 3: Sort

The largest item, structured as its own CL series:

1. **Sorting networks** for the base case (8–32 elements per width).
   The networks must be register-resident: the parquet port measured a
   16x serialization from building vectors through stack arrays
   (store-forwarding stalls), and sorting networks are maximally
   exposed to that trap. Construct with SetElem-style operations, not
   arrays.
2. **Vectorized partition**: broadcast pivot, compare, compress-store.
   AVX-512 `VPCOMPRESS*` makes this direct; the AVX2 tier uses
   lookup-table permutes. References: Intel x86-simd-sort (adopted by
   numpy for its 10–17x sort speedups) and Google vqsort.
3. **Integration**: dispatched kinds get quicksort with vectorized
   partition + network base case + heapsort depth fallback, keeping
   pdqsort's pivot heuristics; all other types keep pdqsort unchanged.
   Scope v1 to 32/64-bit integer kinds.
4. **Floats** (follow-up): NaN-prefilter (move NaNs to the front in
   any order — legal because `Sort` makes no stability promise, so NaN
   bit-order is unspecified), then sort the remainder; matches
   `cmp.Less`'s NaN-first ordering.
5. **`SortStable`**: a one-line dispatch decision, not a separate
   engineering item. For integer kinds, equal elements are
   indistinguishable, so stability is vacuous and the unstable
   vectorized path is byte-for-byte equivalent: dispatch integers to
   the Phase 3 sort. Float kinds keep the stable merge: -0/+0 and NaN
   bit patterns compare equal under `cmp.Less` but are observable via
   `math.Signbit` / `math.Float64bits`, so reordering them breaks the
   stability contract. Strings keep the stable merge (and were never
   SIMD-sortable).

## Testing and validation

- Kernel tests follow the min/max playbook: threshold-straddling
  sizes, extreme/violation-position sweeps through every tail path,
  adversarial patterns for sort (sorted, reversed, duplicate-heavy,
  sawtooth), seeds including infinities and NaN where floats apply.
- Float `Sort` cannot be compared bit-for-bit against a reference
  (NaN order is unspecified): compare as multiset-of-bits plus an
  ordering check.
- A fuzz harness comparing against the unspecialized implementation,
  particularly for sort.
- Hardware cadence per phase: Rosetta 2 locally for AVX2-path
  correctness; one GCP c3 (Sapphire Rapids) run, GOAMD64=v4 only, for
  AVX-512 correctness and `benchstat -count=10` numbers before
  anything mails. Hardware numbers go in CL messages from the start;
  no emulator numbers (Rosetta inverted the verdict on the 64-bit
  compare+blend tier during the min/max work).

## Known traps (from the min/max and parquet-go work)

- `ClearAVXUpperBits()` before every vector-to-scalar transition
  (AVX-SSE transition penalties; measured +2091% on one parquet
  kernel).
- Never rely on archsimd float `Min`/`Max` NaN behavior or operand
  order (undocumented; commutative-operand canonicalization).
- Ops that compile at AVX2 widths but SIGILL without AVX-512:
  `Int64x4.Min/Max`, 256-bit unsigned compares, `Mask32x8FromBits`.
  Use compare+blend / compare-built masks on the AVX2 tiers.
- Store-forwarding: build vectors with SetElem, never via stack
  arrays, in anything loop-resident.
- zsh `$c:src` history-modifier expansion mangles `git show` paths in
  scripts; use `${c}`.

## Sequencing and effort

| Phase | Contents | Effort |
|---|---|---|
| 0 | bytealg routing for Equal/Index/Contains | days |
| 1 | internal/simdslices + IsSorted | ~one min/max round |
| 2 | Reverse + wide Index/Equal kernels | ~one min/max round |
| 3 | Sort + SortStable dispatch | ~the whole min/max project |

Branch per phase off master, series-of-green-commits style. Phase 0
CLs stand alone; the gated phases get one umbrella tracker issue
("slices: vectorize operations for fixed-width element types under
GOEXPERIMENT=simd"), deliberately separate from #80857 so this track
does not inherit the compiler-pattern debate.
