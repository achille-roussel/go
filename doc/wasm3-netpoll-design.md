# Netpoll design for GOOS=js GOARCH=wasm3

Status: design accepted, implementation pending.

## Why this can't be a port of an existing netpoll backend

Every other runtime netpoll backend (epoll / kqueue / poll_oneoff /
IOCP) is a *kernel-poller wrapper*: the runtime calls into a host
syscall that blocks the OS thread until some watched fd becomes
ready, then returns a batch.

The wasm3 host is V8 / Node. V8 doesn't poll fds — Node libuv does,
and what it exposes to JS is a callback / Promise per I/O operation.
There is no "watch many fds, wake me when any is ready" syscall to
import. There is, however, the JSPI primitive: a wasm activation
can suspend on a Promise and resume when the Promise resolves.

That changes the locus of "who blocks on the I/O". On Linux the
runtime poller does, on a dedicated thread. On wasm3 **the Go
goroutine itself suspends on the JS Promise via JSPI** — the
runtime never blocks.

## The chosen design: per-pollDesc JSPI bridge (option A)

The runtime poller becomes vestigial. `netpoll(delay)` always
returns `(gList{}, 0)`. Every I/O syscall on wasm3 goes through a
wasm3-specific path that:

1. Issues the non-blocking syscall.
2. On `EAGAIN`, mints a fresh `parkID int32` for this wait.
3. Calls a suspending host import `runtime.AwaitFdReadable(fd, parkID)`
   (or `AwaitFdWritable`). The JS handler registers a
   `socket.once('readable', () => WasmReady(parkID))` and returns
   the resolver Promise.
4. The wasm activation suspends — V8 returns to the event loop.
5. When data arrives, the JS handler fires `WasmReady(parkID)` →
   the Promise resolves → wasm resumes.
6. The Go side retries the syscall, which now succeeds.

Cancellation and deadlines work the same way: on
`runtime_pollUnblock(pd)` or deadline expiry, Go calls `WasmReady`
on the parked ID to unstick the goroutine; the syscall then sees
`pd.closing` or a deadline error and returns the appropriate
`pollErr*`.

### Why option A over options B (single-poller) and C (skip)

- **B (one shared poller import that returns a list of ready fds)**
  needs multi-value returns through suspending imports, which the
  current `//go:wasmimport` lowering on wasm3 doesn't support — see
  the void-only comment in
  `runtime/wasm/jspi_jswasm3.go`. Even if that were fixed, the
  shared-poller batching has no benefit on a single-threaded host:
  V8 will deliver the wakes one-by-one through the event loop
  anyway.
- **C (no `internal/poll` integration at all — syscall stubs gopark
  directly)** is closest to the existing `chan_jswasm3.go` style
  but moves the I/O completion semantics fully into the
  `syscall/jswasm3` layer, requiring it to know about pollDesc
  state (closing, deadlines, fdseq) that `internal/poll` currently
  owns. Option A keeps the pollDesc as the source of truth for
  closing/deadline; only the *waiting* is wasm3-native.

## Component surface

### Go side

#### 1. `runtime/netpoll_wasm3.go` (Rung 20 — immediate)

A full mirror of `runtime/netpoll.go` per the
[[wasm3-always-full-mirror]] rule. The poller-platform functions
(`netpollinit` / `netpollopen` / `netpollclose` / `netpoll` /
`netpollBreak` / `netpollIsPollDescriptor` / `netpollarm`) are
no-ops or trivial returns, like `netpoll_fake.go`. The existing
`netpoll_jswasm3.go` is folded into this mirror.

Key adaptations in the mirror:

- `netpollblockcommit` (line 530 in upstream) does
  `atomic.Casuintptr((*uintptr)(gpp), pdWait, uintptr(unsafe.Pointer(gp)))`.
  On wasm3 we replace the body with a direct atomic store of a
  `parkID`-derived sentinel; the wasm3 wait path doesn't need to
  store the *g pointer in the semaphore because the parkID
  uniquely identifies which goroutine is waiting.

- The `runtime_pollWait` linkname entrypoint on wasm3 calls into a
  wasm3-native blocker (`wasm3PollWait`) instead of `netpollblock`.

