;; Fat-pointer-with-class de-risk prototype for wasm3.
;;
;; Validates that the design proposed for *T on wasm3 — a polymorphic
;; pointer type with a class field carrying get/set funcrefs — works
;; in V8 with WasmGC subtyping. The leaf type T = i32 here; the same
;; pattern extends to every other scalar leaf.
;;
;; Hierarchy:
;;   $go.ptr.i32                       — polymorphic pointer to an i32 cell
;;     $go.ptr.array.i32   (subtype)   — pointer to an array element
;;     $go.ptr.struct.i32  (subtype)   — pointer to a struct field
;;
;; The class is carried as field $class on the base pointer type. All
;; subtypes inherit that field at offset 0, so the dispatcher can read
;; `ptr.class.get` / `ptr.class.set` without a downcast. The downcast
;; happens inside each variant's get/set impl, which knows its own
;; concrete subtype.
;;
;; All type declarations live in one `rec` group because of the cycle:
;;   $go.ptr.i32        references  $go.ref.i32          (class field type)
;;   $go.ref.i32        references  $get-i32-fn / $set-i32-fn (funcref types)
;;   $get-i32-fn        references  $go.ptr.i32          (param type)
;;
;; Three end-to-end checks:
;;   test-array          → 30   (build array, &arr[2], deref polymorphically)
;;   test-struct         → 99   (build struct, &s.f0, deref polymorphically)
;;   test-chan-transit   → 123  (store fat ptr through an anyref local,
;;                              cast back, deref — proves no static-type
;;                              info needed at the deref site, so the
;;                              ptr survives chan transit / function-
;;                              boundary travel)

