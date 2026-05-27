# GOARCH=wasm3 GOOS=js — `go fn()` keyword status

## Where this stands

`runtime/wasm.Spawn(fn)` works end-to-end: a Go program that calls
`wasm.Spawn(fn)` gets a real concurrent goroutine on its own JSPI
suspender stack, validated on Node 25.9 with
`--experimental-wasm-stack-switching` (see
`doc/wasm3-js-m4-complete.md` for the canary).

**The literal Go-language `go fn()` keyword does NOT yet route
through `wasm.Spawn`** — it goes through `runtime.newproc` as on
every other arch, which hits the standard scheduler the wasm3
runtime doesn't have.

## What I tried, and what blocks

The natural route is to compile-time-gate `runtime.newproc` so
js/wasm3 dispatches to a `newprocJSPI` function that forwards to
`runtime/wasm.Spawn`:

```go
// proc.go
func newproc(fn *funcval) {
    if goos.IsJs == 1 && goarch.IsWasm3 == 1 {
        newprocJSPI(fn)
        return
    }
    // ... standard scheduler ...
}

// runtime/spawn_jswasm3.go
func newprocJSPI(fn *funcval) {
    wasm.Spawn(*(*func())(unsafe.Pointer(&fn)))
}
```

This compiles cleanly. But the resulting binary fails V8 instantiation:

```
CompileError: Compiling function #11:"runtime_wasm.goroutineRun"
              failed: Unknown heap type -64 @+1773
```

`wasm-tools print` confirms a malformed byte in `goroutineRun`'s
body — specifically a `0x40` (s33-encoded as `-64`) in a position
where the wasm decoder expects a typed heap type. The byte is
emitted by the wasm3 backend when `wasm.Spawn` gets inlined into
`newprocJSPI` and the inliner's interaction with
`goroutineRun`'s `//go:wasmexport` and the func-typed
`spawnSlot` global produces invalid codegen for the slot store
path.

Diagnosis details:

  - The exact same `goroutineRun` body compiles cleanly when
    `wasm.Spawn(fn)` is called only from user code (the working
    `m4_spawn.wasm` fixture). Adding a second caller from
    `runtime.newprocJSPI` is what triggers the bug.
  - All six shapes I tried produced the same `Unknown heap type
    -64` error:
    1. Direct `wasm.Spawn(*(*func())(unsafe.Pointer(&fn)))` import.
    2. `unsafe.Pointer`-typed slot stored from runtime, read as
       func() in goroutineRun via reinterpret.
    3. `*funcval`-typed slot stored from runtime, read via
       reinterpret in a separate helper.
    4. `//go:linkname` bridge from runtime to runtime/wasm slot
       global.
    5. `//go:linkname` bridge from runtime to runtime/wasm.Spawn
       (typed as `*funcval` on the runtime side, `func()` on
       runtime/wasm side — same wasm-level anyref).
    6. `//go:nowritebarrier` + `//go:noinline` on the
       newprocJSPI / Spawn / goroutineRun trio.

  - The bug is not the cast or the linkname — it's specifically
    the wasm3 SSA/codegen pass that emits goroutineRun's body
    when an additional caller of wasm.Spawn exists. Suspect: a
    type-analysis pass that treats spawnSlot's element type
    differently in the multi-caller case and emits a malformed
    typed-ref opcode.

## What unblocks `go fn()`

Fixing the wasm3 backend codegen for the malformed-heap-type
emission. Specifically:

1. Reproduce minimally: a 30-line Go program with a func()
   global, a //go:wasmexport reader, and two writers — one in
   the same package, one cross-package via a runtime intercept
   path. (The `newprocJSPI`-as-intercept arrangement triggers it
   reliably.)
2. wasm-tools dump the emitted module; locate the
   `Unknown heap type -64` byte (a `0x40` in a typed-ref slot).
3. Trace it back through the wasm3 obj-backend encoder to find
   the SSA op that emits it without the correct preceding
   reference type. Suspect: `OpWasm3LoweredCall` /
   `OpWasm3FuncValue` / one of the closure-related ops with an
   off-by-one in the type-immediate sequence.
4. Once that's fixed, the `newprocJSPI` intercept in proc.go
   should produce a working `go fn()` keyword path without
   further integration work — the runtime side is already
   sketched out (see this doc's "What I tried" section).

## Workaround for now

Use `runtime/wasm.Spawn(fn)` explicitly instead of `go fn()`:

```go
import "runtime/wasm"

wasm.Spawn(func() {
    // goroutine body — runs on its own JSPI suspender stack.
})
```

Same concurrency model, same JSPI semantics, just a different
spelling. The `m4_spawn.wasm` and `m4_chan.wasm` fixtures both use
this path and run end-to-end.

## Why the wasm3 backend bug isn't trivial to fix in-session

The malformed byte is downstream of an SSA pass that's making a
type-analysis decision based on the call-graph shape. It's not a
single missing case statement — it's an interaction between the
inliner, the global-store codegen, the //go:wasmexport wrapper
emission, and the wasm3 register/local-type planner. Each of
those works in isolation; the combination breaks.

A real fix needs:

  1. A focused minimal reproducer (the rough shape is in
     `runtime/spawn_jswasm3.go` reverts above + `runtime/wasm/
     spawn_jswasm3.go`'s goroutineRun reader).
  2. SSA-html dumps of goroutineRun in both the working
     (single-caller) and failing (multi-caller) cases.
  3. Comparing the late-phase SSA to find where the type
     diverges.
  4. Identifying the pass that emits the bad opcode.
  5. Fixing in `cmd/compile/internal/wasm3/ssa.go` or
     `cmd/internal/obj/wasm/wasm3obj.go`.

This is several hours of focused wasm3 backend work.
