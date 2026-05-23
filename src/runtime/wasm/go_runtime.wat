;; go_runtime — the dynamically-linked wasm3 runtime primitive module.
;;
;; This is the hand-written WasmGC "assembly" layer for GOARCH=wasm3's
;; boxed object model. On wasm3, Go's per-arch primitive operations that
;; historically used hand-written assembly + linear-memory pointers
;; (string/slice equality and comparison, hashing, memmove/memclr, …)
;; cannot use a linear data pointer: a string is a (ref $go.string) whose
;; bytes live in a (ref $go.bytes) array. So those primitives live here,
;; hand-written over the boxed types, and the compiler emits calls to
;; them via //go:wasmimport go_runtime <name> (see linearmem_wasm3.go and
;; the per-type .eq/.hash glue the compiler generates in
;; cmd/compile/internal/reflectdata/alg.go, which delegate leaves here).
;;
;; Linking model (decided): this module is a SEPARATE module, dynamically
;; linked — the Go linker emits the main module with go_runtime imports
;; and does NOT merge the two; the wasm host / wasm-tools links and
;; instantiates the pair (e.g. `wasmtime --preload go_runtime=...`). The
;; host statically links it anyway. See doc/wasm3-slice-boxing.md and the
;; project memory on the boxed bulk-ops architecture.
;;
;; Types are declared as singleton rec groups matching the wasm3 prelude
;; (object, bytes, string — see cmd/internal/wasmgc/type.go PreludeTypes)
;; so they match STRUCTURALLY across the module boundary, which is what
;; lets a (ref $go.string) constructed in the main module be passed to an
;; import here. De-risked end to end on wasmtime 44 (gc + function-
;; references): a caller module's (ref $go.string) crosses the import
;; boundary and stringEqual reads its fields correctly.
;;
;; NOTE: $go.string offset/length are i64 (matching the wasm3 i64 Go-int
;; width and the prelude); the backing is a non-null (ref $go.bytes).
(module $go_runtime
  (rec (type $go.object (sub (struct))))
  (rec (type $go.bytes  (sub (array (mut i8)))))
  (rec (type $go.string (sub $go.object (struct
    (field (ref $go.bytes))   ;; 0: backing (non-null)
    (field i64)               ;; 1: offset
    (field i64)))))           ;; 2: length

  ;; strcmp(a, b) -> negative / 0 / positive, lexicographic by unsigned
  ;; byte (Go string comparison / runtime.cmpstring semantics). One
  ;; primitive serves every string comparison: == / != are strcmp==0 /
  ;; !=0, and < <= > >= are strcmp <op> 0. A null ref is the empty
  ;; string (length 0). $sa/$sb are nullable locals so the function
  ;; validates (a non-nullable local would be flagged uninitialized on
  ;; the null-input path); they are only dereferenced inside the compare
  ;; loop, which runs only when both lengths are > 0 (hence non-null).
  (func (export "strcmp")
      (param $a (ref null $go.string)) (param $b (ref null $go.string)) (result i32)
    (local $sa (ref null $go.string)) (local $sb (ref null $go.string))
    (local $la i64) (local $lb i64) (local $n i64) (local $i i64)
    (local $ca i32) (local $cb i32)
    (if (ref.is_null (local.get $a))
      (then (local.set $la (i64.const 0)))
      (else
        (local.set $sa (ref.as_non_null (local.get $a)))
        (local.set $la (struct.get $go.string 2 (local.get $sa)))))
    (if (ref.is_null (local.get $b))
      (then (local.set $lb (i64.const 0)))
      (else
        (local.set $sb (ref.as_non_null (local.get $b)))
        (local.set $lb (struct.get $go.string 2 (local.get $sb)))))
    ;; n = min(la, lb)
    (local.set $n (select (local.get $la) (local.get $lb)
                          (i64.lt_u (local.get $la) (local.get $lb))))
    (local.set $i (i64.const 0))
    (block $done
      (loop $cmp
        (br_if $done (i64.ge_u (local.get $i) (local.get $n)))
        (local.set $ca (array.get_u $go.bytes (struct.get $go.string 0 (local.get $sa))
          (i32.wrap_i64 (i64.add (struct.get $go.string 1 (local.get $sa)) (local.get $i)))))
        (local.set $cb (array.get_u $go.bytes (struct.get $go.string 0 (local.get $sb))
          (i32.wrap_i64 (i64.add (struct.get $go.string 1 (local.get $sb)) (local.get $i)))))
        (if (i32.lt_u (local.get $ca) (local.get $cb)) (then (return (i32.const -1))))
        (if (i32.gt_u (local.get $ca) (local.get $cb)) (then (return (i32.const  1))))
        (local.set $i (i64.add (local.get $i) (i64.const 1)))
        (br $cmp)))
    ;; common prefix equal: shorter string sorts first
    (if (i64.lt_u (local.get $la) (local.get $lb)) (then (return (i32.const -1))))
    (if (i64.gt_u (local.get $la) (local.get $lb)) (then (return (i32.const  1))))
    (i32.const 0))

  ;; stringEqual(a, b) -> 1 if a == b, else 0. Length-first fast path
  ;; lets the common unequal-strings case skip the byte loop. Null refs
  ;; are the empty string (length 0). $sa/$sb are nullable for the same
  ;; validation reason as in strcmp — only dereferenced once both
  ;; lengths are confirmed > 0 (hence non-null). Backing/offset are
  ;; allowed to differ as long as the bytes they project equal.
  (func (export "stringEqual")
      (param $a (ref null $go.string)) (param $b (ref null $go.string)) (result i32)
    (local $sa (ref null $go.string)) (local $sb (ref null $go.string))
    (local $la i64) (local $lb i64) (local $i i64)
    (local $ba (ref null $go.bytes)) (local $bb (ref null $go.bytes))
    (local $oa i64) (local $ob i64)
    (if (ref.is_null (local.get $a))
      (then (local.set $la (i64.const 0)))
      (else
        (local.set $sa (ref.as_non_null (local.get $a)))
        (local.set $la (struct.get $go.string 2 (local.get $sa)))))
    (if (ref.is_null (local.get $b))
      (then (local.set $lb (i64.const 0)))
      (else
        (local.set $sb (ref.as_non_null (local.get $b)))
        (local.set $lb (struct.get $go.string 2 (local.get $sb)))))
    ;; lengths must match
    (if (i64.ne (local.get $la) (local.get $lb)) (then (return (i32.const 0))))
    ;; both empty -> equal
    (if (i64.eqz (local.get $la)) (then (return (i32.const 1))))
    ;; both non-empty here; pre-load backing arrays and offsets once
    (local.set $ba (struct.get $go.string 0 (local.get $sa)))
    (local.set $bb (struct.get $go.string 0 (local.get $sb)))
    (local.set $oa (struct.get $go.string 1 (local.get $sa)))
    (local.set $ob (struct.get $go.string 1 (local.get $sb)))
    (local.set $i (i64.const 0))
    (block $done
      (loop $cmp
        (br_if $done (i64.ge_u (local.get $i) (local.get $la)))
        (if (i32.ne
              (array.get_u $go.bytes (local.get $ba)
                (i32.wrap_i64 (i64.add (local.get $oa) (local.get $i))))
              (array.get_u $go.bytes (local.get $bb)
                (i32.wrap_i64 (i64.add (local.get $ob) (local.get $i)))))
          (then (return (i32.const 0))))
        (local.set $i (i64.add (local.get $i) (i64.const 1)))
        (br $cmp)))
    (i32.const 1))

  ;; stringHash(s, h) -> i64 — mixes s's bytes into the running hash h
  ;; using the same algorithm as runtime.wasm3StringHash in
  ;; alg_hash_wasm3.go (`x = (x ^ byte) * 0x9E3779B97F4A7C15`). The
  ;; two sites MUST stay byte-for-byte equivalent: callers using
  ;; per-type compiler-generated hash glue mix in non-string leaves via
  ;; wasm3StringHash/wasm3Uint64Hash, then this primitive for string
  ;; leaves — divergence breaks map invariants (equal values must hash
  ;; equal). Null and empty strings hash to the seed unchanged, matching
  ;; the Go side's `len(s) == 0` early-out.
  (func (export "stringHash")
      (param $s (ref null $go.string)) (param $h i64) (result i64)
    (local $ns (ref $go.string)) (local $b (ref $go.bytes))
    (local $o i64) (local $n i64) (local $i i64) (local $x i64)
    (if (ref.is_null (local.get $s)) (then (return (local.get $h))))
    (local.set $ns (ref.as_non_null (local.get $s)))
    (local.set $n (struct.get $go.string 2 (local.get $ns)))
    (if (i64.eqz (local.get $n)) (then (return (local.get $h))))
    (local.set $b (struct.get $go.string 0 (local.get $ns)))
    (local.set $o (struct.get $go.string 1 (local.get $ns)))
    (local.set $x (local.get $h))
    (local.set $i (i64.const 0))
    (block $done
      (loop $hash
        (br_if $done (i64.ge_u (local.get $i) (local.get $n)))
        (local.set $x
          (i64.mul
            (i64.xor (local.get $x)
              (i64.extend_i32_u
                (array.get_u $go.bytes (local.get $b)
                  (i32.wrap_i64 (i64.add (local.get $o) (local.get $i))))))
            (i64.const 0x9E3779B97F4A7C15)))
        (local.set $i (i64.add (local.get $i) (i64.const 1)))
        (br $hash)))
    (local.get $x))

  ;; bytesClone(src, off, n) -> (ref $go.bytes) holding a fresh copy of
  ;; src[off:off+n]. The primitive behind `string(b)` and any
  ;; mutability-breaking copy: the compiler hands the source backing,
  ;; offset, and length; this allocates a fresh (mutable) array and
  ;; fills it with array.copy, returning a backing whose mutability is
  ;; harmless because it never escapes back to the caller's []byte. The
  ;; n==0 case returns a fresh zero-length array (no null pun — string
  ;; backings are non-null by type, and `array.new_default $go.bytes 0`
  ;; is valid). No bounds checking — the compiler proves off+n in range.
  (func (export "bytesClone")
      (param $src (ref null $go.bytes)) (param $off i64) (param $n i64)
      (result (ref $go.bytes))
    (local $dst (ref $go.bytes))
    (local.set $dst (array.new_default $go.bytes (i32.wrap_i64 (local.get $n))))
    (if (i64.eqz (local.get $n)) (then (return (local.get $dst))))
    (array.copy $go.bytes $go.bytes
      (local.get $dst) (i32.const 0)
      (ref.as_non_null (local.get $src)) (i32.wrap_i64 (local.get $off))
      (i32.wrap_i64 (local.get $n)))
    (local.get $dst))

  ;; bytesEqualRange(a, ao, b, bo, n) -> 1 if a[ao:ao+n] == b[bo:bo+n],
  ;; else 0. The general byte-equality building block: callers compose
  ;; it for whole-array equality (ao=bo=0, n=array.len), slice equality
  ;; (use the slice header's offset/len), and runtime.memequal's
  ;; sub-range comparisons. Null refs are valid only when n==0
  ;; (zero-length comparison is trivially equal); a non-zero range
  ;; against a null backing is a programmer error and traps via
  ;; ref.as_non_null. No bounds checking — the host caller is the
  ;; compiler, which proves ao+n and bo+n in range.
  (func (export "bytesEqualRange")
      (param $a (ref null $go.bytes)) (param $ao i64)
      (param $b (ref null $go.bytes)) (param $bo i64)
      (param $n i64) (result i32)
    (local $na (ref $go.bytes)) (local $nb (ref $go.bytes))
    (local $i i64)
    (if (i64.eqz (local.get $n)) (then (return (i32.const 1))))
    (local.set $na (ref.as_non_null (local.get $a)))
    (local.set $nb (ref.as_non_null (local.get $b)))
    (local.set $i (i64.const 0))
    (block $done
      (loop $cmp
        (br_if $done (i64.ge_u (local.get $i) (local.get $n)))
        (if (i32.ne
              (array.get_u $go.bytes (local.get $na)
                (i32.wrap_i64 (i64.add (local.get $ao) (local.get $i))))
              (array.get_u $go.bytes (local.get $nb)
                (i32.wrap_i64 (i64.add (local.get $bo) (local.get $i)))))
          (then (return (i32.const 0))))
        (local.set $i (i64.add (local.get $i) (i64.const 1)))
        (br $cmp)))
    (i32.const 1))
)
