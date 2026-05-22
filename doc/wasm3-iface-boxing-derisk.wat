;; De-risk for wasm3 interface boxing with descriptors as opaque WasmGC
;; identity refs (the dominant remaining blocker: IMake/ITab/IData).
;;
;; The interface/panic path needs $go.iface = {itab, data} with both
;; fields as refs, but a type descriptor (itab / *_type) is today a linear
;; i64 address (OpWasm3LoweredAddr) which cannot be boxed into an anyref
;; field. The user directive (constants/descriptors live in WasmGC memory)
;; suggests representing each descriptor as a WasmGC object. This de-risk
;; validates the M2-minimal form: each descriptor is a UNIQUE OPAQUE ref
;; held in a wasm global initialized by a CONSTANT struct.new_default
;; expression (NO _start needed — the global init runs at instantiation).
;;
;; It validates the operations the interface path needs WITHOUT
;; dereferencing the descriptor (method calls / reflect are deferred):
;;   - IMake:  struct.new $go.iface {itab-ref, data-ref}
;;   - ITab/IData: struct.get
;;   - type identity: ref.eq on two itabs (same descriptor global -> equal)
;;   - nil interface: a null $go.iface ref / ref.is_null
;; If this runs exit 0, descriptors-as-opaque-global-refs is a viable M2
;; foundation for interfaces, panic(constant), and error returns.

(module
  (type $go.object (struct))
  ;; $go.iface = (struct (field anyref itab) (field anyref data))
  (type $go.iface (struct
    (field $itab (mut anyref))
    (field $data (mut anyref))))

  ;; A concrete boxed value the interface points at (e.g. *errFoo).
  (type $errFoo (struct (field $x (mut i64))))

  ;; Two type descriptors as opaque identity refs, each a UNIQUE object
  ;; built by a constant init expression at instantiation (no _start).
  (global $desc.errFoo (ref $go.object) (struct.new_default $go.object))
  (global $desc.errBar (ref $go.object) (struct.new_default $go.object))

  ;; IMake(itab, data) -> $go.iface
  (func $imake (param $itab anyref) (param $data anyref) (result (ref $go.iface))
    local.get $itab
    local.get $data
    struct.new $go.iface)

  ;; ITab(iface) -> anyref
  (func $itab (param $i (ref $go.iface)) (result anyref)
    local.get $i
    struct.get $go.iface $itab)

  ;; sameType(a, b): a and b have identical dynamic type (itab identity).
  ;; The itab field is anyref; ref.eq needs eqref, so cast first (a
  ;; descriptor is a $go.object struct, which is eq-comparable).
  (func $sameType (param $a (ref $go.iface)) (param $b (ref $go.iface)) (result i32)
    local.get $a
    call $itab
    ref.cast (ref null eq)
    local.get $b
    call $itab
    ref.cast (ref null eq)
    ref.eq)

  (func (export "_start")
    (local $e1 (ref $go.iface))
    (local $e2 (ref $go.iface))
    (local $nilIface (ref null $go.iface))

    ;; e1 := errFoo-typed interface wrapping &errFoo{7}
    global.get $desc.errFoo
    i64.const 7
    struct.new $errFoo
    call $imake
    local.set $e1

    ;; e2 := another errFoo-typed interface
    global.get $desc.errFoo
    i64.const 9
    struct.new $errFoo
    call $imake
    local.set $e2

    ;; e1 and e2 share the dynamic type errFoo -> sameType == 1
    local.get $e1
    local.get $e2
    call $sameType
    i32.eqz
    if (unreachable) end

    ;; an errBar-typed interface differs from errFoo -> sameType == 0
    local.get $e1
    global.get $desc.errBar
    ref.null none
    call $imake
    call $sameType
    if (unreachable) end

    ;; nil interface: a null $go.iface ref is recognizable as nil
    local.get $nilIface
    ref.is_null
    i32.eqz
    if (unreachable) end))
