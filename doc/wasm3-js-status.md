# GOARCH=wasm3 GOOS=js — current status

## Summary

The js/wasm3 direction (see
`.claude/plans/js-wasm3-jspi.md`) is the engine-runnable
alternative to the wasmfx M4 path that was blocked at every
upstream runtime. **JSPI's core primitives — suspending imports,
promising exports, gopark/goready-shaped park+ready pairs — all
work end-to-end today on stock V8 in Node 25.9** with
`--experimental-wasm-stack-switching`.

What got built this session:

| Phase | Commits | What runs end-to-end |
|---|---|---|
| 0 — GOOS=js registration, JS shim | `57530dd7ad`..`aeb0d389d8` | empty `main` builds, instantiates, exits cleanly on Node |
| 1+2 — `wasm.SleepMs` (JSPI suspending) + entry promising | `0e73fe4570` | `logInt(0); SleepMs(150); logInt(1)` — real 150ms wall-clock gap on V8 |
| 3 — `WasmPark` / `WasmReady` primitives | `f262bd3817` | `selfWakeMs(42); WasmPark(42); logInt(1)` — wasm suspends, JS setTimeout fires `WasmReady`, wasm resumes |

The Phase 3 demo is the engine-validated proof that gopark/goready
mechanics work via JSPI today.

## What's still required for a "real M4" deliverable

The Go-side scheduler isn't yet wired to use the JSPI primitives.
The primitives exist as `runtime/wasm.WasmPark` / `wasm.WasmReady`;
turning that into `go func(){...}()` + working channels needs:

1. **`proc_jswasm3.go` scheduler shims**: replace `gopark`'s
   busy-loop `Gosched` path on js/wasm3 with a `WasmPark(g_id)`
   call; replace `goready`'s runqueue enqueue with `WasmReady(g_id)`.
   `g_id` is a stable integer per `*g` (could be allgs index).

2. **`go fn()` spawn machinery**: `runtime.newproc` on js/wasm3
   needs to tell the JS host to start a NEW promising call into a
   `__wasm3_run_goroutine(g_id int32)` wasm export. Each goroutine
   becomes its own JSPI suspender stack. The export looks up `g_id`
   in `allgs`, sets it as the current `g`, and runs its entry.

3. **JS-side scheduler**: a `Map<g_id, Promise>` of running
   goroutines, a list of pending spawns, lifetime tracking so g_ids
   can be reclaimed after goexit.

4. **Channel test**: `ch := make(chan int); go func(){ch<-42}(); println(<-ch)`
   runs end-to-end. This is the canonical M4 deliverable.

These are individually clear pieces — none of them face an engine
blocker — but together they're 1-2 days of focused work to land
properly (the standard Go scheduler is fiddly to override; the
spawn machinery needs careful design around promising-call
lifecycle and resource cleanup).

## Known toolchain gap (separate from M4)

`//go:wasmimport` on wasm3 drops parameters past the first AND drops
return types when the imported module isn't `go_runtime`. The
wrapper signature reduces to `(func (param i32))` regardless of the
Go signature.

  - Affects: any js/wasm3 //go:wasmimport with >1 param or any
    non-void result against the `gojs` module.
  - Worked around: Phase 1+3 demos use single-param void imports
    + JS-side state (the `parkResolvers` Map). Phase 4 will need
    to fix this for `go fn()` spawning that passes (g_id, fn_ptr)
    plus channel ops that exchange typed values.
  - Fix scope: `cmd/compile/internal/ssagen/abi.go`
    paramsToWasmFields / resultsToWasmFields produce the right
    `wi.Params` / `wi.Results`; the dropoff happens somewhere
    between those and the linker's wrapper signature emission.
    Plausibly the `refABI` gate (currently `Module ==
    wasm3GoRuntimeModule`) is too narrow and the non-go_runtime
    path falls into a fallback that emits a single i32 placeholder.

## How to run the demos

Phase 1+2 (suspend / resume on a setTimeout):
```sh
cat > /tmp/m4_jspi.go <<'EOF'
package main
import "runtime/wasm"
//go:wasmimport gojs runtime.LogInt
func logInt(n int32)
func main() {
    logInt(0)
    wasm.SleepMs(150)
    logInt(1)
}
EOF
GOOS=js GOARCH=wasm3 go build -o /tmp/m4_jspi.wasm /tmp/m4_jspi.go
node --experimental-wasm-stack-switching \
     misc/wasm/wasm_exec_wasm3.js /tmp/m4_jspi.wasm
# log 0
# (150ms wall-clock gap)
# log 1
```

Phase 3 (park / external ready):
```sh
cat > /tmp/m4_park.go <<'EOF'
package main
import "runtime/wasm"
//go:wasmimport gojs runtime.LogInt
func logInt(n int32)
//go:wasmimport gojs runtime.SelfWakeMs
func selfWakeMs(parkID int32)
func main() {
    logInt(0)
    selfWakeMs(42)
    wasm.WasmPark(42)
    logInt(1)
}
EOF
GOOS=js GOARCH=wasm3 go build -o /tmp/m4_park.wasm /tmp/m4_park.go
node --experimental-wasm-stack-switching \
     misc/wasm/wasm_exec_wasm3.js /tmp/m4_park.wasm
# log 0
# (~100ms wall-clock gap)
# log 1
```
