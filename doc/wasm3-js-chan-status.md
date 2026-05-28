# GOARCH=wasm3 GOOS=js — `chan` integration status

## Status: half-done — scheduler integrated, allocation blocked

The scheduler glue (gopark/goready ↔ WasmPark/WasmReady) is
complete and verified non-breaking. The remaining blocker is
wasm3-backend allocation support for `make(chan T)` — a
substantial parallel of the existing map intrinsic infrastructure.

## What works

`runtime.gopark` and `runtime.goready` route to `WasmPark`/
`WasmReady` on js/wasm3 via a per-sudog `wasm3ParkID`. Commit
`ab524d90cf` lands the substrate:

  - `sudog.wasm3ParkID int32` — the JSPI park identifier for a
    blocked goroutine, set by `wasm3Park` (gopark's js/wasm3
    dispatch) and read by `wasm3Goready` (the chan/select waker
    path).
  - `runtime/sched_jswasm3.go` — `wasm3Park` (gopark impl) and
    `wasm3Goready` (waker), backed by `wasm3PreparePark` which
    chan/select call right before the standard gopark.
  - `runtime/sched_notjswasm3.go` — no-op / forward-to-standard-
    goready stubs so chan.go / select.go can call the new hooks
    unconditionally on every build.
  - `runtime/chan.go` blocking gopark sites now call
    `wasm3PreparePark(mysg)` first; goready call sites call
    `wasm3Goready(sg)` instead of `goready(gp, …)`.
  - `runtime/proc.go` — gopark/goready early-return on
    `goos.IsJs && goarch.IsWasm3` to the wasm3 dispatchers.

Verified non-breaking:
  - Host (darwin/arm64): stock `chan` round-trip works.
  - wasip1/wasm3: builds + validates.
  - js/wasm3 WasmPark/WasmReady-primitive tests still run end-
    to-end.

## What's blocked

Stock `make(chan T, n)` fails at instantiation on js/wasm3:

```
CompileError: WebAssembly.instantiate(): Compiling function
              "main.main" failed: call[0] expected type anyref,
              found local.get of type i64
```

Root cause: the wasm3 backend declares `runtime.makechan`'s
wasm-level signature with `anyref` for the `*chantype` argument
(correct, per the M2 pointer-rep cutover — `*T` is a typed ref),
but the call site in `main.main` marshals the type-descriptor
arg as a raw `i64` linear-memory address:

```
;; main.main SSA → wasm:
Get $type:chan int32(SB)     ;; loads i64 (descriptor address)
LocalSet $1                  ;; into i64 local
...
LocalGet $1                  ;; i64
I64Const $1                  ;; size i64
CALL runtime.makechan        ;; expects (anyref, i64) — MISMATCH
```

The caller hasn't been taught that the chan type-descriptor arg
needs to be a typed ref / anyref descriptor at this call boundary.

## Two paths to resolution

### Option A — Intrinsify `runtime.makechan` (mirror the map path)

The existing pattern for maps:

  - `walkMakeMap` records the map `*types.Type` on a side channel
    (`ir.Wasm3MakeMapTypes`).
  - `wasm3MakeMapIntrinsic` in `ssagen/intrinsics.go` replaces
    the call with `OpWasm3MakeMap`.
  - `OpWasm3MakeMap` codegen in `wasm3/ssa.go` emits
    `struct.new_default $go.map.<K,V>`.
  - The runtime map type is bypassed entirely — wasm3 uses its
    own simplified `$go.map.<K,V>` struct, with per-(K,V) wasm3-
    native map operations (16 intrinsics in
    `ssagen/intrinsics.go`).

For chan, the parallel work:

  1. Add `ir.Wasm3MakeChanTypes sync.Map`.
  2. Modify `walkMakeChan` to record the chan `*types.Type` on
     wasm3.
  3. Add `OpWasm3MakeChan` SSA op declaration in
     `ssa/_gen/Wasm3Ops.go`, regenerate.
  4. Add `wasm3MakeChanIntrinsic` for `runtime.makechan` /
     `runtime.makechan64`.
  5. Define a `$go.chan.<T>` wasm3-native struct. Minimal fields:
     `qcount`, `dataqsiz`, `buf (ref (array T))`, `sendx`,
     `recvx`, `sendq (ref null $go.sudog)`,
     `recvq (ref null $go.sudog)`, `closed bool`. No `elemtype`
     pointer (the element type is encoded structurally), no
     `lock` (single-threaded under JSPI).
  6. Codegen for `OpWasm3MakeChan`: `struct.new_default
     $go.chan.<T>`, then `struct.set dataqsiz`, then for n > 0
     also `array.new_default` + `struct.set buf`.
  7. Per-(T) wasm3-native chan operations to replace
     `runtime.chansend` / `runtime.chanrecv` / `runtime.closechan`
     / `runtime.chanlen` / `runtime.chancap`. Each routes
     through `wasm3PreparePark` + WasmPark for blocking and
     `wasm3Goready` for waking.
  8. `select.go` parallel — `runtime.selectgo` driver paired
     with per-case chan ops.

Scope estimate: comparable to the M3 map work (~1600 lines of
chan+select runtime to parallel; 16+ intrinsics; substantial
codegen). Multi-day project.

### Option B — Fix the calling convention for `*runtime.<type>` args

Generic fix at the wasm3 SSA backend: when a runtime call's
signature has a `*T` argument whose `T` is a runtime type
descriptor (`chantype`, `maptype`, etc.), marshal the arg as
the typed ref / anyref the callee expects.

The needed change is in the wasm3 call lowering — specifically
where `Get $type:foo(SB)` produces an i64 instead of a ref. The
"descriptors as WasmGC refs" mechanism from M3 Stage G already
materialises type descriptors as `(ref $go.object)` globals; the
fix is to route runtime-type symbol references through that
global rather than emitting a linear-memory address.

This is more general (would fix every `*type` arg, not just
chan), but the audit + retrofit is non-trivial.

## Recommendation

Option A is the cleaner mirror of existing wasm3-native patterns
and contains less risk to other paths. Option B fixes a wider
class of issues but requires careful audit. Both are real M4
follow-up projects, not single-session work.

For now, programs that need concurrency on js/wasm3 can use the
`runtime/wasm` primitives directly:

```go
import "runtime/wasm"

const myParkID = 7
go func() {
    // ... do work ...
    wasm.WasmReady(myParkID)
}()
wasm.WasmPark(myParkID)
```

These primitives are the same JSPI suspend/resume the chan
integration would use internally, just exposed at a lower level.
