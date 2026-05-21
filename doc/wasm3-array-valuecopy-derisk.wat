;; De-risk for wasm3 whole-array VALUE copy (the Move/Zero-of-boxed-composite
;; core). A Go [N]T is a value type: `b := a` / `return a` must COPY, not
;; alias. wasm3 boxes [N]T as (ref (array T)), so the linear Move [32]
;; (per-8-byte memcpy) the SSA emits is wrong; the copy must be
;; array.new_default + array.copy. This validates that array.copy yields an
;; INDEPENDENT array (mutating the copy does not change the source) — the
;; correctness property the bedrock Move{[N]T} fix depends on.
;;
;; Also validates Zero{[N]T} == a fresh array.new_default (all-zero).

(module
  (type $arr (array (mut i64)))

  ;; valueCopy(src) -> a fresh independent copy of all elements
  (func $valueCopy (param $src (ref $arr)) (result (ref $arr))
    (local $dst (ref $arr))
    ;; dst := new [len(src)]i64  (array.new_default zero-fills)
    local.get $src
    array.len
    array.new_default $arr
    local.set $dst
    ;; array.copy $dst $src : dst[0:len] = src[0:len]
    local.get $dst
    i32.const 0
    local.get $src
    i32.const 0
    local.get $src
    array.len
    array.copy $arr $arr
    local.get $dst)

  (func (export "_start")
    (local $a (ref $arr))
    (local $b (ref $arr))
    ;; a := [3]i64 ; a[0]=10 a[1]=20 a[2]=30
    i64.const 0
    i32.const 3
    array.new $arr
    local.set $a
    local.get $a i32.const 0 i64.const 10 array.set $arr
    local.get $a i32.const 1 i64.const 20 array.set $arr
    local.get $a i32.const 2 i64.const 30 array.set $arr

    ;; b := valueCopy(a)
    local.get $a
    call $valueCopy
    local.set $b

    ;; mutate the COPY: b[1] = 999
    local.get $b i32.const 1 i64.const 999 array.set $arr

    ;; assert b[1] == 999 (copy sees the write)
    local.get $b i32.const 1 array.get $arr
    i64.const 999
    i64.ne
    if unreachable end

    ;; assert a[1] == 20 (source UNCHANGED — independent copy, value semantics)
    local.get $a i32.const 1 array.get $arr
    i64.const 20
    i64.ne
    if unreachable end

    ;; assert b[0] == 10 and b[2] == 30 (other elements copied through)
    local.get $b i32.const 0 array.get $arr
    i64.const 10
    i64.ne
    if unreachable end
    local.get $b i32.const 2 array.get $arr
    i64.const 30
    i64.ne
    if unreachable end

    ;; Zero check: a fresh array.new_default is all-zero
    i32.const 4
    array.new_default $arr
    i32.const 3
    array.get $arr
    i64.const 0
    i64.ne
    if unreachable end)
)
