# GOARCH=wasm3 — chan implementation via WasmGC subtyping

## Status

Plan, not yet implemented. Supersedes the current per-T `$go.chan.<T>` +
fully-generated trampoline design (see commits `da0b94c0a9` chain,
`88413a3466`, `da1d5265d5`, `b09b5ced37`, `c3144caa48`).

## Motivation

The current chan-T mapgen design (`reflectdata/wasm3_changen.go`) generates
a complete per-T trampoline per chan element type, with the chan struct
shape `$go.chan.<T>` redefined independently in each function's local type
collector. This has produced two recurring failure modes:

1. **Chan-struct-shape inconsistency** between functions. `makechan`'s
   local collector and `chanrecv`'s local collector can produce
   structurally-different `$go.chan.<T>` shapes (e.g. one with
   `buf: (ref null (array (ref null go.object)))` and the other with
   `buf: (ref null (array (ref null $go.struct.T)))`). `mergeTable`'s
   structural dedup correctly treats these as distinct types, and the
   `ref.cast` in the consuming function traps with "illegal cast" at
   runtime. This is the current blocker for `chan UserStruct` end-to-end.

2. **Code bloat + IR-construction fragility.** Each per-T trampoline
   duplicates ~70 lines of park/unpark/field-update IR built via
   `ir.New*` calls in `wasm3_changen.go`. The IR-construction surface
   is broad and rule-generator interactions (e.g. the variadic-`___`
   bug in the auto-rewriter, fixed in `53a61e50b6`) keep biting.

Both stem from the same root choice: the chan struct's *full* shape is
per-T, so every function that touches a `chan T` re-derives the shape
independently, and every operation has to be generated per T.

## Design

Use the WasmGC subtyping mechanism wasm3 already employs for `$go.object`
and the per-Go-struct types: pull the chan state into a base type shared
across all `chan T`, and make the per-T type a subtype that only adds the
typed buf array.

### Type layout

```
;; In wasmgc.PreludeTypes — defined once, shared across all functions.
$go.chan (sub $go.object (struct
    (field (mut i32))   ;; 0: qcount    — live buffered count
    (field (mut i32))   ;; 1: dataqsiz  — buffer capacity
    (field (mut i32))   ;; 2: sendx     — next send slot
    (field (mut i32))   ;; 3: recvx     — next recv slot
    (field (mut i8))    ;; 4: closed
    (field (mut i32))   ;; 5: sendParkID
    (field (mut i32))))  ;; 6: recvParkID

;; Per-T, registered lazily by collectChanStruct. Adds only the typed buf.
$go.chan.<T> (sub $go.chan (struct
    (field (mut i32))                       ;; 0: qcount     (inherited slot)
    (field (mut i32))                       ;; 1: dataqsiz
    (field (mut i32))                       ;; 2: sendx
    (field (mut i32))                       ;; 3: recvx
    (field (mut i8))                        ;; 4: closed
    (field (mut i32))                       ;; 5: sendParkID
    (field (mut i32))                       ;; 6: recvParkID
    (field (mut (ref null (array T))))))    ;; 7: slots — the only per-T bit
```

Per the WasmGC sub-typing rules, fields 0..6 of `$go.chan.<T>` *must*
repeat the base's field declarations exactly; the validator enforces
that. The slots field is the only structural extension.

i32 throughout — aligns with the broader "int = i32 on wasm3" direction.
Capacity caps at 2^31-1 (Go's `make(chan T, n)` already caps `n` at
`MaxInt`, which equals the i32 limit on a 32-bit ABI). `array.get/set`
takes i32 indices natively, so the per-op `i32.wrap_i64` tax in the
current design goes away.

### Runtime helpers (runtime/wasm)

