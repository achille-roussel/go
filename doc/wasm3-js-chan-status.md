# GOARCH=wasm3 GOOS=js — `chan` integration status

## Status: scheduler + allocation done — per-T send/recv ops still TBD

Two halves of the integration shipped:

  1. Scheduler glue (gopark/goready ↔ WasmPark/WasmReady) —
     commit `ab524d90cf`.
  2. `make(chan T[, n])` allocation via `wasm3MakeChanIntrinsic`
     + per-T `$go.chan.<T>` WasmGC struct — commit `a81818d2de`.

The remaining piece is per-T `chansend` / `chanrecv` / `closechan`
codegen that operates on the new `$go.chan.<T>` struct.

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

Stock `chan T` send/receive trap — programs like

```go
done := make(chan int32, 1)
go func() { done <- 42 }()
v := <-done
```

compile + instantiate (allocation is done) but `main.main` lowers
to `unreachable` because there is no codegen yet for the
`OSEND` / `ORECV` SSA shapes operating on a `$go.chan.<T>` ref.
The chan struct fields (`qcount`, `dataqsiz`, `sendx`, `recvx`,
`buf`, `closed`) are declared but no op reads or writes them.

## Architecture revision: runtime/wasm.Chan[T], not per-T SSA ops

After a session of attempting per-T `OpWasm3ChanSend`/
`OpWasm3ChanRecv` SSA codegen, the verdict is: **don't**. The
per-op codegen surface is huge (each op ~100 lines of careful
SSA construction), every iteration is gated on the wasm3
backend's `OnWasmStackSkipped` tracker (subtle — "bad stack"
panics that need multi-step diagnosis), and the whole tower has
to be built before any tests run. Map ops landed via this path
in M3 Stage E/F, but that took weeks of focused work.

**Better:** a generic `runtime/wasm.Chan[T]` struct + methods.
Plain Go, testable on the host, single file, no new SSA ops.
The walk-layer substitution for `make(chan T)` /
`c <- v` / `<-c` becomes the only compiler-side work — much
smaller surface than per-T intrinsics.

The slice/string/interface field-cast fix (commit `c1451c4f91`,
this session) was the first blocker for this approach — without
it any struct with a slice field hit
`struct.set[1] expected (ref null $T), found anyref`. With that
fixed, a non-generic `struct { buf []int32 }` compiles cleanly.

## Remaining blocker: generic dictionary calling convention

Generic functions on wasm3 currently fail with the same
type-descriptor calling-convention mismatch as the original
makechan call:

```
CompileError: call[0] expected type anyref, found i64.const of type i64
```

Minimal repro:

```go
type box[T any] struct{ v T }
func makeBox[T any](v T) *box[T] { return &box[T]{v: v} }

func main() {
    b := makeBox[int32](42)  // fails at instantiation
    ...
}
```

Cause: Go generics use GC-shape stenciling (per-shape, not
per-instantiation). The first arg to an instantiated generic
function is a runtime *dictionary* pointing at a type
descriptor. The wasm3 backend lowers `*runtime.<dictionary>` as
anyref in the callee's signature but the call site marshals
the dictionary address as raw i64 — identical to the
`*chantype` issue we sidestepped by intrinsifying `makechan`.

Until this is fixed, `runtime/wasm.Chan[T]` is not callable.
The fix is in the wasm3 SSA backend's calling-convention
lowering — specifically, recognising that any `*T` arg whose
`T` is a runtime type descriptor should marshal as a typed ref
(via the descriptor's WasmGC ref global from M3 Stage G), not
as a linear-memory address. Same root cause as the original
makechan blocker; fixing it once unblocks both the chan
integration *and* generic functions in general on wasm3.

## Path forward

1. **Fix the wasm3 generic-dictionary calling-convention mismatch**
   in `cmd/compile/internal/wasm3/wasmabi.go` (or wherever the
   call-site arg marshaling happens). One focused session of
   wasm3 backend work.

2. **Implement `runtime/wasm.Chan[T]`** as a generic struct with
   `Send` / `Recv` / `Close` methods using `WasmPark` /
   `WasmReady` directly. ~100 lines of plain Go, testable.

3. **Walk-layer substitution** on js/wasm3: rewrite OMAKECHAN /
   OSEND / ORECV to call `runtime/wasm.MakeChan` /
   `.Send` / `.Recv`. The `chan T` source type maps to
   `*wasm.Chan[T]` via the walk transformation.

Without step 1 we cannot use generics in any user-facing
chan-like API on js/wasm3, so it's the gating change.

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
