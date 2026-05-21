;; De-risk for wasm3 ref-element slice interior pointers ([]*T).
;;
;; Validates the mechanism LoadInterior/StoreInterior would emit for a
;; reference element class: the backing is (array (ref null $T)); a store
;; takes an anyref value (the *T per-value local is anyref) and must
;; ref.cast it down to (ref null $T) before array.set; a load array.get's
;; the typed ref and uses it as anyref. The CRITICAL question is whether a
;; NULLABLE ref.cast on a null value succeeds (a nil *T must NOT trap).
;;
;; storeThrough/loadThrough are deliberately GENERIC over anyref — exactly
;; how a callee that received the value as anyref would behave.

(module
  (type $T (struct (field $v (mut i64))))
  (type $arr (array (mut (ref null $T))))

  ;; storeThrough(a, i, val): a[i] = (ref null $T)val
  (func $storeThrough (param $a (ref $arr)) (param $i i32) (param $val anyref)
    local.get $a
    local.get $i
    local.get $val
    ref.cast (ref null $T)   ;; nullable downcast — must accept null
    array.set $arr)

  ;; loadThrough(a, i) -> anyref
  (func $loadThrough (param $a (ref $arr)) (param $i i32) (result anyref)
    local.get $a
    local.get $i
    array.get $arr)

  (func (export "_start")
    (local $a (ref $arr))
    (local $obj (ref $T))
    ;; a := new [2]*T (elements default to null)
    ref.null $T
    i32.const 2
    array.new $arr
    local.set $a

    ;; obj := &T{v:42}
    i64.const 42
    struct.new $T
    local.set $obj

    ;; a[0] = obj  (non-nil store through anyref)
    local.get $a
    i32.const 0
    local.get $obj
    call $storeThrough

    ;; a[1] = nil  (NIL store through anyref — the trap risk)
    local.get $a
    i32.const 1
    ref.null any
    call $storeThrough

    ;; check a[0] is non-null and a[0].v == 42
    local.get $a
    i32.const 0
    call $loadThrough
    ref.cast (ref $T)
    struct.get $T $v
    i64.const 42
    i64.ne
    if
      unreachable
    end

    ;; check a[1] is null
    local.get $a
    i32.const 1
    call $loadThrough
    ref.is_null
    i32.eqz
    if
      unreachable
    end)
)