#### 2. `runtime/wasm/io_jswasm3.go` (new — Rung 21+)

The wasm3 I/O suspending-import layer. Defines:

```go
//go:wasmimport gojs runtime.AwaitFdReadable
func awaitFdReadable(fd int32, parkID int32)

//go:wasmimport gojs runtime.AwaitFdWritable
func awaitFdWritable(fd int32, parkID int32)

//go:wasmimport gojs runtime.RegisterFd
func registerFd(fd int32, jsHandleKind int32)  // 0=net.Socket, 1=fs.FileHandle, ...
```

Plus a Go-side parkID allocator:

```go
var nextParkID atomic.Int32

func mintParkID() int32 {
    id := nextParkID.Add(1)
    if id == 0 || id == -1 { // reserve sentinels
        return mintParkID()
    }
    return id
}
```

#### 3. `internal/poll` integration (Rung 22+)

`internal/poll.runtime_pollWait` is `go:linkname`d to runtime.
On wasm3 the runtime implementation routes to `wasm3PollWait(pd, mode)`:

```go
func wasm3PollWait(pd *pollDesc, mode int) int32 {
    if err := netpollcheckerr(pd, int32(mode)); err != pollNoError {
        return err
    }
    parkID := mintParkID()
    pd.wasm3ParkID = parkID  // stash for unblock cancellation
    if mode == 'r' {
        wasm.AwaitFdReadable(int32(pd.fd), parkID)
    } else {
        wasm.AwaitFdWritable(int32(pd.fd), parkID)
    }
    pd.wasm3ParkID = 0
    return netpollcheckerr(pd, int32(mode)) // re-check for closed/deadline
}
```

`runtime_pollUnblock` calls `wasm.WasmReady(pd.wasm3ParkID)` so
that a closing or deadline-expired wait wakes promptly.

#### 4. `syscall/js_wasm3` adaptation (Rung 23+)

