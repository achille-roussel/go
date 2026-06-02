# Rung 0 inventory — wasm3 runtime ptr-arith triage

Scope: every file in `runtime/` that ends up in the `GOOS=js GOARCH=wasm3`
build AND contains a forbidden pointer-arithmetic pattern caught by the new
SSA-gen wasm3 fatal (commit `3f174090e8`).

**56 ptr-arith call sites across 19 files,**
grouped into 10 subsystems below. **Each subsystem must move as a
unit** — verified by attempting to tag individual files `!wasm3` and observing
cascade link failures (taggedPointer used by lfstack, sysAllocOS used by mem.go,
pollDesc used by netpoll_jswasm3.go, etc.).

Status legend:

- **TAG_OFF** — dead code path on wasm3; can be excluded once the subsystem
  moves together. No wasm3-side replacement needed.
- **NEEDS_MIRROR** — actively used on wasm3; needs a parallel `_wasm3.go`
  implementation that uses WasmGC refs instead of uintptr-stored pointers.
- **NEEDS_REFACTOR** — patterns are mixed: some uses are dead, some are live.
  Splitting / per-function gating required.

## `scheduler` — NEEDS_MIRROR

_Rungs 1+2 (proc_wasm3.go extension; new runtime2_wasm3.go)_

15 call sites across 3 files:

  - `runtime/proc.go` (6 sites)
  - `runtime/sema.go` (5 sites)
  - `runtime/runtime2.go` (4 sites)

## `panic` — NEEDS_MIRROR

_Rung 4 / M5 phase 3 (cross-goroutine panic via resume_throw)_

4 call sites across 1 files:

  - `runtime/panic.go` (4 sites)

## `chan` — TAG_OFF

_Rung 3 (hchan path dead; per-T trampolines are live impl)_

1 call sites across 1 files:

  - `runtime/select.go` (1 sites)

## `iface` — NEEDS_REFACTOR

_Rung 5 / M6 (compiler-emitted per-type eq+hash; iface still needs ref-identity comparison)_

5 call sites across 2 files:

  - `runtime/iface.go` (4 sites)
  - `runtime/type.go` (1 sites)

## `mheap_gc` — TAG_OFF

_Already partly tagged off (task #128 GC exclusion). Finish the gating._

14 call sites across 3 files:

  - `runtime/mem_sbrk.go` (10 sites)
  - `runtime/tagptr_64bit.go` (3 sites)
  - `runtime/lfstack.go` (1 sites)

## `symtab` — TAG_OFF

_pclntab walking dead on wasm3 (task #166)._

5 call sites across 2 files:

  - `runtime/symtab.go` (3 sites)
  - `runtime/stkframe.go` (2 sites)

## `netpoll` — NEEDS_REFACTOR

_wasm3 needs JSPI-based netpoll, not the unix poller. Current `netpoll_jswasm3.go` is the stub home._

1 call sites across 1 files:

  - `runtime/netpoll.go` (1 sites)

## `cgo` — TAG_OFF

_cgo unsupported on wasm3._

3 call sites across 2 files:

  - `runtime/cgocheck.go` (2 sites)
  - `runtime/cgocall.go` (1 sites)

## `debuglog` — TAG_OFF

_in-memory ring buffer dead on wasm3._

3 call sites across 1 files:

  - `runtime/debuglog.go` (3 sites)

## `mem_ops` — NEEDS_REFACTOR

_string.go / slice.go ops are widely needed; some are intrinsified, some are not._

5 call sites across 3 files:

  - `runtime/string.go` (2 sites)
  - `runtime/stubs.go` (2 sites)
  - `runtime/runtime1.go` (1 sites)

