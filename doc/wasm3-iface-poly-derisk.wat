;; De-risk of the wasm3 polymorphic interface design.
;;
;; What we need to prove before writing compiler code:
;;
;;   (1) A polymorphic base type ($go.iface) + per-interface
;;       subtype ($go.iface.<I>) that adds a typed vtable field.
;;       Caller-side dispatch knows I but not T, sees the iface
;;       value as (ref $go.iface.<I>) via wasmgc subtyping.
;;
;;   (2) Per-(I,T) shared vtab singleton — built once at module
;;       init as a module-level global, referenced by every iface
;;       instance with the same (I,T) pair. Holds the per-T method
;;       funcrefs at fixed-offset fields the I-level layout
;;       defines.
;;
;;   (3) Method dispatch: a function that takes (ref $go.iface.<I>)
;;       and calls method m_0. Reads vtab via struct.get, extracts
;;       funcref via struct.get at the method's fixed offset, calls
;;       via call_ref with iface.data as receiver.
;;
;;   (4) Type assertion: ref.eq on type-descriptor refs. iface.vtab
;;       carries a type-descriptor anyref at a fixed offset; the
;;       call site compares against the target type's descriptor
;;       global. nil-iface check: ref.is_null on the iface itself.
;;
;;   (5) Empty interface ($go.eface): same shape as $go.iface (no
;;       vtab field; just type + data). The existing prelude
;;       $go.iface already has this layout — we re-use it for the
;;       empty interface and add subtypes for non-empty ones.
;;
;;   (6) Multiple Ts implementing the same I: two singletons
;;       $go.vtab.<I,T1> + $go.vtab.<I,T2> share the same vtable
;;       type definition; iface instances built with each vtab
;;       dispatch to the correct method body.
;;
;; If wasm-tools validate accepts this AND Node executes the
;; polymorphic-dispatch + type-assert + nil-check cases, the
;; interface migration's wasmgc shape is sound and the compiler
;; work can proceed without rework.

