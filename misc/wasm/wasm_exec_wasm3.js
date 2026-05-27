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
        },
    };

    // Phase 4 spawn machinery: SpawnGoroutine(id) tells the host
    // to schedule a fresh promising-wrapped goroutine_run(id) call
    // on the next microtask. Each such call is its own JSPI
    // suspender — concurrent independently-suspendable wasm
    // activations.
    //
    // promisingGoroutineRun is created after instantiation because
    // it references the export.
    let promisingGoroutineRun = null;
    importObject.gojs["runtime.SpawnGoroutine"] = (id) => {
        // Use queueMicrotask so the spawning call returns before
        // the new goroutine starts. The new goroutine runs on its
        // own JSPI suspender via the promising wrapper.
        queueMicrotask(() => {
            if (promisingGoroutineRun) {
                promisingGoroutineRun(Number(id)).catch((e) => {
                    console.error("goroutine_run failed:", e);
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

    // Phase 4: wrap goroutine_run as promising for the spawn path.
    if (typeof instance.exports.goroutine_run === "function") {
        promisingGoroutineRun = WebAssembly.promising(instance.exports.goroutine_run);
    }

    await run();
}

main().catch((err) => {
    console.error(err);
    process.exit(1);
});