Today the `syscall/jswasm3` stub layer (task #177) returns
`ENOSYS` for `Read`/`Write`/etc. On wasm3 with netpoll wired,
these go non-blocking-by-default and pair with the
`internal/poll` integration above. The fd table itself moves to
JS (see below).

### JS side (`lib/wasm/wasm_exec_wasm3.js`)

Three new objects:

```javascript
// Handle table: maps fd (int32) to Node-side object.
const fdHandles = new Map(); // fd -> { kind: "net.Socket"|"fs.FileHandle"|..., handle: object, readable: bool, writable: bool }

function registerFd(fd, kind, handle) {
    fdHandles.set(fd, { kind, handle, readable: false, writable: false });
    // Wire up listeners that flip the readable/writable flags and
    // resolve any pending AwaitFd*Resolver.
    if (kind === 0) { // net.Socket
        handle.on("readable", () => { /* mark readable, resolve pending park */ });
        handle.on("drain",    () => { /* mark writable, resolve pending park */ });
        handle.on("close",    () => { fdHandles.delete(fd); /* resolve all pending */ });
    }
    // ... other kinds
}

// Per-fd-per-mode pending park registry.
const pendingFdReads  = new Map(); // fd -> parkID
const pendingFdWrites = new Map();

const awaitFdReadableSuspending = new WebAssembly.Suspending(
    function awaitFdReadable(fd, parkID) {
        const slot = fdHandles.get(fd);
        if (!slot || slot.readable) {
            return Promise.resolve(); // already readable, fast path
        }
        return new Promise((resolve) => {
            pendingFdReads.set(fd, { parkID, resolve });
        });
    },
);
// Symmetric awaitFdWritable.

// On the readable/drain/close listeners above, look up the pending
// park for this fd, call WasmReady(parkID), and the resolver fires.
```

## Lifecycle / edge cases

- **Pre-resolved wake (ready-before-park race).** Goready can fire
  before park completes — the standard `pdReady` /
  `pdWait` state machine handles this. On wasm3 the equivalent is
  the `parkResolvers.set(id, "ready")` marker that
  `wasm_exec_wasm3.js` already implements for `WasmPark` /
  `WasmReady`. The new `awaitFdReadable` uses the same Map; if a
  ready listener fires before the suspending import is called,
  the resolver is pre-stashed.

- **Multiple waiters on the same fd.** `pollDesc.rg` / `pollDesc.wg`
  enforce at most one read waiter and one write waiter at a time
  on every arch. wasm3 keeps that constraint (the
  `pollDesc` machinery is preserved) — the JS-side per-fd map
  only ever has one entry per (fd, mode) pair.

- **Deadline / close.** `runtime_pollSetDeadline` schedules a
  timer (already a `setTimeout` on wasm3 via the same JSPI
  plumbing); on fire it calls `runtime_pollUnblock` →
  `wasm.WasmReady(pd.wasm3ParkID)`. The waker wins the race; the
  goroutine resumes, sees `pollErrTimeout` from
  `netpollcheckerr`, and propagates the deadline error.

- **fd closed during wait.** `runtime_pollClose` flips
  `pd.closing` and calls unblock; same wake path as deadline.

- **GC of `pollDesc`.** `pollDesc` is `sys.NotInHeap`; the parkID
  is just an int32 stashed in the `pollDesc` and never on its own
  in the heap. JS-side resolver entries are owned by the Map and
  removed on wake.

## Out of scope (and intentionally so)

The browser is the primary target. Raw TCP / UDP sockets are not
available in a browser sandbox — `net.Dial("tcp", ...)` /
`net.Listen` are Node-only conveniences and not the priority. The
netpoll machinery described above (`AwaitFdReadable` /
`AwaitFdWritable`) is generic enough to back any
Promise-resolves-when-ready primitive, so the same plumbing can
later be wired to `fetch` body streams, WebSocket `message`
events, etc. — those are the browser-relevant follow-ons.

What this design does NOT address (separate workstreams):

- **`fetch` integration**: outbound HTTP via the browser/Node
  `fetch` API. The body stream's `ReadableStream.read()` returns
  a Promise; that's the natural pairing for an `AwaitStream`
  suspending import. Likely a new `runtime/wasm/fetch_jswasm3.go`
  built on the same parkID plumbing.
- **WebSocket**: bidirectional streaming via the browser
  `WebSocket` API. `onmessage` resolves a read; `bufferedAmount`
  drains drive a write.
- **`os` package file I/O**: `os.Open` / `os.File.Read` on a Node
  `fs` backend (Node-only — not browser).
- **`time.Timer` / `time.Sleep`**: route through `WasmPark` +
  setTimeout. The existing `SleepMs` import is the prototype.
- **TLS / DNS**: provided by the host (Node tls, browser fetch
  HTTPS); binding is its own workstream.
- **Raw TCP/UDP**: explicitly deferred. Possible later via Node
  `net.Socket`, but not the priority since the browser
  sandbox doesn't expose this.

## Milestone breakdown

- **Rung 20** — full mirror `netpoll.go` (wasm3-native poller stubs
  + `netpollblockcommit` adaptation). Just makes `./runtime`
  build green; no actual I/O works yet.
- **Rung 21** — `runtime/wasm/io_jswasm3.go` skeleton + JS-side
  handlers + parkID allocator. Doesn't wire to any syscall yet.
- **Rung 22** — `internal/poll` wasm3 integration: route
  `runtime_pollWait` through the new wasm3 blocker; smoke test
  with a hand-crafted "fake fd" that the JS side resolves
  immediately.
- **Rung 23** — `syscall/jswasm3` adaptation: non-blocking
  `Read` / `Write` paired with the netpoll machinery. First real
  I/O: a Go program that drives a JS-side stream (initially a
  hand-rolled test stream) through the runtime_pollWait →
  AwaitFdReadable → resolve → retry cycle.
- **Beyond** (separate designs, browser-first):
  - `fetch` for outbound HTTP — body reads via stream Promises
  - WebSocket for bidirectional messaging — `onmessage` resolves
  - file I/O on Node only — `fs.FileHandle` reads via Promises
  - TLS/DNS provided by host
  - raw TCP/UDP sockets — explicitly deferred (sandbox restricted)

The current goal scope (`/runtime` building green) is just
Rung 20.
