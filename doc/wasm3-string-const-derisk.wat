;; De-risk for wasm3 small string constants built in WasmGC memory
;; (commit a5e6cf877a). Mirrors EXACTLY what the wasm3 backend emits for a
;; string literal in OpWasm3StructNew: build the (array $go.bytes) inline
;; from per-byte i32.const values via array.new_fixed, then struct.new
;; $go.string {backing, off=0, len}. Validates that the constructed string
;; reads back correctly (length + individual bytes) on a real engine —
;; i.e. that a string constant lives in WasmGC memory as a real (array i8),
;; not as a linear-memory address ref.cast (which is invalid).

(module
  (type $go.bytes (array (mut i8)))
  ;; $go.string = (struct (field (ref $go.bytes)) (field i64 off) (field i64 len))
  (type $go.string (struct
    (field $backing (mut (ref $go.bytes)))
    (field $off (mut i64))
    (field $len (mut i64))))

  ;; mkHi() builds the constant "hi" exactly as the compiler codegen does:
  ;;   i32.const 'h'; i32.const 'i'; array.new_fixed $go.bytes 2
  ;;   i64.const 0 (off); i64.const 2 (len); struct.new $go.string
  (func $mkHi (result (ref $go.string))
    i32.const 104   ;; 'h'
    i32.const 105   ;; 'i'
    array.new_fixed $go.bytes 2
    i64.const 0
    i64.const 2
    struct.new $go.string)

  ;; len(s)
  (func $strlen (param $s (ref $go.string)) (result i64)
    local.get $s
    struct.get $go.string $len)

  ;; s[i] -> byte (unsigned)
  (func $strbyte (param $s (ref $go.string)) (param $i i32) (result i32)
    local.get $s
    struct.get $go.string $backing
    local.get $i
    array.get_u $go.bytes)

  (func (export "_start")
    (local $s (ref $go.string))
    call $mkHi
    local.set $s

    ;; len must be 2
    local.get $s
    call $strlen
    i64.const 2
    i64.ne
    if (unreachable) end

    ;; s[0] must be 'h' (104)
    local.get $s
    i32.const 0
    call $strbyte
    i32.const 104
    i32.ne
    if (unreachable) end

    ;; s[1] must be 'i' (105)
    local.get $s
    i32.const 1
    call $strbyte
    i32.const 105
    i32.ne
    if (unreachable) end))
