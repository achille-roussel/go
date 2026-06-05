// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// wasm_exec_wasm3.js — minimal Node.js host for GOARCH=wasm3 GOOS=js
// (Phase 0).  Loads the .wasm passed as argv[2], instantiates with
// the import shape the Phase 0 wasm3 runtime needs (the gojs
// module's nanotime1 / walltime / resetMemoryDataView wasmimports
// declared in stubs3.go / timestub2.go / mem_jswasm3.go), and calls
// the `run` export.
//
// Phase 1 extends this with: JSPI-promising wrapping of `run`,
// JSPI-suspending wrappers for I/O imports, a real
// __wasm3_write(fd,bytes) stdout binding.
//
// Run:  node --experimental-wasm-stack-switching wasm_exec_wasm3.js <wasm>

"use strict";

const fs = require("fs");

if (process.argv.length < 3) {
    console.error("usage: node wasm_exec_wasm3.js <wasm-file>");
    process.exit(2);
}

const wasmPath = process.argv[2];
const wasmBytes = fs.readFileSync(wasmPath);

async function main() {
    const startNs = process.hrtime.bigint();

    // Phase 1 (JSPI): wrap each suspending wasm import via
    // WebAssembly.Suspending around a Promise-returning JS function.
    // The wasm-side call suspends the activation (a JSPI suspender
    // stack frame, host-managed) until the Promise resolves; then
    // the call returns. This is the engine-validated alternative to
    // wasmfx cont.new/resume/suspend.
    //
    // All Phase 1 imports are void-result — returning values
    // (especially externref) through //go:wasmimport on wasm3 has
    // a toolchain gap today; the void shape is sufficient to prove
    // JSPI works end-to-end and matches what Phase 3 gopark needs
    // anyway (the resolver's value isn't read by the Go scheduler).
    const sleepMsSuspending = new WebAssembly.Suspending(
        function sleepMs(ms) {
            return new Promise((resolve) => setTimeout(resolve, Number(ms)));
        },
    );

    // Phase 3 primitives: WasmPark suspends a wasm activation until
    // WasmReady(parkID) resolves the matching Promise. The Go-side
    // scheduler manages parkID — one per gopark call, derived from
    // the parked g's pointer/identifier — so that goready(g) can
    // look up and resolve its specific Promise.
    //
    // Pre-resolve semantics: if WasmReady fires BEFORE WasmPark
    // (the ready-before-park race the standard Go scheduler
    // tolerates), we stash the resolver-less "already ready" marker
    // and the matching WasmPark returns immediately.
    const parkResolvers = new Map(); // parkID -> resolver | "ready"

    const wasmParkSuspending = new WebAssembly.Suspending(
        function wasmPark(parkID) {
            const id = Number(parkID);
            const existing = parkResolvers.get(id);
            if (existing === "ready") {
                // ready-before-park: consume the marker and return.
                parkResolvers.delete(id);
                return Promise.resolve();
            }
            return new Promise((resolve) => {
                parkResolvers.set(id, resolve);
            });
        },
    );

    function wasmReady(parkID) {
        const id = Number(parkID);
        const r = parkResolvers.get(id);
        if (typeof r === "function") {
            parkResolvers.delete(id);
            r();
        } else {
            // Park hasn't happened yet — stash the marker.
            parkResolvers.set(id, "ready");
        }
    }

    // Rung 21 — per-fd JSPI bridge (doc/wasm3-netpoll-design.md).
    // V8 doesn't poll fds; Node's libuv emits 'readable' / 'drain' /
    // 'close' events per handle. fdHandles maps an integer fd to
    // {kind, handle, readable, writable, closed}. registerFd installs
    // the listeners that flip the flags and resolve any pending park.
    //
    // pendingFdReads / pendingFdWrites are the per-fd-per-mode park
    // registry: one entry per (fd, mode) — the pollDesc layer above
    // enforces at-most-one waiter, so there's no need for a list.
    // The entry's `resolve` matches the corresponding
    // AwaitFdReadable / AwaitFdWritable Suspending wrapper's Promise.
    //
    // Pre-resolve semantics match WasmPark / WasmReady: if the JS
    // side fires 'readable' before the wasm side calls
    // AwaitFdReadable (the ready-before-park race), the flag stays
    // set and the next await resolves immediately.
    const fdHandles = new Map(); // fd -> { kind, handle, readable, writable, closed }
    // pendingFdReads / pendingFdWrites map fd -> parkID. The
    // actual resolver lives in parkResolvers (shared with
    // WasmPark / WasmReady) — that lets WasmReady(parkID) from a
    // cancellation path (runtime_pollUnblock on close / deadline)
    // wake an AwaitFd* park without needing a separate per-fd
    // dispatch. fdSet{Readable,Writable,Closed} below do the
    // reverse lookup: fd → parkID → resolver.
    const pendingFdReads = new Map(); // fd -> parkID
    const pendingFdWrites = new Map(); // fd -> parkID

    function resolveParkID(parkID) {
        const r = parkResolvers.get(parkID);
        if (typeof r === "function") {
            parkResolvers.delete(parkID);
            r();
        } else {
            // Park hasn't happened yet — stash the marker so the
            // next AwaitFd*/WasmPark sees ready and fast-paths.
            parkResolvers.set(parkID, "ready");
        }
    }

    function fdSetReadable(fd) {
        const slot = fdHandles.get(fd);
        if (!slot) return;
        slot.readable = true;
        const parkID = pendingFdReads.get(fd);
        if (parkID !== undefined) {
            pendingFdReads.delete(fd);
            resolveParkID(parkID);
        }
    }

    function fdSetWritable(fd) {
        const slot = fdHandles.get(fd);
        if (!slot) return;
        slot.writable = true;
        const parkID = pendingFdWrites.get(fd);
        if (parkID !== undefined) {
            pendingFdWrites.delete(fd);
            resolveParkID(parkID);
        }
    }

    function fdSetClosed(fd) {
        const slot = fdHandles.get(fd);
        if (!slot) return;
        slot.closed = true;
        // Close races with both modes — resolve whichever side
        // is waiting. The Go side re-checks pd.closing after
        // the AwaitFd* import returns and propagates the right
        // pollErr.
        const rID = pendingFdReads.get(fd);
        if (rID !== undefined) {
            pendingFdReads.delete(fd);
            resolveParkID(rID);
        }
        const wID = pendingFdWrites.get(fd);
        if (wID !== undefined) {
            pendingFdWrites.delete(fd);
            resolveParkID(wID);
        }
        fdHandles.delete(fd);
    }

    // registerFd: Go's runtime.RegisterFd handoff. kind:
    //   0 — net.Socket (TCP/Unix stream)
    //   1 — fs.FileHandle (regular file)
    //   2 — process.stdin / stdout / stderr (pre-registered below)
    // The `handle` itself is bound separately by the kind-specific
    // wiring (e.g. a future net.connect helper would assign the
    // socket to fdHandles via this same map). For Rung 21 we just
    // record kind so later rungs (22 internal/poll, 23 syscall)
    // can dispatch.
    function registerFd(fd, kind) {
        if (fdHandles.has(fd)) return; // already registered (e.g. stdin pre-reg)
        fdHandles.set(fd, {
            kind: Number(kind),
            handle: null,
            readable: false,
            writable: false,
            closed: false,
        });
    }

    // Pre-register the standard streams so syscall.Read(0, ...) /
    // Write(1, ...) work without an explicit RegisterFd call.
    // stdout/stderr are always "writable" (Node buffers them
    // internally; drain backpressure is a Rung-23 concern).
    fdHandles.set(0, { kind: 2, handle: process.stdin, readable: false, writable: false, closed: false });
    fdHandles.set(1, { kind: 2, handle: process.stdout, readable: false, writable: true, closed: false });
    fdHandles.set(2, { kind: 2, handle: process.stderr, readable: false, writable: true, closed: false });

    const awaitFdReadableSuspending = new WebAssembly.Suspending(
        function awaitFdReadable(fd, parkID) {
            const f = Number(fd);
            const id = Number(parkID);
            const slot = fdHandles.get(f);
            if (!slot) {
                // No registered handle — resolve immediately so the
                // Go side sees its syscall return an error. Future
                // rungs may instead reject for a cleaner trap.
                return Promise.resolve();
            }
            // Pre-ready / pre-resolved fast paths.
            if (slot.readable || slot.closed) {
                slot.readable = false; // edge-triggered: consume the flag
                return Promise.resolve();
            }
            const existing = parkResolvers.get(id);
            if (existing === "ready") {
                // WasmReady(parkID) raced us — consume the marker.
                parkResolvers.delete(id);
                return Promise.resolve();
            }
            return new Promise((resolve) => {
                parkResolvers.set(id, resolve);
                pendingFdReads.set(f, id);
            });
        },
    );

    const awaitFdWritableSuspending = new WebAssembly.Suspending(
        function awaitFdWritable(fd, parkID) {
            const f = Number(fd);
            const id = Number(parkID);
            const slot = fdHandles.get(f);
            if (!slot) {
                return Promise.resolve();
            }
            if (slot.writable || slot.closed) {
                return Promise.resolve();
            }
            const existing = parkResolvers.get(id);
            if (existing === "ready") {
                parkResolvers.delete(id);
                return Promise.resolve();
            }
            return new Promise((resolve) => {
                parkResolvers.set(id, resolve);
                pendingFdWrites.set(f, id);
            });
        },
    );

    const importObject = {
        gojs: {
            // monotonic clock in nanoseconds since module start.
            "runtime.nanotime1": () => Number(process.hrtime.bigint() - startNs),
            // wall clock: (sec int64, nsec int32).
            "runtime.walltime": () => {
                const ms = Date.now();
                const sec = BigInt(Math.floor(ms / 1000));
                const nsec = (ms % 1000) * 1_000_000;
                return [sec, nsec];
            },
            // memory.grow notification — Phase 0 no-op.
            "runtime.resetMemoryDataView": () => {},

            // Phase 1 JSPI primitive: suspends the calling wasm
            // activation for ms milliseconds.
            "runtime.SleepMs": sleepMsSuspending,

            // Phase 3 gopark/goready primitives.
            "runtime.WasmPark": wasmParkSuspending,
            "runtime.WasmReady": wasmReady,

            // Rung 21 per-fd JSPI bridge (doc/wasm3-netpoll-design.md).
            // AwaitFdReadable/Writable suspend the wasm activation
            // until the per-fd handler resolves the matching parkID.
            // RegisterFd records the kind so Rungs 22 (internal/poll)
            // and 23 (syscall non-blocking Read/Write) can dispatch.
            // The Rung 21 commit wires the seam only; no syscall
            // currently routes through it yet.
            "runtime.AwaitFdReadable": awaitFdReadableSuspending,
            "runtime.AwaitFdWritable": awaitFdWritableSuspending,
            "runtime.RegisterFd": registerFd,

            // Phase 3 self-test (TEMPORARY): SelfWakeMs(parkID)
            // schedules a setTimeout that will call WasmReady(parkID)
            // after a fixed 100ms. Lets the wasm side test the
            // WasmPark/WasmReady pair end-to-end via a single-param
            // import while the //go:wasmimport multi-param wrapper
            // gap on wasm3 is being investigated separately. Phase 4
            // replaces this with a real second-goroutine path.
            "runtime.SelfWakeMs": (parkID) => {
                setTimeout(() => wasmReady(parkID), 100);
            },

            // Phase 1 visibility helper — wasm3 write1Bytes through
            // a JS import lands in Phase 2; until then LogInt is the
            // simplest way to observe execution.
            "runtime.LogInt": (n) => process.stdout.write(`log ${n}\n`),

            // Clean program termination — M5 phase 1B uses this when
            // a panic is recovered cleanly (without proper wasm-frame
            // unwinding via try_table+throw_ref the panicking
            // function's frame can't return, so wasm3Gopanic exits
            // the program after defers ran). Bound to runtime.exit
            // on the wasm3 side.
            "runtime.Exit": (code) => process.exit(code),

            // Per-goroutine footprint measurement helper (M4 Phase E):
            // emit a tagged process.memoryUsage() snapshot to stderr.
            // Bash driver scrapes the lines to compute slope per
            // spawned goroutine across N runs.
            "runtime.memSnap": (label) => {
                const m = process.memoryUsage();
                process.stderr.write(
                    `memsnap label=${label} rss=${m.rss} heapUsed=${m.heapUsed} heapTotal=${m.heapTotal} external=${m.external} arrayBuffers=${m.arrayBuffers}\n`,
                );
            },

            // Phase 2 write path: the wasm side stages bytes via
            // WriteLinearMemory and hands the (fd, off, n) triple to
            // this import. fd 1 → stdout, fd 2 → stderr; anything
            // else is silently dropped to match the host's
            // runtime.write policy (os_js.go writes fd > 2 are not
            // supported).
            "runtime.writeBytes": (fd, off, n) => {
                if (n === 0) return;
                const mem = instance.exports.mem.buffer;
                const view = new Uint8Array(mem, Number(off), Number(n));
                if (fd === 1) {
                    process.stdout.write(view);
                } else if (fd === 2) {
                    process.stderr.write(view);
                }
            },
        },
    };

    // Phase 4 spawn machinery: SpawnGoroutine(id) tells the host
    // to schedule a fresh promising-wrapped _run_wasm3_js(id) call
    // on the next microtask. Each such call is its own JSPI
    // suspender — concurrent independently-suspendable wasm
    // activations.
    //
    // promisingRunWasm3JS is created after instantiation because
    // it references the export.
    let promisingRunWasm3JS = null;
    importObject.gojs["runtime.SpawnGoroutine"] = (id) => {
        // Schedule the new goroutine at the next *task* boundary
        // (setImmediate), not the next microtask. This is what
        // gives "main returns => process exits immediately"
        // semantics: when main.main returns without ever yielding,
        // run() resolves and main()'s await-continuation + the
        // .then(process.exit) callback are both microtasks queued
        // after the wasm activation finishes. Microtasks drain
        // fully before setImmediate fires, so process.exit lands
        // first and the spawned goroutine is dropped on the floor
        // (along with every other pending host resource). When
        // main DOES yield (e.g. via WasmPark), it parks on a
        // Promise; the event loop processes microtasks (none new),
        // then immediates — the spawn fires while main is parked,
        // which is the case we want it to.
        setImmediate(() => {
            if (promisingRunWasm3JS) {
                promisingRunWasm3JS(Number(id)).catch((e) => {
                    console.error("_run_wasm3_js failed:", e);
                });
            }
        });
    };

    const { module: mod, instance } = await WebAssembly.instantiate(wasmBytes, importObject);
    const rawRun = instance.exports.run;
    if (typeof rawRun !== "function") {
        console.error("error: wasm module does not export `run`");
        process.exit(1);
    }
    // Phase 2 (JSPI promising): wrap the entry export so the wasm
    // activation runs on a JSPI suspender stack. Without this,
    // calling any `WebAssembly.Suspending` import from inside
    // raises `SuspendError: trying to suspend without
    // WebAssembly.promising`. Returns a Promise that resolves when
    // wasm `run` returns.
    const run = WebAssembly.promising(rawRun);

    // Phase 4: wrap _run_wasm3_js as promising for the spawn path.
    if (typeof instance.exports._run_wasm3_js === "function") {
        promisingRunWasm3JS = WebAssembly.promising(instance.exports._run_wasm3_js);
    }

    await run();
}

// Go program semantics: when main returns, the program exits
// immediately. Any in-flight goroutines (pending microtasks for
// _run_wasm3_js, parked WasmPark Promises, queued timers from
// SleepMs) are dropped along with the rest of process state.
// `go fn()` does NOT keep the program alive past main, matching
// the runtime behavior of every other Go target. os.Exit will
// land here too once it's wired up.
main()
    .then(() => process.exit(0))
    .catch((err) => {
        console.error(err);
        process.exit(1);
    });
