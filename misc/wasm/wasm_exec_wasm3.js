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
    const importObject = {
        gojs: {
            // monotonic clock in nanoseconds since module start (so
            // value fits in int64 for a long time).
            "runtime.nanotime1": () => Number(process.hrtime.bigint() - startNs),
            // wall clock: split a Date.now() value into (seconds, nanoseconds).
            "runtime.walltime": () => {
                const ms = Date.now();
                const sec = BigInt(Math.floor(ms / 1000));
                const nsec = (ms % 1000) * 1_000_000;
                // The Go side declares this as (sec int64, nsec int32) but
                // wasm imports use multi-value; here we return an object the
                // host engine maps. For Node, BigInt for i64 and Number for i32.
                return [sec, nsec];
            },
            // memory.grow notification — Phase 0 no-op, since the wasm3
            // backend rebinds its own DataView equivalent for the bridge.
            "runtime.resetMemoryDataView": () => {},
        },
    };

    const { module: mod, instance } = await WebAssembly.instantiate(wasmBytes, importObject);
    const run = instance.exports.run;
    if (typeof run !== "function") {
        console.error("error: wasm module does not export `run`");
        process.exit(1);
    }
    run();
}

main().catch((err) => {
    console.error(err);
    process.exit(1);
});
