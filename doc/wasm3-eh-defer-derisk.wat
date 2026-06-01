;; De-risk of the wasm3 M5 EH plan for defer / panic / recover.
;;
;; What we need to prove before writing compiler code:
;;
;;   (1) try_table with catch_all_ref captures the in-flight
;;       exception on the catch label's stack; we can run "deferred
;;       work" then throw_ref to re-propagate. This is the exact
;;       shape the Phase 1 function epilogue will emit.
;;
;;   (2) A single $go.panic tag carrying one anyref payload is the
;;       right shape for both throw (panic) and throw_ref (rethrow).
;;
;;   (3) "recover" modeled as an inner try_table inside a deferred
;;       function's body — catches the panic, returns normally
;;       instead of rethrowing. Subsequent calling-frame epilogue
;;       sees no in-flight exception and exits normally.
;;
;;   (4) LIFO defer ordering — nested try_tables that each note
;;       their level and throw_ref produce the correct
;;       innermost-first order.
;;
;; Run with: sh wasm3-eh-defer-derisk.run.sh

(module
  ;; Single tag for Go panics. One anyref payload — the boxed
  ;; panic value. throw $go.panic pushes the payload; throw_ref
  ;; preserves whatever the original exception carried.
  (tag $go.panic (param anyref))

  (import "wasi_snapshot_preview1" "proc_exit" (func $proc_exit (param i32)))
  (memory (export "memory") 1)

  ;; Scratch GC struct so we have a non-trivial anyref payload.
  (rec (type $payload (sub (struct (field (mut i32))))))

  ;; Mutable counter that records the order of "deferred work"
  ;; running. BCD-packed: each "defer ran" shifts left 4 bits and
  ;; OR's in the level tag, so 0x321 == defer 3 then 2 then 1.
  (global $order (mut i32) (i32.const 0))
  (func $note_defer (param $tag i32)
    (global.set $order
      (i32.or
        (i32.shl (global.get $order) (i32.const 4))
        (local.get $tag))))

  ;; -------------------------------------------------------------
  ;; Test (1): try_table + catch_all_ref + throw_ref round-trip.
  ;; The catch label must be a block defined OUTSIDE try_table;
  ;; on catch, control transfers to that block's continuation
  ;; (after the block) with the exnref pushed on the stack.
  ;; -------------------------------------------------------------

  (func $inner_throw
    (throw $go.panic (struct.new $payload (i32.const 42))))

  ;; Models a function with a single defer: $note_defer(1) plays
  ;; the role of the deferred function body. On normal exit
  ;; (impossible here since $inner_throw always throws) we also
  ;; need to run the defer; we route that path through the same
  ;; sequence by branching to $run_defer with a null exnref.
  (func $outer_with_defer
    (block $run_defer (result (ref null exn))
      (try_table (catch_all_ref $run_defer)
        (call $inner_throw))
      ;; try_table completed normally — push a null exnref to
      ;; reuse the same $run_defer continuation.
      (ref.null exn))
    ;; Stack: [exnref or null]. Run "deferred work":
    (call $note_defer (i32.const 1))
    ;; If the exnref is null we exit normally; otherwise rethrow.
    (block $no_rethrow (param (ref null exn))
      (br_on_null $no_rethrow)
      (throw_ref)))

  ;; -------------------------------------------------------------
  ;; Test (2): LIFO ordering across nested deferred frames.
  ;; -------------------------------------------------------------

  (func $level3_throws
    (throw $go.panic (struct.new $payload (i32.const 0))))

  (func $level2_with_defer
    (block $rd (result (ref null exn))
      (try_table (catch_all_ref $rd)
        (call $level3_throws))
      (ref.null exn))
    (call $note_defer (i32.const 2))     ;; LIFO middle
    (block $nr (param (ref null exn))
      (br_on_null $nr)
      (throw_ref)))

  (func $level1_with_defer
    (block $rd (result (ref null exn))
      (try_table (catch_all_ref $rd)
        (call $level2_with_defer))
      (ref.null exn))
    (call $note_defer (i32.const 1))     ;; LIFO outer
    (block $nr (param (ref null exn))
      (br_on_null $nr)
      (throw_ref)))

  ;; Top-level catcher so the test (2) throw doesn't escape.
  (func $lifo_run
    (block $caught
      (try_table (catch_all $caught)
        (call $level1_with_defer))))

  ;; -------------------------------------------------------------
  ;; Test (3): "recover" via an inner try_table inside the
  ;; deferred function's body. The inner try_table catches and
  ;; swallows; the outer body returns normally.
  ;; -------------------------------------------------------------

  (func $panicking_inner
    (throw $go.panic (struct.new $payload (i32.const 99))))

  (func $deferred_recover_body
    (block $rec
      (try_table (catch_all $rec)
        (call $panicking_inner))))   ;; normal exit if catch fires

  (func $recover_outer
    (block $rd (result (ref null exn))
      (try_table (catch_all_ref $rd)
        (call $deferred_recover_body))
      (ref.null exn))
    (call $note_defer (i32.const 7))      ;; 7 means "ran recover-defer"
    (block $nr (param (ref null exn))
      (br_on_null $nr)
      (throw_ref)))

  ;; -------------------------------------------------------------
  ;; Top-level driver: runs tests in sequence and exits with
  ;; $order as the status code. Expected value: 0x1217.
  ;;   Test 1: $note_defer(1)  → order = 0x1
  ;;   Test 2: $note_defer(2)  → order = 0x12
  ;;           $note_defer(1)  → order = 0x121
  ;;   Test 3: $note_defer(7)  → order = 0x1217
  ;; -------------------------------------------------------------
  (func (export "_start")
    ;; Test 1: $outer_with_defer rethrows after running defer.
    (block $t1_done
      (try_table (catch_all $t1_done)
        (call $outer_with_defer)))

    ;; Test 2: nested LIFO defers.
    (call $lifo_run)

    ;; Test 3: recover.
    (call $recover_outer)

    ;; Exit with the BCD-packed order as the status code.
    (call $proc_exit (global.get $order)))
)