```go
//go:build js && wasm3

package wasm

// channel mirrors $go.chan exactly so a *channel maps to (ref $go.chan).
// Unexported — internal to the runtime/wasm + compiler boundary.
type channel struct {
    qcount     int32
    dataqsiz   int32
    sendx      int32
    recvx      int32
    closed     bool
    sendParkID int32
    recvParkID int32
}

// wasm3ChanReserveSend blocks until a buf slot is free and returns sendx.
// The trampoline writes its value at slots[sendx] and then calls
// wasm3ChanFinishSend.
//
//go:linkname wasm3ChanReserveSend
//go:noinline
func wasm3ChanReserveSend(c *channel) int32 {
    for c.qcount >= c.dataqsiz {
        c.sendParkID = wasm3MintChanParkID()
        WasmPark(c.sendParkID)
        c.sendParkID = 0
    }
    return c.sendx
}

//go:linkname wasm3ChanFinishSend
//go:noinline
func wasm3ChanFinishSend(c *channel) {
    c.sendx = (c.sendx + 1) % c.dataqsiz
    c.qcount++
    if id := c.recvParkID; id != 0 {
        c.recvParkID = 0
        WasmReady(id)
    }
}

//go:linkname wasm3ChanReserveRecv
//go:noinline
func wasm3ChanReserveRecv(c *channel) int32 {
    for c.qcount <= 0 {
        c.recvParkID = wasm3MintChanParkID()
        WasmPark(c.recvParkID)
        c.recvParkID = 0
    }
    return c.recvx
}

//go:linkname wasm3ChanFinishRecv
//go:noinline
func wasm3ChanFinishRecv(c *channel) {
    c.recvx = (c.recvx + 1) % c.dataqsiz
    c.qcount--
    if id := c.sendParkID; id != 0 {
        c.sendParkID = 0
        WasmReady(id)
    }
}
```

The compiler-emitted trampoline only needs `ref.cast` at the boundary
to access `slots` (the per-T field). At every other access (qcount,
sendx, parkID, etc.) the helpers go through the base `(ref $go.chan)`
which doesn't need `ref.cast` per T. This is the WasmGC subtype-
polymorphism payoff: the parent struct's fields are visible through
any subtype reference.

### Compiler-emitted per-T trampoline

```go
// auto-generated per chan T:
func wasm3MakeChan_<T>(n int32) *channel {
    c := new($go.chan.<T>)         ;; struct.new_default $go.chan.<T>
    c.dataqsiz = n                 ;; struct.set $go.chan 1 (via base)
    if n > 0 {
        c.slots = make([T], n)     ;; array.new_default + struct.set $go.chan.<T> 7
    }
    return (*channel)(c)           ;; (ref $go.chan.<T>) → (ref $go.chan) via subtype
}

func wasm3ChanSend_<T>(c *channel, v T) {
    slot := wasm3ChanReserveSend(c)
    cT := (*$go.chan.<T>)(c)       ;; ref.cast (ref $go.chan) → (ref $go.chan.<T>)
    cT.slots[slot] = v             ;; array.set $arr_T
    wasm3ChanFinishSend(c)
}

func wasm3ChanRecv_<T>(c *channel) T {
    slot := wasm3ChanReserveRecv(c)
    cT := (*$go.chan.<T>)(c)
    v := cT.slots[slot]
    wasm3ChanFinishRecv(c)
    return v
}
```

Lowered to wasm, the send/recv trampolines are roughly:

```wat
(func $wasm3ChanSend_T (param (ref null $go.chan.T) (ref null $T))
  local.get 0
  ;; (ref $go.chan.<T>) is a subtype of (ref $go.chan) — pass directly,
  ;; no ref.cast needed at the call boundary.
  call $wasm3ChanReserveSend           ;; → i32 slot
  local.set 2
  local.get 0
  struct.get $go.chan.T 7              ;; slots
  ref.cast (ref $arr_T)
  local.get 2                          ;; slot index (already i32)
  local.get 1                          ;; value
  array.set $arr_T
  local.get 0
  call $wasm3ChanFinishSend)
```

Per-T IR generated: ~10 lines instead of ~70.

## Migration plan

1. **Define `$go.chan` in `cmd/internal/wasmgc/type.go`'s prelude.**
   Add `TypeGoChan` after `TypeGoIface`, with the 7-field i32/i8 layout.
   Bump `NumPreludeTypes`. Update the table-merge logic to treat
   `TypeGoChan` as a fixed prelude index (like `TypeGoObject`, etc.).

2. **Rewrite `collectChanStruct` in `cmd/compile/internal/wasm3/wasmtype.go`.**
   The per-T struct now has `Super: TypeGoChan` and exactly one
   additional field: `(ref null (array T))` at index 7. The first
   seven fields must literally repeat the base's fields (WasmGC
   subtyping requires the prefix match). Reuse `collectBacking(t.Elem())`
   for the slots array type.

3. **Add `runtime/wasm/channel_jswasm3.go`** with the `channel` struct
   and the four helpers above. Delete the existing per-field stubs in
   `src/runtime/chan_jswasm3.go` (qcount/dataqsiz/sendx/recvx/sendParkID/
   recvParkID getters and setters) — they're no longer needed because
   the trampoline body is short enough to call the bundled helpers
   directly. Keep `wasm3ChanPark`, `wasm3ChanReady`, `wasm3MintChanParkID`,
   `wasm3ChanAlloc` (still needed for allocation).

