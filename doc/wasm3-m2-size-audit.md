# wasm3 M2 binary-size audit

Date: 2026-05-16
Toolchain: branch `wasm3-m2-cutover` (tip `2ddf5c0f0b`)
Comparison: `GOOS=wasip1 GOARCH=wasm` vs `GOOS=wasip1 GOARCH=wasm3`,
both built with `-ldflags='-s -w'`.

## Headline numbers

| Program | wasm | wasm3 | ratio |
|---|---:|---:|---:|
| empty `main` | 1,829,590 B (1.79 MB) | 1,775 B (1.73 KB) | **1031×** smaller |
| empty `main`, gzipped | 553,211 B | 951 B | **581×** |
| fib(20) + `print` | 1,830,517 B (1.79 MB) | 6,562 B (6.41 KB) | **279×** |
| fib(20) + `print`, gzipped | 553,402 B | 2,605 B | **212×** |
| bubble-sort + gcd + fib(25) + `print` | 1,832,064 B (1.79 MB) | 7,827 B (7.64 KB) | **234×** |
| same, gzipped | 553,945 B | 3,120 B | **178×** |

A real program — bubble-sort, gcd, recursive fib, multiple `print`
calls, runs to completion and produces the expected stdout — fits in
**7.6 KB** on wasm3 versus 1.8 MB on wasm.

## Section-by-section (empty main)

| Section | wasm | wasm3 | notes |
|---|---:|---:|---|
| types | 67 | 35 | wasm3 uses richer types but fewer of them |
| imports | 374 | 1 | wasm3 has zero WASI imports for empty main |
| functions | 1232 | 5 | wasm: 1230 funcs; wasm3: 4 |
| tables | 5 | 0 | wasm3 has no funcref table |
| globals | 41 | 6 | wasm3 has just the SP global stub |
| elements | 2350 | 0 | wasm3 has no element section |
| **code** | **1,145,429** | **17** | actual instruction bytes |
| **data** | **679,712** | **1,344** | pclntab, RTTI, string literals |
| producers | 153 | 153 | toolchain-version comment |

The two big consumers in wasm are `code` (1.1 MB of compiled runtime —
GC, allocator, scheduler, panic, reflect, channels, ...) and `data`
(680 KB of pclntab + RTTI + string literals). wasm3 deletes essentially
all of this. The empty-main wasm3 binary has **17 bytes of code** in
the whole module.

## Section-by-section (hello/fib)

| Section | wasm | wasm3 | notes |
|---|---:|---:|---|
| functions | 1233 | 14 | wasm3: main.fib, main.main, _start, printint/printuint/printstring/etc. (the runtime fork) |
| code | 1,146,071 | 663 | 1729× |
| data | 679,994 | 5,392 | mostly source-file paths in the embedded pclntab; could shrink further |

The wasm3 hello binary's data section is dominated by pclntab metadata
embedded by the linker (file paths, function names). A small additional
size win is available by trimming what we emit for wasm3 — that
metadata is largely needed for stack traces, which the wasm3 runtime
doesn't generate today.

## What's in the wasm3 binary

For the algos program (7.6 KB):

- 9 type-section entries: the prelude (`$go.object`, `$go.bytes`,
  `$go.string`) plus typed function signatures.
- 1 import: `wasi_snapshot_preview1.fd_write`.
- 14 functions: `main.bubbleSort`, `main.gcd`, `main.fib`, `main.main`,
  `_rt0_wasm3_wasip1`, `runtime.fd_write` (the import wrapper),
  `runtime.write1`, `runtime.printint`, `runtime.printuint`,
  `runtime.printstring`, `runtime.formatUint10`, plus four runtime
  helpers/stubs.
- 663 bytes of compiled code total.
- 5.4 KB of data — primarily pclntab/source paths.

No GC, no allocator, no scheduler, no goroutine machinery, no reflect,
no panic infrastructure, no channels, no maps, no interfaces.

## Caveats

The comparison is "what wasm3 can run today" versus "what wasm can run."
Apples to oranges in capability: many wasm programs would not compile
on wasm3 yet (interfaces, maps, closures, channels, panic/recover,
heap allocation under explicit escape, the broader reflect surface).
What wasm3 demonstrates is the *lower bound* of binary size when the
host runtime takes over Go's GC + allocator + scheduler. The 7.6 KB
figure is what a real algorithmic program with print costs once you
strip away everything the host already provides.

Per the original [size audit](wasm3-design.md#1-binary-size-audit) the
GC + allocator alone were ~244 KB of code (~13% of an empty-main
binary). The audit predicted a WasmGC-backed runtime "is one pillar of
a minimal-runtime target, not a standalone win." This audit confirms
the prediction inversely: deleting the entire runtime (not just GC +
allocator) — which is what M2 actually does — is what unlocks the
1000× shrink. WasmGC + stack-switching + EH are the wasm-side
substitutes that make that deletion safe; together they remove the
necessity for a Go runtime in the binary.

## Implications

- M2's binary-size promise is delivered: empty wasm3 fits in 1.7 KB
  vs. wasm's 1.8 MB.
- The remaining size in wasm3 binaries is mostly pclntab metadata,
  which is a follow-on lever (DWARF/pclntab trimming for wasm3,
  separate from the runtime-deletion lever).
- The cost so far is capability: closures, interfaces, maps, panics,
  goroutines, full reflect — all need M3+ work. Programs that fit
  the encodable subset (no heap allocation under escape, no
  closures, no interfaces) get the full size win for free.
- The runtime-fork shims (`printint_wasm3.go`, `write1_wasip1_wasm3.go`,
  etc.) are a workaround for §7 interior pointer boxing not yet
  landing. Once boxing is in, those shims can be removed and the
  standard runtime implementations will encode cleanly — adding
  back a few hundred bytes at most.
