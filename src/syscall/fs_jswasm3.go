// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build js && wasm3

// js/wasm3 fs stubs. The js/wasm version of this file in fs_js.go
// wires every syscall through the JS bridge in syscall/js, which
// can't be used on wasm3 (its //go:wasmimport surface uses parameter
// types — int / []ref / []byte — that wasm3's per-ref wasmimport
// validation rejects). For now, return ENOSYS for everything except
// the stdio writes that test output and println rely on.
//
// Stdout/stderr writes (fd 1, 2) forward to runtime.write1Bytes,
// which on js/wasm3 stages the slice through the linear-memory
// bridge and calls the host's runtime.writeBytes import (defined in
// lib/wasm/wasm_exec_wasm3.js). Reads from fd 0 and any operation
// on a real file path / fd return ENOSYS.

package syscall

import (
	"sync"
	_ "unsafe" // for go:linkname
)

// Provided by package runtime.
func now() (sec int64, nsec int32)

// write1Bytes is the runtime's wasm3 stdio write path. Pulled via
// go:linkname so the syscall package doesn't have to import runtime.
//
//go:linkname runtime_write1Bytes runtime.write1Bytes
func runtime_write1Bytes(fd uintptr, b []byte) int32

var (
	openFiles  = map[int]*stubFile{}
	openFilesM sync.Mutex
	nextFd     = 3
)

type stubFile struct {
	path string
}

func Open(path string, openmode int, perm uint32) (int, error)            { return -1, ENOSYS }
func Close(fd int) error                                                  { return ENOSYS }
func CloseOnExec(fd int)                                                  {}
func Mkdir(path string, perm uint32) error                                { return ENOSYS }
func ReadDirent(fd int, buf []byte) (int, error)                          { return 0, ENOSYS }
func Stat(path string, st *Stat_t) error                                  { return ENOSYS }
func Lstat(path string, st *Stat_t) error                                 { return ENOSYS }
func Fstat(fd int, st *Stat_t) error                                      { return ENOSYS }
func Unlink(path string) error                                            { return ENOSYS }
func Rmdir(path string) error                                             { return ENOSYS }
func Chmod(path string, mode uint32) error                                { return ENOSYS }
func Fchmod(fd int, mode uint32) error                                    { return ENOSYS }
func Chown(path string, uid, gid int) error                               { return ENOSYS }
func Fchown(fd int, uid, gid int) error                                   { return ENOSYS }
func Lchown(path string, uid, gid int) error                              { return ENOSYS }
func UtimesNano(path string, ts []Timespec) error                         { return ENOSYS }
func Rename(from, to string) error                                        { return ENOSYS }
func Truncate(path string, length int64) error                            { return ENOSYS }
func Ftruncate(fd int, length int64) error                                { return ENOSYS }
func Getcwd(buf []byte) (n int, err error)                                { return 0, ENOSYS }
func Chdir(path string) error                                             { return ENOSYS }
func Fchdir(fd int) error                                                 { return ENOSYS }
func Readlink(path string, buf []byte) (n int, err error)                 { return 0, ENOSYS }
func Link(path, link string) error                                        { return ENOSYS }
func Symlink(path, link string) error                                     { return ENOSYS }
func Fsync(fd int) error                                                  { return ENOSYS }
func Read(fd int, b []byte) (int, error)                                  { return 0, ENOSYS }
func Pread(fd int, b []byte, offset int64) (int, error)                   { return 0, ENOSYS }
func Pwrite(fd int, b []byte, offset int64) (int, error)                  { return 0, ENOSYS }
func Seek(fd int, offset int64, whence int) (int64, error)                { return 0, ENOSYS }
func Dup(fd int) (int, error)                                             { return 0, ENOSYS }
func Dup2(fd, newfd int) error                                            { return ENOSYS }
func Pipe(fd []int) error                                                 { return ENOSYS }

// Write is the one fs syscall we honor on wasm3: fd 1 → stdout,
// fd 2 → stderr, everything else ENOSYS. The runtime bridge handles
// the actual bytes-to-host transfer.
func Write(fd int, b []byte) (int, error) {
	if fd != 1 && fd != 2 {
		return 0, ENOSYS
	}
	n := runtime_write1Bytes(uintptr(fd), b)
	if n < 0 {
		return 0, EIO
	}
	return int(n), nil
}