4. **Shrink `reflectdata/wasm3_changen.go`.** `MakeChanFuncWasm3` becomes
   alloc + dataqsiz set + slots alloc + return. `ChanSendFuncWasm3` /
   `ChanRecvFuncWasm3` become reserve + cast + array op + finish. All
   the per-field SSA-op orchestration (qcount loops, parkID handling,
   send/recv-x arithmetic, blocked-peer wakeup) deletes — it lives in
   Go in runtime/wasm now.

5. **Retire the per-field SSA ops in `Wasm3Ops.go` + intrinsics.** The
   `OpWasm3Chan{Qcount,Dataqsiz,Sendx,Recvx,SendParkID,RecvParkID}`
   getters/setters and the corresponding runtime stubs
   (`wasm3ChanQcount`, `wasm3ChanQcountSet`, ...) all delete. Keep
   `OpWasm3ChanBufGet`, `OpWasm3ChanBufSet`, `OpWasm3ChanBufAlloc`
   under new names (`OpWasm3ChanSlotsGet`/`Set`/`Alloc`) — they still
   wrap the per-T `slots` array access.

6. **Regenerate `rewriteWasm3.go` + `opGen.go`.** Run
   `cmd/compile/internal/ssa/_gen` after the op-list changes.

7. **Update `cmd/compile/internal/wasm3/ssa.go`'s codegen** for the
   surviving chan ops. The base-field access (qcount/dataqsiz/...) is
   reached via `(ref $go.chan)` upcasts when the helpers manipulate
   `*channel` directly, so no codegen change is needed there — the
   wasm validator handles subtype upcast implicitly. Per-T slots
   access stays as today (ref.cast to `$go.chan.<T>` + struct.get 7
   + array.get/set).

8. **Verify the chan-struct merge dedup works correctly under the new
   layout.** Two functions that touch `chan Msg` should produce the
   same `$go.chan.Msg` type after `mergeTable` — the shared base
   plus a single per-T extension forces structural agreement. This
   closes the current `chan UserStruct` "illegal cast" failure.

9. **Test matrix:**
   - `chan int`, `chan int64`, `chan bool`, `chan string` — regression.
   - `chan UserStruct` — new green case.
   - `chan *T` (any pointer T) — new green case.
   - `chan [N]int` — array element type.
   - Multi-element chans, full/empty park/unpark across goroutines.

## What this does NOT yet address

- **`close(c)`** semantics + `v, ok := <-c` receive form. Adding a
  helper `wasm3ChanClose(c *channel)` is straightforward; the trampoline
  for `v, ok := <-c` needs a tuple-return shape that doesn't exist yet.
- **`select`**. The current trampolines bypass the standard sudog
  scheduler glue; supporting `select` (multiple waiters, fair pick)
  needs a per-side queue, not the current single-slot wait register.
- **Multiple waiters per side.** `sendParkID` / `recvParkID` are
  single-slot. A real `[]int32` waiter queue is needed for chans with
  more than one blocked goroutine per side.
- **Unbuffered channels** (synchronous handoff). Today every `chan T`
  acts buffered with `dataqsiz=0` meaning "send always parks". Real
  unbuffered semantics needs the synchronous-handoff path.

These all become easier under this design — they're plain Go edits in
`channel_jswasm3.go` rather than compiler-IR work.

## Estimated effort

1-2 focused sessions. The bulk is mechanical (refactor
`collectChanStruct`, port helpers from IR construction to Go, delete the
per-field op surface). The new code is smaller than what it replaces.

## Files touched

| Area | Path |
|---|---|
| Type prelude | `cmd/internal/wasmgc/type.go` |
| Per-T chan type | `cmd/compile/internal/wasm3/wasmtype.go` (`collectChanStruct`) |
| Runtime helpers | `runtime/wasm/channel_jswasm3.go` (new) |
| Trampoline gen | `cmd/compile/internal/reflectdata/wasm3_changen.go` |
| SSA op defs | `cmd/compile/internal/ssa/_gen/Wasm3Ops.go` + regen |
| SSA intrinsics | `cmd/compile/internal/ssagen/intrinsics.go` |
| Backend codegen | `cmd/compile/internal/wasm3/ssa.go` |
| Runtime dispatch | `runtime/chan_jswasm3.go` (helper-stub list shrinks) |
| Built-in decls | `cmd/compile/internal/typecheck/_builtin/runtime.go` + regen |

Net deletion expected: ~300 LOC (the per-field op surface +
per-trampoline IR generation), against ~80 LOC added (the prelude type
+ four `channel` helpers + shorter trampolines).