(module
  (rec
    ;; Base polymorphic pointer to an i32 cell. The $class field is at
    ;; offset 0 in every subtype (WasmGC subtype layout rule), so the
    ;; dispatcher reads it without a cast.
    (type $go.ptr.i32 (sub (struct
      (field $class (ref $go.ref.i32)))))

    ;; Class type — the "vtable" for *i32. One instance per variant
    ;; (singleton globals below). Carries the get/set funcrefs.
    (type $go.ref.i32 (sub (struct
      (field $get (ref $get-i32-fn))
      (field $set (ref $set-i32-fn)))))

    ;; Funcref signatures for get/set. Both take the polymorphic ptr
    ;; (subtype refs upcast freely into the param type) and use ref.cast
    ;; internally to recover the concrete variant.
    (type $get-i32-fn (func (param (ref $go.ptr.i32)) (result i64)))
    (type $set-i32-fn (func (param (ref $go.ptr.i32)) (param i64)))

    ;; Array-element variant: backing array + element index.
    (type $go.ptr.array.i32 (sub $go.ptr.i32 (struct
      (field $class (ref $go.ref.i32))
      (field $base (ref $array.i32))
      (field $offset i32))))

    ;; Struct-field variant: just the struct ref. The class encodes
    ;; *which* field — in a real implementation we'd generate one
    ;; class per (struct, field) pair; here field 0 of $struct.S is
    ;; hard-coded into the class's get/set impls.
    (type $go.ptr.struct.i32 (sub $go.ptr.i32 (struct
      (field $class (ref $go.ref.i32))
      (field $base (ref $struct.S)))))

    ;; Backing types. $array.i32 is a mutable i32 array; $struct.S has
    ;; one mutable i32 field.
    (type $array.i32 (array (mut i32)))
    (type $struct.S (struct (field $f0 (mut i32))))
  )

  ;; --- Variant: array-element ---

  (func $get-array-i32 (type $get-i32-fn) (param $p (ref $go.ptr.i32)) (result i64)
    (local $pa (ref $go.ptr.array.i32))
    (local.set $pa (ref.cast (ref $go.ptr.array.i32) (local.get $p)))
    (i64.extend_i32_u
      (array.get $array.i32
        (struct.get $go.ptr.array.i32 $base (local.get $pa))
        (struct.get $go.ptr.array.i32 $offset (local.get $pa)))))

  (func $set-array-i32 (type $set-i32-fn) (param $p (ref $go.ptr.i32)) (param $v i64)
    (local $pa (ref $go.ptr.array.i32))
    (local.set $pa (ref.cast (ref $go.ptr.array.i32) (local.get $p)))
    (array.set $array.i32
      (struct.get $go.ptr.array.i32 $base (local.get $pa))
      (struct.get $go.ptr.array.i32 $offset (local.get $pa))
      (i32.wrap_i64 (local.get $v))))

  ;; --- Variant: struct-field (field 0 of $struct.S) ---

  (func $get-struct-i32-f0 (type $get-i32-fn) (param $p (ref $go.ptr.i32)) (result i64)
    (local $ps (ref $go.ptr.struct.i32))
    (local.set $ps (ref.cast (ref $go.ptr.struct.i32) (local.get $p)))
    (i64.extend_i32_u
      (struct.get $struct.S $f0
        (struct.get $go.ptr.struct.i32 $base (local.get $ps)))))

  (func $set-struct-i32-f0 (type $set-i32-fn) (param $p (ref $go.ptr.i32)) (param $v i64)
    (local $ps (ref $go.ptr.struct.i32))
    (local.set $ps (ref.cast (ref $go.ptr.struct.i32) (local.get $p)))
    (struct.set $struct.S $f0
      (struct.get $go.ptr.struct.i32 $base (local.get $ps))
      (i32.wrap_i64 (local.get $v))))

  ;; Declare funcrefs taken via ref.func (per spec, escaping funcrefs
  ;; must be pre-declared).
  (elem declare func
    $get-array-i32 $set-array-i32
    $get-struct-i32-f0 $set-struct-i32-f0)

  ;; Class instances — singletons.
  (global $class.array.i32 (ref $go.ref.i32)
    (struct.new $go.ref.i32
      (ref.func $get-array-i32)
      (ref.func $set-array-i32)))

  (global $class.struct.i32.f0 (ref $go.ref.i32)
    (struct.new $go.ref.i32
      (ref.func $get-struct-i32-f0)
      (ref.func $set-struct-i32-f0)))

  ;; --- Polymorphic deref / store. THIS is what PtrLoad / PtrStore in
  ;; the compiler lower to. No knowledge of the concrete variant. ---

  (func $deref-i32 (param $p (ref $go.ptr.i32)) (result i64)
    ;; ptr.class.get(ptr)
    (call_ref $get-i32-fn
      (local.get $p)
      (struct.get $go.ref.i32 $get
        (struct.get $go.ptr.i32 $class (local.get $p)))))

  (func $store-i32 (param $p (ref $go.ptr.i32)) (param $v i64)
    ;; ptr.class.set(ptr, v)
    (call_ref $set-i32-fn
      (local.get $p)
      (local.get $v)
      (struct.get $go.ref.i32 $set
        (struct.get $go.ptr.i32 $class (local.get $p)))))

  ;; --- Tests ---

  ;; test-array: build arr=[10,20,30,40,50], make &arr[2], polymorphic
  ;; deref. Expect 30.
  (func $test-array (export "test-array") (result i64)
    (local $arr (ref $array.i32))
    (local $ptr (ref $go.ptr.array.i32))
    (local.set $arr (array.new $array.i32 (i32.const 0) (i32.const 5)))
    (array.set $array.i32 (local.get $arr) (i32.const 0) (i32.const 10))
    (array.set $array.i32 (local.get $arr) (i32.const 1) (i32.const 20))
    (array.set $array.i32 (local.get $arr) (i32.const 2) (i32.const 30))
    (array.set $array.i32 (local.get $arr) (i32.const 3) (i32.const 40))
    (array.set $array.i32 (local.get $arr) (i32.const 4) (i32.const 50))
    (local.set $ptr (struct.new $go.ptr.array.i32
      (global.get $class.array.i32)
      (local.get $arr)
      (i32.const 2)))
    (call $deref-i32 (local.get $ptr)))

  ;; test-struct: build s={99}, make &s.f0, polymorphic deref. Expect 99.
  (func $test-struct (export "test-struct") (result i64)
    (local $s (ref $struct.S))
    (local $ptr (ref $go.ptr.struct.i32))
    (local.set $s (struct.new $struct.S (i32.const 99)))
    (local.set $ptr (struct.new $go.ptr.struct.i32
      (global.get $class.struct.i32.f0)
      (local.get $s)))
    (call $deref-i32 (local.get $ptr)))

  ;; test-chan-transit: simulate the chan-transit case where the fat
  ;; pointer's concrete subtype is unknown at the deref site. Store
  ;; the ptr in an `anyref` local (the chan-slot type), read it back,
  ;; cast to base $go.ptr.i32, polymorphic deref. The class dispatch
  ;; recovers the right impl without any compile-time knowledge of
  ;; which variant it is.
  ;;
  ;; Also exercises a STORE through the polymorphic ptr, then re-reads
  ;; via the variant-typed ref to confirm the write reached the
  ;; backing.
  (func $test-chan-transit (export "test-chan-transit") (result i64)
    (local $arr (ref $array.i32))
    (local $arr-ptr (ref $go.ptr.array.i32))
    (local $opaque anyref)
    (local $recovered (ref $go.ptr.i32))
    ;; arr = [0,0,0]; &arr[1]
    (local.set $arr (array.new $array.i32 (i32.const 0) (i32.const 3)))
    (local.set $arr-ptr (struct.new $go.ptr.array.i32
      (global.get $class.array.i32)
      (local.get $arr)
      (i32.const 1)))
    ;; "Send through chan" — store as anyref.
    (local.set $opaque (local.get $arr-ptr))
    ;; "Receive from chan" — cast back to $go.ptr.i32 (the static type
    ;; the receiver knows; the concrete variant is hidden).
    (local.set $recovered (ref.cast (ref $go.ptr.i32) (local.get $opaque)))
    ;; *ptr = 123 via polymorphic store
    (call $store-i32 (local.get $recovered) (i64.const 123))
    ;; Verify by reading arr[1] directly (proves the polymorphic store
    ;; reached the backing array).
    (i64.extend_i32_u
      (array.get $array.i32 (local.get $arr) (i32.const 1))))
)
