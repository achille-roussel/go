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

## Next milestone: per-T chan ops

Per-T `OpWasm3ChanSend` and `OpWasm3ChanRecv` SSA ops that
manipulate the `$go.chan.<T>` struct directly. Intrinsified at
the same layer the makechan intrinsic now lives, with side-
channel recording in `walkSend` / `walkRecv` to pass the chan
`*types.Type`. The codegen:

  - **Buffered fast path:** read `qcount`, compare against
    `dataqsiz`. If room, read `sendx` / `buf`, array.set the
    value, increment `sendx` and `qcount`, check `recvq` head
    and call `WasmReady(sg.wasm3ParkID)` if non-nil. Mirror
    for recv.
  - **Blocking slow path:** `acquireSudog`, populate
    `sg.wasm3ParkID = wasm3NextParkID++`, enqueue on the
    chan's `sendq` / `recvq`, call `wasm.WasmPark(parkID)`.
    On wake, re-check buffer state. The sudog allocator is
    already wasm3-friendly (sudog struct lowers cleanly to
    WasmGC).
  - **Direct handoff optimisation:** skip the buffer when
    `sendq`/`recvq` already has a waiter — copy directly to
    the waiter's elem slot, wake them.

Two missing fields need to be added to `collectChanStruct`:
`sendq` and `recvq` (`(ref null $sudog)` each). Both need
`collectStruct(types.RuntimeSudog)` to register the sudog
WasmGC struct on first use.

`select` is a separate, larger milestone — needs a
`selectgo`-equivalent driver and per-case glue. The chan ops
above are the prerequisite.

Scope: each op is ~50-100 lines of SSA construction in
`wasm3/ssa.go` plus an intrinsic dispatch. ~half a day per op
for send and recv (buffered + blocking + handoff). close is
smaller. Total: probably 2-3 focused sessions.

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
