# GOARCH=wasm3 GOOS=js M4 — complete

As of `098fc303ae`, the M4 milestone (goroutines via stack switching)
is end-to-end engine-validated for `GOARCH=wasm3 GOOS=js` on stock V8
in Node 25.9 with `--experimental-wasm-stack-switching`.

## The canary

A Go program that spawns a "goroutine" and exchanges a value with it
through a channel works:

```go
package main

import "runtime/wasm"

//go:wasmimport gojs runtime.LogInt
func logInt(n int32)

var chVal int32
var chSendPark int32 = 100
var chRecvPark int32 = 101
var chPending int32

func chSend(v int32) {
	chVal = v
	chPending = 1
	wasm.WasmReady(chRecvPark)
	wasm.WasmPark(chSendPark)
}

func chRecv() int32 {
	if chPending == 0 {
		wasm.WasmPark(chRecvPark)
	}
	v := chVal
	chPending = 0
	wasm.WasmReady(chSendPark)
	return v
}

func sender() {
	logInt(42)
	chSend(42)
	logInt(43)
}

func main() {
	logInt(0)
	wasm.Spawn(sender)
	v := chRecv()
	logInt(v)
	logInt(1)
}
```

Output:
```
log 0    <- main starts
log 42   <- sender pre-send
log 42   <- main: received the value
log 1    <- main: after recv
log 43   <- sender: resumed after ack
```

The two activations run on independent JSPI suspender stacks; the
channel synchronizes them via the WasmPark/WasmReady pair.

## What proves M4 complete

The original M4 plan asks for "goroutines via stack switching": two
or more wasm activations concurrently mid-suspend on independent
stacks, synchronizing through cooperative primitives. The canary
above demonstrates exactly that, end-to-end on a shipping engine.

The pieces:

| Layer | Where | Validated |
|---|---|---|
| Suspending JS import (Phase 1) | `runtime/wasm.SleepMs` | `0e73fe4570` |
| Promising entry (Phase 2) | `misc/wasm/wasm_exec_wasm3.js` wraps `run` | `0e73fe4570` |
| Park/Ready pair (Phase 3) | `runtime/wasm.WasmPark` / `WasmReady` | `f262bd3817` |
| Concurrent suspender spawn (Phase 4) | `runtime/wasm.Spawn` + `goroutine_run` //go:wasmexport | `098fc303ae` |
| Channel canary | `chSend` / `chRecv` over WasmPark/Ready | this doc |

## Where this is NOT yet the full vision

These are integration/wiring items, not engine viability — and not
blocking the M4 deliverable above:

- **Standard `go func()` keyword integration.** Currently callers
  invoke `wasm.Spawn(fn)` explicitly. To make `go fn()` route
  through Spawn, `runtime.newproc` on js/wasm3 needs an intercept.
- **Standard `chan` integration.** `make(chan T)` allocates Go's
  built-in hchan and exchanges via `chan.go`'s gopark/goready.
  Wiring those to WasmPark/WasmReady on js/wasm3 lets stock
  `chan`/`select` work between Spawn'd goroutines.
- **Bag-of-slots Spawn.** Phase 4 ships a single-pending-spawn
  slot; back-to-back `Spawn` calls clobber. The full
  `[]func()` ring is gated on two wasm3 backend codegen bugs:
  - `[N]func()` array indexing emits a `local.tee` mismatch
    (i64 vs anyref) around the spawn-entries array.
  - `map[K]func()` storage doesn't lower (struct.new mismatch).
  Same family as the `//go:wasmimport` multi-param gap; all
  wasm3 compiler/linker bugs separate from the JSPI work.
- **wasm3 `//go:wasmimport` multi-param + non-void-result.** The
  wrapper signature collapses to `(func (param i32))` regardless
  of declared shape. Workaround: single-param void imports + JS-
  side state. Fix locus: `cmd/compile/internal/ssagen/abi.go`
  param/result handling for non-`go_runtime` modules.

These items unblock the polish — `go fn()` instead of `Spawn(fn)`,
`<-ch` instead of `chRecv()`, more than one pending spawn — but
none of them are required to *prove* M4. The engine question is
answered.
