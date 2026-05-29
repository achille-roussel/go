# GOARCH=wasm3 GOOS=js — `chan` integration status

## Status: `chan int32` works end-to-end via stock keyword + closure capture

End-to-end stock `chan int32` keyword works on js/wasm3:

```go
done := make(chan int32, 1)
wasm.Spawn(func() { done <- 7 })   // ← captures done by closure
v := <-done                         // → 7
```

Six commits landed in sequence:

  1. Scheduler glue (gopark/goready ↔ WasmPark/WasmReady) — `ab524d90cf`.
  2. `make(chan T)` allocation via wasm3MakeChanIntrinsic — `a81818d2de`.
  3. Slice/string/iface struct-field ref.cast fix — `c1451c4f91`.
  4. `runtime/wasm.ChanInt32` first working channel — `898ff86821`.
  5. Walk-substitution: stock `chan int32` keyword dispatches to
     `wasm.ChanInt32` via runtime helpers — `8b4990e7dc`.
  6. Closure captures of WasmGC ref types (chan/map/func/*T via
     per-closure typed-struct fields) — `c9c9a35da7`.
  7. Generic-dictionary calling convention at runtime-call ABI
     (`OpWasm3LoweredAddr` with anyref param → descriptor ref-
     global via R_WASMDESCRIPTOR) — `3a84ae020e`.

Each fix is small and local; together they form an end-to-end
working chan substrate for `chan int32` with goroutine + closure
capture working through the normal Go syntax.

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

Other element types (`chan string`, `chan int64`, `chan T` for any
user struct) don't yet dispatch — walk-substitution is wired only
for `chan int32`. The fix is a generic `Chan[T]` collapsing the
per-T variants:

```go
// runtime/wasm:
type Chan[T any] struct { ... }
func MakeChan[T any](n int) *Chan[T] { ... }
func (c *Chan[T]) Send(v T) { ... }
func (c *Chan[T]) Recv() T { ... }
```

Plus walk-substitution that picks the right `MakeChan[T]`
instantiation for any chan element type.

Verified `Chan[T]` compiles. Direct call from main works —
generic-dictionary fix `3a84ae020e` makes the outer call cleanly
dispatch. **But the generic body itself fails** with the same
i64-vs-anyref calling-convention mismatch on its internal
`new(Chan[T])` and `make([]T, n)` calls. These take a runtime-
type-descriptor pointer derived from the dictionary, which is
materialised inside the generic body as an i64. The runtime call
(`runtime.newobject` / `runtime.makeslice64`) wants an anyref.

Same root pattern as the outer call site we just fixed, but at a
different SSA shape — the descriptor pointer comes from a dict
field load (`struct.get $dict 0`) rather than from
`OpWasm3LoweredAddr` directly. The fix is structurally similar:
detect "ref-typed param wants anyref + arg materialises as i64
type-descriptor pointer derived from a dict-field load" and
emit the descriptor-ref-global lookup. Smaller-scoped than the
outer-call fix but needs the dict-field-load detector.

## Architecture: runtime/wasm.Chan[T], not per-T SSA ops

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
