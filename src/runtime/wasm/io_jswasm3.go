// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// io_jswasm3.go — wasm3 I/O suspending-import layer (Rung 21 of
// doc/wasm3-netpoll-design.md).
//
// The wasm3 host is V8 / Node; V8 doesn't expose an epoll-style
// "watch many fds, wake me on any" syscall. What it does expose is
// JSPI: a wasm activation can suspend on a Promise. That changes
// who blocks on the I/O — the Go goroutine itself suspends on a
// Promise the JS side resolves when its libuv-backed handle fires
// `readable` / `drain` / `close`. The runtime poller becomes
// vestigial (netpoll always returns an empty gList); every
// blocking I/O syscall on wasm3 goes through the AwaitFd*
// suspending imports below.
//
// The parkID protocol mirrors the existing WasmPark / WasmReady
// pair (jspi_jswasm3.go): the Go side mints a fresh int32 parkID
// per wait, the JS side records (parkID, Promise resolver) in a
// per-fd registry, and on readable/drain the resolver fires —
// JSPI returns the import and the Go syscall retries. Cancellation
// (closing fd, deadline expiry) calls WasmReady(parkID) on the
// stashed ID; the goroutine resumes, netpollcheckerr sees the
// closing/deadline state, and the syscall propagates the right
// pollErr.
//
// This file is the seam — no Go-side I/O wiring yet. Rungs 22 and
// 23 plumb internal/poll's runtime_pollWait and syscall/jswasm3's
// non-blocking Read/Write through to these primitives.

package wasm


// AwaitFdReadable suspends the calling wasm activation until fd
// has data ready to read, then resumes. The JS handler registered
// for fd flips its readable flag on Node's `readable` event and
// resolves the matching parkID's Promise. If fd is already
// readable at call time, the Promise resolves immediately (fast
// path; no real suspend). parkID identifies which suspended
// activation to resume — same protocol as WasmPark / WasmReady.
//
// Pre-resolve semantics: if the JS side called WasmReady(parkID)
// before AwaitFdReadable is invoked (a closing/deadline that
// raced the wait), the parkResolvers map has a "ready" marker
// and the import returns immediately. Same race tolerance as
// WasmPark.
//
//go:wasmimport gojs runtime.AwaitFdReadable
//go:noescape
func AwaitFdReadable(fd int32, parkID int32)

// AwaitFdWritable is the writer-side analogue of AwaitFdReadable.
// The JS handler flips writable on Node's `drain` event.
//
//go:wasmimport gojs runtime.AwaitFdWritable
//go:noescape
func AwaitFdWritable(fd int32, parkID int32)

// RegisterFd tells the JS side that an fd is now backed by a
// host-side handle of the given kind, so it can install the
// per-fd `readable` / `drain` / `close` listeners that drive
// AwaitFdReadable / AwaitFdWritable. Kinds:
//
//	0 — net.Socket (TCP/Unix stream)
//	1 — fs.FileHandle (regular file)
//	2 — process.stdin / .stdout / .stderr (pre-registered by JS)
//	3 — mock stream (test fixture; bytes pushed via runtime.MockPushFd)
//
// More kinds (WebSocket, fetch ReadableStream) follow in their
// own workstreams per the netpoll design "out of scope" list.
// The handle itself is identified by fd; how JS materializes the
// handle is the JS side's business (a Map keyed by fd).
//
//go:wasmimport gojs runtime.RegisterFd
//go:noescape
func RegisterFd(fd int32, jsHandleKind int32)

// ReadFd pulls up to n bytes of ready data from fd's host-side
// queue into the linear-memory scratch arena at offset off, and
// returns the count actually written. A return of -1 means the
// fd has no data ready right now (EAGAIN — the caller should
// AwaitFdReadable and retry); 0 means EOF / closed. -2 means
// "unknown fd" — registration must happen first.
//
// The caller stages the destination buffer via WriteLinearMemory
// (typically a length-only marker; ReadFd overwrites). After the
// call the bytes can be copied back into a Go-side slice via
// ReadLinearMemory, then ResetLinearMemory frees the scratch.
//
//go:wasmimport gojs runtime.ReadFd
//go:noescape
func ReadFd(fd int32, off uint32, n int32) int32

// WriteFd is the non-blocking write analogue. Returns bytes
// written, -1 (EAGAIN) if the destination cannot accept more
// data right now (caller should AwaitFdWritable + retry), -2
// for unknown fd. The bytes themselves are read from the
// linear-memory arena at (off, n) — same staging discipline as
// the existing runtime.writeBytes import for fd 1 / 2.
//
//go:wasmimport gojs runtime.WriteFd
//go:noescape
func WriteFd(fd int32, off uint32, n int32) int32

// MockPushFd is the test fixture: pushes (off, n) bytes from
// the linear-memory arena onto fd's read queue, then fires
// fdSetReadable so a currently-suspended AwaitFdReadable wakes
// and the next ReadFd dequeues the pushed bytes. The fd must
// have been registered via RegisterFd with kind 3 (mock stream).
// Production code never uses this — it's the Rung 23 end-to-end
// driver until real net.Socket / fs.FileHandle wiring lands.
//
//go:wasmimport gojs runtime.MockPushFd
//go:noescape
func MockPushFd(fd int32, off uint32, n int32)

// nextParkID hands out fresh wait identifiers. Sentinel values
// 0 and -1 are reserved so callers can distinguish "no park" from
// a real park (0 is the zero-value of pollDesc.wasm3ParkID;
// -1 reserved for future "already woken" marker if needed).
//
// wasm3 is single-threaded so a plain int32 suffices — no
// internal/runtime/atomic is needed here, and avoiding it
// sidesteps a wasm3 backend codegen issue with the
// atomic.Int32.Add lowering surfaced during Rung 21 bring-up.
var nextParkID int32

// MintParkID returns a fresh int32 parkID, skipping the reserved
// sentinels 0 and -1. The runtime caller stashes this in the
// pollDesc (or wherever the wait state lives) so the JS side has
// a stable handle to resolve later.
//
//go:nosplit
func MintParkID() int32 {
	for {
		nextParkID++
		id := nextParkID
		if id != 0 && id != -1 {
			return id
		}
	}
}
