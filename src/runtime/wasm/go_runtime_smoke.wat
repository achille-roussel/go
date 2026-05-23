;; go_runtime_smoke — engine-runnable assertion harness for the
;; primitives in go_runtime.wat.
;;
;; This module declares the SAME singleton rec-group prelude
;; ($go.object / $go.bytes / $go.string) as go_runtime.wat — structural
;; type matching across the wasm module boundary is what lets a string
;; constructed here cross the import boundary into go_runtime's
;; primitives. If the prelude types ever drift, the import signatures
;; fail to link with a clear type-mismatch error.
;;
;; Each test case constructs the inputs, calls the primitive, and traps
;; (unreachable) on assertion failure. `_start` runs them in sequence;
;; a clean exit means every assertion held.
;;
;; Build + run:
;;   wasm-tools parse go_runtime.wat -o /tmp/gort.wasm
;;   wasm-tools parse go_runtime_smoke.wat -o /tmp/smoke.wasm
;;   wasmtime -W gc=y -W function-references=y \
;;     --preload go_runtime=/tmp/gort.wasm /tmp/smoke.wasm
;; Exit 0 = all assertions held; trap = failure (line in trace).
;;
;; This is hand-validation infrastructure for the M3 boxed bulk-ops
;; primitive layer ([[wasm3-boxed-bulkops]]). It is NOT part of any
;; Go build — the Go toolchain does not compile .wat files.
(module $go_runtime_smoke
  (rec (type $go.object (sub (struct))))
  (rec (type $go.bytes  (sub (array (mut i8)))))
  (rec (type $go.string (sub $go.object (struct
    (field (ref $go.bytes))
    (field i64)
    (field i64)))))

  (import "go_runtime" "strcmp"
    (func $strcmp (param (ref null $go.string)) (param (ref null $go.string)) (result i32)))
  (import "go_runtime" "stringEqual"
    (func $stringEqual (param (ref null $go.string)) (param (ref null $go.string)) (result i32)))
  (import "go_runtime" "bytesEqualRange"
    (func $bytesEqualRange (param (ref null $go.bytes)) (param i64)
                            (param (ref null $go.bytes)) (param i64)
                            (param i64) (result i32)))
  (import "go_runtime" "bytesClone"
    (func $bytesClone (param (ref null $go.bytes)) (param i64) (param i64) (result (ref $go.bytes))))
  (import "go_runtime" "stringHash"
    (func $stringHash (param (ref null $go.string)) (param i64) (result i64)))
  (import "go_runtime" "stringConcat2"
    (func $stringConcat2 (param (ref null $go.string)) (param (ref null $go.string)) (result (ref $go.string))))
  (import "go_runtime" "WriteLinearMemory"
    (func $WriteLinearMemory (param i32) (param (ref null $go.bytes)) (result i32)))
  (import "go_runtime" "ReadLinearMemory"
    (func $ReadLinearMemory (param i32) (param i32) (param i32) (result (ref $go.bytes))))
  (import "go_runtime" "ResetLinearMemory"
    (func $ResetLinearMemory (param i32) (param i32)))

  ;; makeBytes($a, $b, $c) -> a freshly-allocated 3-byte $go.bytes
  ;; holding {a, b, c} (i32 byte values). Used by the test cases to
  ;; build prelude bytes arrays inline.
  (func $makeBytes3 (param $a i32) (param $b i32) (param $c i32) (result (ref $go.bytes))
    (local $r (ref $go.bytes))
    (local.set $r (array.new_default $go.bytes (i32.const 3)))
    (array.set $go.bytes (local.get $r) (i32.const 0) (local.get $a))
    (array.set $go.bytes (local.get $r) (i32.const 1) (local.get $b))
    (array.set $go.bytes (local.get $r) (i32.const 2) (local.get $c))
    (local.get $r))

  (func $makeBytes5 (param $a i32) (param $b i32) (param $c i32)
                    (param $d i32) (param $e i32) (result (ref $go.bytes))
    (local $r (ref $go.bytes))
    (local.set $r (array.new_default $go.bytes (i32.const 5)))
    (array.set $go.bytes (local.get $r) (i32.const 0) (local.get $a))
    (array.set $go.bytes (local.get $r) (i32.const 1) (local.get $b))
    (array.set $go.bytes (local.get $r) (i32.const 2) (local.get $c))
    (array.set $go.bytes (local.get $r) (i32.const 3) (local.get $d))
    (array.set $go.bytes (local.get $r) (i32.const 4) (local.get $e))
    (local.get $r))

  ;; mkString(bytes, off, len) -> (ref $go.string)
  (func $mkString (param $b (ref $go.bytes)) (param $o i64) (param $n i64) (result (ref $go.string))
    (struct.new $go.string (local.get $b) (local.get $o) (local.get $n)))

  ;; ==========================
  ;; Test runner
  ;; ==========================
  (func (export "_start")
    (local $abc (ref $go.string))
    (local $abc2 (ref $go.string))
    (local $abd (ref $go.string))
    (local $ab (ref $go.string))
    (local $empty (ref $go.string))
    (local $cat (ref $go.string))
    (local $hashed i64)
    (local $off1 i32) (local $off2 i32) (local $off3 i32)
    (local $read (ref $go.bytes)) (local $de (ref $go.bytes))

    ;; strings: "abc", "abc"(separate backing), "abd", "ab", ""
    (local.set $abc  (call $mkString (call $makeBytes3 (i32.const 97) (i32.const 98) (i32.const 99))
                                     (i64.const 0) (i64.const 3)))
    (local.set $abc2 (call $mkString (call $makeBytes3 (i32.const 97) (i32.const 98) (i32.const 99))
                                     (i64.const 0) (i64.const 3)))
    (local.set $abd  (call $mkString (call $makeBytes3 (i32.const 97) (i32.const 98) (i32.const 100))
                                     (i64.const 0) (i64.const 3)))
    ;; "ab" reuses abc's backing, offset 0 length 2
    (local.set $ab   (call $mkString (struct.get $go.string 0 (local.get $abc))
                                     (i64.const 0) (i64.const 2)))
    (local.set $empty (call $mkString (array.new_default $go.bytes (i32.const 0))
                                      (i64.const 0) (i64.const 0)))

    ;; ----- strcmp -----
    (if (i32.ne (call $strcmp (local.get $abc)  (local.get $abc2)) (i32.const  0)) (then (unreachable)))
    (if (i32.ne (call $strcmp (local.get $abc)  (local.get $abd)) (i32.const -1)) (then (unreachable)))
    (if (i32.ne (call $strcmp (local.get $abd)  (local.get $abc)) (i32.const  1)) (then (unreachable)))
    (if (i32.ne (call $strcmp (local.get $abc)  (local.get $ab))  (i32.const  1)) (then (unreachable)))
    (if (i32.ne (call $strcmp (local.get $ab)   (local.get $abc)) (i32.const -1)) (then (unreachable)))
    (if (i32.ne (call $strcmp (ref.null $go.string) (ref.null $go.string)) (i32.const 0)) (then (unreachable)))
    (if (i32.ne (call $strcmp (ref.null $go.string) (local.get $abc)) (i32.const -1)) (then (unreachable)))
    (if (i32.ne (call $strcmp (local.get $abc) (ref.null $go.string)) (i32.const  1)) (then (unreachable)))

    ;; ----- stringEqual -----
    (if (i32.ne (call $stringEqual (local.get $abc)  (local.get $abc2)) (i32.const 1)) (then (unreachable)))
    (if (i32.ne (call $stringEqual (local.get $abc)  (local.get $abd))  (i32.const 0)) (then (unreachable)))
    (if (i32.ne (call $stringEqual (local.get $abc)  (local.get $ab))   (i32.const 0)) (then (unreachable)))
    (if (i32.ne (call $stringEqual (ref.null $go.string) (ref.null $go.string)) (i32.const 1)) (then (unreachable)))
    (if (i32.ne (call $stringEqual (ref.null $go.string) (local.get $empty))    (i32.const 1)) (then (unreachable)))
    (if (i32.ne (call $stringEqual (ref.null $go.string) (local.get $abc))      (i32.const 0)) (then (unreachable)))

    ;; ----- bytesEqualRange -----
    ;; equal whole-array
    (if (i32.ne (call $bytesEqualRange
                  (struct.get $go.string 0 (local.get $abc)) (i64.const 0)
                  (struct.get $go.string 0 (local.get $abc2)) (i64.const 0)
                  (i64.const 3)) (i32.const 1)) (then (unreachable)))
    ;; equal sub-range: abc[0..2] vs abc2[0..2]
    (if (i32.ne (call $bytesEqualRange
                  (struct.get $go.string 0 (local.get $abc)) (i64.const 0)
                  (struct.get $go.string 0 (local.get $abc2)) (i64.const 0)
                  (i64.const 2)) (i32.const 1)) (then (unreachable)))
    ;; unequal: abc vs abd at full length
    (if (i32.ne (call $bytesEqualRange
                  (struct.get $go.string 0 (local.get $abc)) (i64.const 0)
                  (struct.get $go.string 0 (local.get $abd)) (i64.const 0)
                  (i64.const 3)) (i32.const 0)) (then (unreachable)))
    ;; zero-length: returns 1 even with nulls
    (if (i32.ne (call $bytesEqualRange
                  (ref.null $go.bytes) (i64.const 0)
                  (ref.null $go.bytes) (i64.const 0)
                  (i64.const 0)) (i32.const 1)) (then (unreachable)))

    ;; ----- bytesClone -----
    ;; clone abc[0..3] → equal to abc's backing
    (if (i32.ne (call $bytesEqualRange
                  (call $bytesClone (struct.get $go.string 0 (local.get $abc))
                        (i64.const 0) (i64.const 3))
                  (i64.const 0)
                  (struct.get $go.string 0 (local.get $abc)) (i64.const 0)
                  (i64.const 3)) (i32.const 1)) (then (unreachable)))
    ;; clone of zero length → length 0
    (if (i32.ne (i32.const 0) (array.len (call $bytesClone (ref.null $go.bytes)
                                            (i64.const 0) (i64.const 0)))) (then (unreachable)))

    ;; ----- stringHash -----
    ;; null/empty pass seed through unchanged
    (if (i64.ne (call $stringHash (ref.null $go.string) (i64.const 42)) (i64.const 42)) (then (unreachable)))
    (if (i64.ne (call $stringHash (local.get $empty)     (i64.const 42)) (i64.const 42)) (then (unreachable)))
    ;; equal strings hash equal (regardless of backing identity)
    (if (i64.ne (call $stringHash (local.get $abc)  (i64.const 7))
                (call $stringHash (local.get $abc2) (i64.const 7))) (then (unreachable)))
    ;; "abc" with seed 0: x = (((0 ^ 'a') * mul ^ 'b') * mul ^ 'c') * mul
    ;; precomputed in i64 with mul = 0x9E3779B97F4A7C15
    (local.set $hashed (call $stringHash (local.get $abc) (i64.const 0)))
    (if (i64.eqz (local.get $hashed)) (then (unreachable))) ;; non-zero sanity

    ;; ----- stringConcat2 -----
    ;; "ab" + "" -> "ab"
    (local.set $cat (call $stringConcat2 (local.get $ab) (local.get $empty)))
    (if (i32.ne (call $stringEqual (local.get $cat) (local.get $ab)) (i32.const 1)) (then (unreachable)))
    ;; "" + "abc" -> "abc"
    (local.set $cat (call $stringConcat2 (local.get $empty) (local.get $abc)))
    (if (i32.ne (call $stringEqual (local.get $cat) (local.get $abc)) (i32.const 1)) (then (unreachable)))
    ;; null + null -> ""
    (local.set $cat (call $stringConcat2 (ref.null $go.string) (ref.null $go.string)))
    (if (i64.ne (struct.get $go.string 2 (local.get $cat)) (i64.const 0)) (then (unreachable)))
    ;; "ab" + "c" -> "abc"
    (local.set $cat (call $stringConcat2 (local.get $ab) (call $mkString
                          (call $makeBytes3 (i32.const 99) (i32.const 0) (i32.const 0))
                          (i64.const 0) (i64.const 1))))
    (if (i32.ne (call $stringEqual (local.get $cat) (local.get $abc)) (i32.const 1)) (then (unreachable)))
    ;; "abc" + "de" -> "abcde": build "abcde" via makeBytes5 to compare
    (local.set $cat (call $stringConcat2 (local.get $abc)
                          (call $mkString
                            (call $makeBytes3 (i32.const 100) (i32.const 101) (i32.const 0))
                            (i64.const 0) (i64.const 2))))
    (if (i32.ne (call $stringEqual (local.get $cat)
                  (call $mkString
                    (call $makeBytes5 (i32.const 97) (i32.const 98) (i32.const 99) (i32.const 100) (i32.const 101))
                    (i64.const 0) (i64.const 5))) (i32.const 1)) (then (unreachable)))

    ;; ----- WriteLinearMemory + ReadLinearMemory + ResetLinearMemory round-trip -----
    ;; write "abc" (3 bytes) into linear memory, read 3 bytes back,
    ;; compare against the source. Then write "de" (2 bytes), confirm
    ;; the bump pointer advanced (next write returns a higher offset).
    ;; Then Reset to the start and confirm the next write reuses the
    ;; original offset.
    (block $linmem_done
      ;; off1 = WriteLinearMemory(0, abc.backing)  -> some offset (typically 0 on first call)
      ;; read1 = ReadLinearMemory(0, off1, 3)
      ;; assert bytesEqualRange(abc.backing, 0, read1, 0, 3) == 1
      (local.set $off1 (call $WriteLinearMemory (i32.const 0)
                              (struct.get $go.string 0 (local.get $abc))))
      (local.set $read (call $ReadLinearMemory (i32.const 0) (local.get $off1) (i32.const 3)))
      (if (i32.ne (call $bytesEqualRange
                    (struct.get $go.string 0 (local.get $abc)) (i64.const 0)
                    (local.get $read) (i64.const 0)
                    (i64.const 3)) (i32.const 1)) (then (unreachable)))

      ;; write "de"; off2 must be > off1 (bump advanced)
      (local.set $de (call $makeBytes3 (i32.const 100) (i32.const 101) (i32.const 0)))
      ;; truncate to 2 by cloning the first 2 bytes
      (local.set $de (call $bytesClone (local.get $de) (i64.const 0) (i64.const 2)))
      (local.set $off2 (call $WriteLinearMemory (i32.const 0) (local.get $de)))
      (if (i32.le_u (local.get $off2) (local.get $off1)) (then (unreachable)))
      ;; read "de" back, compare
      (local.set $read (call $ReadLinearMemory (i32.const 0) (local.get $off2) (i32.const 2)))
      (if (i32.ne (call $bytesEqualRange
                    (local.get $de) (i64.const 0)
                    (local.get $read) (i64.const 0)
                    (i64.const 2)) (i32.const 1)) (then (unreachable)))

      ;; Reset to off1 frees both writes; next write should land at off1.
      (call $ResetLinearMemory (i32.const 0) (local.get $off1))
      (local.set $off3 (call $WriteLinearMemory (i32.const 0)
                              (struct.get $go.string 0 (local.get $abc))))
      (if (i32.ne (local.get $off3) (local.get $off1)) (then (unreachable)))

      ;; ReadLinearMemory(_, _, 0) returns a fresh empty array
      (if (i32.ne (i32.const 0) (array.len
                    (call $ReadLinearMemory (i32.const 0) (i32.const 0) (i32.const 0))))
        (then (unreachable)))

      ;; WriteLinearMemory of null returns the current bump offset (no allocation)
      (if (i32.ne (call $WriteLinearMemory (i32.const 0) (ref.null $go.bytes))
                  (i32.add (local.get $off3) (i32.const 3)))
        (then (unreachable))))
  )
)
