;; De-risk of the $go.ptr.<class> accessor-pair fat-pointer design:
;; a fat pointer carries {base, offset, get-funcref, set-funcref}; a
;; GENERIC callee dereferences it via call_ref WITHOUT knowing the
;; container's static struct type (the get/set funcs, specialized at the
;; creation site, hold that knowledge). This is the boundary-crossing
;; case that plain (container, offset) fat pointers can't handle.
(module
  (rec (type $go.object (sub (struct))))
  (rec (type $S (sub $go.object (struct (field (mut i64))))))
  (rec (type $getter (func (param anyref i32) (result i64))))
  (rec (type $setter (func (param anyref i32 i64))))
  (rec (type $go.ptr.i64 (sub $go.object (struct
    (field anyref)             ;; 0: base (container)
    (field i32)                ;; 1: offset / field index
    (field (ref $getter))      ;; 2: get
    (field (ref $setter))))))  ;; 3: set

  (import "wasi_snapshot_preview1" "proc_exit" (func $proc_exit (param i32)))
  (memory (export "memory") 1)
  ;; Declare the accessors ref-able (ref.func requires this).
  (elem declare func $get_S $set_S)

  ;; Per-container accessors (specialized; statically know $S).
  (func $get_S (param $base anyref) (param $off i32) (result i64)
    (struct.get $S 0 (ref.cast (ref $S) (local.get $base))))
  (func $set_S (param $base anyref) (param $off i32) (param $v i64)
    (struct.set $S 0 (ref.cast (ref $S) (local.get $base)) (local.get $v)))

  ;; GENERIC callees: do NOT mention $S. They only know $go.ptr.i64.
  (func $storeThrough (param $p (ref $go.ptr.i64)) (param $v i64)
    (call_ref $setter
      (struct.get $go.ptr.i64 0 (local.get $p))
      (struct.get $go.ptr.i64 1 (local.get $p))
      (local.get $v)
      (struct.get $go.ptr.i64 3 (local.get $p))))
  (func $loadThrough (param $p (ref $go.ptr.i64)) (result i64)
    (call_ref $getter
      (struct.get $go.ptr.i64 0 (local.get $p))
      (struct.get $go.ptr.i64 1 (local.get $p))
      (struct.get $go.ptr.i64 2 (local.get $p))))

  (func (export "_start")
    (local $s (ref $S))
    (local $p (ref $go.ptr.i64))
    (local.set $s (struct.new $S (i64.const 7)))
    ;; &s.value as a fat pointer (created where $S is known)
    (local.set $p (struct.new $go.ptr.i64
      (local.get $s) (i32.const 0) (ref.func $get_S) (ref.func $set_S)))
    ;; generic store/load through the boundary
    (call $storeThrough (local.get $p) (i64.const 42))
    ;; exit = (loadThrough(p)!=42) + (s.value!=42)*10  -> 0 on success
    (call $proc_exit
      (i32.add
        (i32.ne (i32.wrap_i64 (call $loadThrough (local.get $p))) (i32.const 42))
        (i32.mul (i32.ne (i32.wrap_i64 (struct.get $S 0 (local.get $s))) (i32.const 42)) (i32.const 10)))))
)