(module
  (import "wasi_snapshot_preview1" "proc_exit" (func $proc_exit (param i32)))
  (memory (export "memory") 1)

  ;; --- Per-type concrete struct types ---------------------------
  ;; Two Go types T1 and T2, each with one int32 field as state.

  (rec
    (type $T1 (sub (struct (field (mut i32))))))
  (rec
    (type $T2 (sub (struct (field (mut i32))))))

  ;; --- Interface I and its vtable -----------------------------
  ;; Interface I has one method:
  ;;   M(self) → i32
  ;;
  ;; The funcref type for any M implementation: takes the receiver
  ;; as anyref (the iface-data slot is anyref because the caller
  ;; doesn't know T) and returns i32. Each per-T implementation
  ;; downcasts the anyref receiver to its own concrete type at
  ;; entry.

  (rec
    (type $M_funcref (func (param anyref) (result i32))))

  ;; The vtable for I: a struct containing the type descriptor
  ;; reference (for type assertions) and the method funcref(s).
  ;; Subtype of an empty struct $go.object to allow ref.eq across
  ;; vtab instances if needed.

  (rec
    (type $go.object (sub (struct))))
  (rec
    (type $go.vtab.I (sub $go.object (struct
      (field (ref null $go.object))  ;; 0: type descriptor (eqref-compatible for ref.eq)
      (field (ref $M_funcref))       ;; 1: M's funcref
    ))))

  ;; The iface base type ($go.iface) and the per-interface
  ;; subtype ($go.iface.I). Subtype extends base with the typed
  ;; vtab field; the base layout (data anyref) is kept first so
  ;; common-path data access works across all interfaces.

  (rec
    (type $go.iface (sub $go.object (struct
      (field anyref)           ;; 0: data
    ))))
  (rec
    (type $go.iface.I (sub $go.iface (struct
      (field anyref)                 ;; 0: data (inherited)
      (field (ref $go.vtab.I))       ;; 1: vtab
    ))))

  ;; --- Per-T method bodies + type descriptors -----------------
  ;; Each T's M takes the iface-data anyref, ref.casts to T's
  ;; concrete struct, reads the field, returns it.

  (elem declare func $T1.M $T2.M)
  (func $T1.M (param $self anyref) (result i32)
    (struct.get $T1 0 (ref.cast (ref $T1) (local.get $self))))
  (func $T2.M (param $self anyref) (result i32)
    (i32.add
      (struct.get $T2 0 (ref.cast (ref $T2) (local.get $self)))
      (i32.const 1000)))

  ;; Type-descriptor globals. Each Go type lowers to a unique
  ;; struct.new instance ($go.object subtype); ref.eq across two
  ;; iface values' vtab[0] is the wasmgc analog of the standard
  ;; runtime's *_type pointer comparison.

  (global $type.T1 (ref $go.object) (struct.new_default $go.object))
  (global $type.T2 (ref $go.object) (struct.new_default $go.object))

  ;; Per-(I,T) vtab singletons, built at module init. Each holds
  ;; the type-descriptor ref + the per-T method funcrefs.

  (global $vtab.I.T1 (ref $go.vtab.I)
    (struct.new $go.vtab.I
      (global.get $type.T1)
      (ref.func $T1.M)))
  (global $vtab.I.T2 (ref $go.vtab.I)
    (struct.new $go.vtab.I
      (global.get $type.T2)
      (ref.func $T2.M)))

  ;; --- Polymorphic dispatcher ----------------------------------
  ;; callI_M takes (ref $go.iface.I) and dispatches M.
  ;; Knows about I (sees vtab + data offsets), does NOT know T.
  ;; This is the wasm analog of `iface.M()` lowering at the call
  ;; site.

  (func $callI_M (param $iface (ref $go.iface.I)) (result i32)
    (call_ref $M_funcref
      ;; receiver: iface.data
      (struct.get $go.iface.I 0 (local.get $iface))
      ;; funcref: iface.vtab.M
      (struct.get $go.vtab.I 1
        (struct.get $go.iface.I 1 (local.get $iface)))))

  ;; --- Type assertion ------------------------------------------
  ;; assertI_T1 checks if the iface's underlying type is T1 by
  ;; comparing vtab.type against $type.T1. Returns 1 if match,
  ;; 0 otherwise.

  (func $assertI_T1 (param $iface (ref $go.iface.I)) (result i32)
    (ref.eq
      (struct.get $go.vtab.I 0
        (struct.get $go.iface.I 1 (local.get $iface)))
      (global.get $type.T1)))

  ;; --- Iface construction helpers ------------------------------
  ;; Build iface from a T value + its singleton vtab.

  (func $makeI_T1 (param $t (ref $T1)) (result (ref $go.iface.I))
    (struct.new $go.iface.I
      (local.get $t)
      (global.get $vtab.I.T1)))
  (func $makeI_T2 (param $t (ref $T2)) (result (ref $go.iface.I))
    (struct.new $go.iface.I
      (local.get $t)
      (global.get $vtab.I.T2)))

  ;; --- Driver --------------------------------------------------
  ;; Builds T1{42}, T2{7}, dispatches M on each, asserts type,
  ;; aggregates into one exit code so the host can verify.
  ;;
  ;; Expected:
  ;;   t1.M()             == 42
  ;;   t2.M()             == 1007   (7 + 1000)
  ;;   assertI_T1(t1)     == 1
  ;;   assertI_T1(t2)     == 0
  ;;   exit code = 42 + 1007*1 - 7*0  + 1*1 + 0*0 = 1050 ish; we
  ;;   encode them deterministically:
  ;;     ((dispatchT1 << 16) | (dispatchT2 << 4) | (assertT1<<3) | (assertT2<<2))
  ;;   = (42 << 16) | (1007 << 4) | (1 << 3) | (0 << 2)
  ;;   = 2752512 | 16112 | 8
  ;;   = 2768632

  (func (export "_start")
    (local $iface1 (ref $go.iface.I))
    (local $iface2 (ref $go.iface.I))
    (local $r i32)

    ;; Build (ref $T1) with value 42.
    (local.set $iface1 (call $makeI_T1
      (struct.new $T1 (i32.const 42))))
    ;; Build (ref $T2) with value 7.
    (local.set $iface2 (call $makeI_T2
      (struct.new $T2 (i32.const 7))))

    ;; Dispatch M on each, type-assert, encode into $r.
    (local.set $r
      (i32.or
        (i32.shl (call $callI_M (local.get $iface1)) (i32.const 16))
        (i32.or
          (i32.shl (call $callI_M (local.get $iface2)) (i32.const 4))
          (i32.or
            (i32.shl (call $assertI_T1 (local.get $iface1)) (i32.const 3))
            (i32.shl (call $assertI_T1 (local.get $iface2)) (i32.const 2))))))

    (call $proc_exit (local.get $r)))
)
