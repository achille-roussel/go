# GOARCH=wasm3 GOOS=js — `go fn()` keyword

## Status: working

The literal Go-language `go fn()` keyword runs end-to-end on js/wasm3.
Each `go` statement starts a new JSPI suspender stack via
`runtime/wasm.Spawn`.

Verified shapes:

  - `go bareFunc()`            → ✓ (`log 42`)
  - `go func() { ... }()`      → ✓ (`log 42`)
  - `go func() { ... }()` with captured local → ✓ (`log 99`)

## Dispatch path

The compiler routes `OGO` to a wasm3-specific runtime symbol instead
of the standard `runtime.newproc`:

```
go fn()
   ↓  cmd/compile/internal/ssagen/ssa.go callGo path
runtime.newprocJSWasm3(fn func())
   ↓  src/runtime/spawn_jswasm3.go
runtime/wasm.Spawn(fn)
   ↓  src/runtime/wasm/spawn_jswasm3.go
spawnSlot = fn ; wasmSpawn(0)  // JS host: queueMicrotask(promising_goroutine_run(0))
   ↓  JS event loop
WebAssembly.promising(instance.exports.goroutine_run)(0)
   ↓  runtime/wasm.goroutineRun
fn()                                   // runs on its own JSPI suspender stack
```

The signature change (`fn func()` instead of `fn *funcval`) lets the
closure SSA value — already typed `(ref $closureCtx)` by the wasm3
backend — flow straight into the spawn machinery without going
through the `*(*func())(unsafe.Pointer(&fn))` reinterpret-cast that
the standard runtime uses but the wasm3 backend can't lower (typed-
ref locals aren't address-takeable).

## Enabler: R_WASMHEAPTYPE

The first attempt to wire newproc → wasm.Spawn produced V8
instantiation failures: `Unknown heap type -64` in
`runtime_wasm.goroutineRun`. Diagnosis:

  - `ref.cast` / `ref.test` / `ref.null` take a `heaptype`
    immediate, which the wasm GC spec encodes as **s33 LEB128**.
  - The wasm3 linker was writing typeidx immediates as **uleb128**
    via `R_WASMTYPE` in every slot, including heap-type positions.
  - For typeidx ≥ 64, uleb and sleb diverge: typeidx 64 is uleb
    `0x40` (one byte) but sleb33 `0xC0 0x00` (two bytes). A wasm
    validator reading the position as a sleb33 sees `0x40` as -64,
    the void block-type byte → "invalid heap type".

The fix is a new relocation type, `objabi.R_WASMHEAPTYPE`, with a
parallel handler in the linker that calls `writeSleb128` instead of
`writeUleb128`. The four ops that emit heap-type immediates —
`ARefCast`, `ARefCastNull`, `ARefTest`, `ARefNull`, and the
`(ref null T)` block-result form for `ABlock`/`ALoop` — switched to
the new relocation. Other ops (`AStructGet`, `AArrayGet`,
`ACallIndirect`, ...) keep `R_WASMTYPE` since their spec position is
typeidx (uleb).

## Known limitation: single-slot Spawn

`wasm.Spawn` uses one global slot (`spawnSlot`); a tight loop of N
goroutines spawns N times before any goroutine_run runs, and only
the last surviving slot value executes. From a Phase 4 test:

```go
for i := int32(10); i < 13; i++ {
    v := i
    go func() { logInt(v) }()
}
```

prints `log 12` only. The other two spawns are lost.

This is the documented Phase 4 floor — the bag-of-stacks rewrite is
gated on wasm3 codegen for `[N]func()` arrays and `map[K]func()`
(see `runtime/wasm/spawn_jswasm3.go`'s doc comment). It's orthogonal
to the `go` keyword wiring: as soon as Spawn is multi-slot, `go fn()`
inherits it for free.

## Canary

```go
package main

import _ "runtime/wasm"

//go:wasmimport gojs runtime.LogInt
func logInt(n int32)

func main() {
    logInt(0)
    go func() { logInt(42) }()
    logInt(1)
}
```

```
$ GOOS=js GOARCH=wasm3 go build -o main.wasm .
$ node --experimental-wasm-stack-switching \
       --experimental-wasm-wasmfx \
       misc/wasm/wasm_exec_wasm3.js main.wasm
log 0
log 1
log 42
```

The `log 1` printing before `log 42` shows the goroutine ran on a
separate JSPI suspender — it was queued via `queueMicrotask` after
main yielded, then executed.
