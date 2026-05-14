;; wasm3 M2 spike — the target WasmGC module shape for GOOS=wasip1 GOARCH=wasm3.
;;
;; Models this Go program:
;;
;;   package main
;;   type point struct { x, y int64; next *point }
;;   func main() {
;;       p := &point{x: 21, y: 21}        // struct.new on the host GC heap
;;       msg := "wasm3 ok\n"              // a GC (array i8)
;;       os.Stdout.Write([]byte(msg))     // GC -> linear copy, then WASI fd_write
;;       // exit code = p.x + p.y - 42  (0 iff struct.new/struct.get worked)
;;   }
;;
;; Demonstrates every shape M2 must produce: the $go.object supertype + a
;; self-recursive struct in a rec group, struct.new/struct.get, a GC byte
;; array, the stack-discipline bump allocator, the mandatory GC->linear copy
;; for WASI I/O, typed functions (no (i32)->i32 ABI), and the degenerate
;; _start entry with no scheduler / no PC_F/PC_B loop.

(module
  ;; --- type section -------------------------------------------------------
  ;; $go.object is the open base every Go heap object subtypes. $point is
  ;; self-recursive (the $next field), so it genuinely needs the rec group —
  ;; this is the common shape for real Go linked structures.
  (rec
    (type $go.object (sub (struct)))
    (type $point (sub $go.object (struct
      (field $x (mut i64))
      (field $y (mut i64))
      (field $next (mut (ref null $point)))))))
  ;; String/[]byte backing.
  (type $go.bytes (array (mut i8)))

  ;; --- imports ------------------------------------------------------------
  (import "wasi_snapshot_preview1" "fd_write"
    (func $fd_write (param i32 i32 i32 i32) (result i32)))
  (import "wasi_snapshot_preview1" "proc_exit"
    (func $proc_exit (param i32)))

  ;; --- linear memory: minimal, just bump-allocator scratch for I/O --------
  (memory (export "memory") 1)                  ;; one 64 KiB page
  (global $linearSP (mut i32) (i32.const 65536)) ;; bump pointer, grows down

  ;; The string literal lives as a passive data segment; array.new_data turns
  ;; it into a GC (array i8) at runtime — the M2 lowering for a string const.
  (data $msg "wasm3 ok\n")                       ;; 9 bytes

  ;; --- runtime: the stack-discipline bump allocator (all of mem_wasm3.go) -
  ;; linAlloc bumps $linearSP down by n (8-byte aligned) and returns the base.
  ;; "Freeing" is just restoring a saved $linearSP mark.
  (func $linAlloc (param $n i32) (result i32)
    (global.set $linearSP
      (i32.sub (global.get $linearSP)
               (i32.and (i32.add (local.get $n) (i32.const 7))
                        (i32.const 0xfffffff8))))
    (global.get $linearSP))

  ;; --- main.main ----------------------------------------------------------
  ;; A normal typed wasm function: no PC_B param, no unwind-flag result.
  ;; Returns p.x + p.y so the caller can observe that the GC struct worked.
  (func $main (result i64)
    (local $p    (ref $point))
    (local $msg  (ref $go.bytes))
    (local $len  i32)
    (local $i    i32)
    (local $base i32)   ;; linear scratch holding the copied message bytes
    (local $iov  i32)   ;; linear scratch for the iovec + nwritten slot
    (local $mark i32)   ;; saved bump-allocator mark

    ;; p := &point{x: 21, y: 21, next: nil}  -- host-GC allocation
    (local.set $p
      (struct.new $point (i64.const 21) (i64.const 21) (ref.null $point)))

    ;; msg := "wasm3 ok\n" as a GC (array i8)
    (local.set $len (i32.const 9))
    (local.set $msg
      (array.new_data $go.bytes $msg (i32.const 0) (local.get $len)))

    ;; --- WASI write: copy the GC array into linear scratch, fd_write, pop --
    (local.set $mark (global.get $linearSP))
    (local.set $base (call $linAlloc (local.get $len)))
    (local.set $iov  (call $linAlloc (i32.const 12)))  ;; 8B iovec + 4B nwritten

    ;; for i := 0; i < len; i++ { linmem[base+i] = msg[i] }
    (local.set $i (i32.const 0))
    (block $done
      (loop $copy
        (br_if $done (i32.ge_u (local.get $i) (local.get $len)))
        (i32.store8
          (i32.add (local.get $base) (local.get $i))
          (array.get_u $go.bytes (local.get $msg) (local.get $i)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $copy)))

    ;; iovec{ buf: base, len: len }
    (i32.store          (local.get $iov) (local.get $base))
    (i32.store offset=4 (local.get $iov) (local.get $len))
    ;; fd_write(stdout=1, iovs=iov, iovs_len=1, nwritten=iov+8)
    (drop (call $fd_write
      (i32.const 1) (local.get $iov) (i32.const 1)
      (i32.add (local.get $iov) (i32.const 8))))

    ;; restore the bump pointer — stack discipline
    (global.set $linearSP (local.get $mark))

    ;; return p.x + p.y  (exercises struct.get on the host GC heap)
    (i64.add
      (struct.get $point $x (local.get $p))
      (struct.get $point $y (local.get $p))))

  ;; --- _rt0_wasm3_wasip1: the degenerate entry ----------------------------
  ;; Exported as "_start" (what the linker does). No scheduler, no g/m dance,
  ;; no PC_F/PC_B loop: just call main and exit. Exit code is p.x+p.y-42, so
  ;; the run exits 0 only if struct.new + struct.get produced 42.
  (func $_rt0_wasm3_wasip1 (export "_start")
    (call $proc_exit
      (i32.wrap_i64 (i64.sub (call $main) (i64.const 42)))))
)
