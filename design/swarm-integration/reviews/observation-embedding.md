# Observation and embedding: findings and decisions

[Review index](README.md) · [Examples](../examples/README.md)

The reviewer favored existing streaming support and explicit CLI operations.
Progress watching needs a narrower scope; release tooling and voice policy need
separate treatment from the runtime API.

## Findings incorporated

- **R27, lost acceptance:** A transport error is not known rejection. The example
  distinguishes typed rejection from unknown acceptance. Session/request identity
  persists across restart; acceptance follows storage and conflicting reuse fails.
  SubmitNewTurn, Steer and Enqueue are explicit operations, not readiness guesses.
- **R21, gap handling:** Returning on a preview gap abandoned the final reply.
  The example now clears that message's preview and keeps following. A new
  current-API demo joins mid-stream, receives a gap, then receives the saved reply.
  It does not prove overflow handling. Hub previews are process-local; cross-process
  delivery needs a host transport preserving sequence and gap semantics.
- **R20, scope:** WatchViews covers Dashi state plus activity. Host ledger updates,
  worker errors, record retirement and authorization remain outside it. Activity
  registers notifications before reading, refreshes current state, distinguishes
  worker generations and catches up records after stopped activity. Compare the
  full adapter with a smaller Follow/view-accumulator alternative.
- **R19, trustworthy observation:** Use one activity snapshot, preserve unknown
  effects/final stops, and distinguish actual activity from Session.WorkerStatus
  claims. Its Running value means may-still-act, not verified process liveness.
- **R22, lifetime:** Following later records is useful for mirrors. Host ledger
  changes still need host notifications; avoid a permanent watcher per historical bill.
- **R29, unequal comparison:** The before now shows the actual cross-build script,
  not its still-needed verifier. Defer runtime CLI self-build; use release/dev tools
  or authenticated assets. Byte reproducibility needs pinned tooling and proof.
- **R30, existing safeguards:** Session.Send already serializes writes and validates
  tool/response state. Defer the broad controller. The smaller readiness sketch
  leaves Receive ownership, speech/playback correlation, queues and cancellation
  explicit. Canceled Send contexts close today's connection, so tool cancellation
  must not accidentally cancel the session.
- **R31, catalog claim:** Dashi exports DefaultModel, not a complete voice catalog.
  Adopt constants now. New capabilities/default metadata is small; a full catalog
  needs a maintained policy shared by Connect and the UI.
- **R27/R28, alternatives:** Compare the existing planned ACP adapter with a local
  command port. Both should use the same session input operations. Reconnect
  needs a snapshot/cursor; readiness is a hint, admission still checks busy state.

## Decision for every recommendation

| ID | Decision | Application benefit or next proof |
| --- | --- | --- |
| R19 | Keep/refine | Shared presentation mapping using trustworthy activity. |
| R20 | Narrow/compare | Dashi views plus activity; retain host overlays and prove adapter simplification. |
| R21 | Keep existing API | Responsive output with gap/final-reply and reader-lifetime examples. |
| R22 | Keep, low priority | Explicit all-records mode using existing validated reader logic. |
| R27 | Keep/refine | Remove terminal editing/mention escaping through explicit durable operations. |
| R28 | Combine with R27 | Versioned handshake, schema, reconnect and request-correlated events. |
| R29 | Release tooling | Remove build glue; keep artifact verification and installation. |
| R30 | Defer controller | Test a complete event-loop/readiness example before broader abstraction. |
| R31 | Existing defaults; small metadata | Define catalog ownership/support before adding validation. |
| R32 | Keep | Keyless mechanics now; complete database adoption guide remains future work. |

The demos do not prove R20's final-save/worker-exit race, two-reader isolation,
live CLI input or voice playback. Those remain qualification requirements.

**Follow-up review:** verified on 2026-10-11 by the same independent reviewer.
All substantive findings and final example corrections are addressed; no
unresolved substantive issue remains in this batch. This accepts the review
proposal, not the correctness of an unimplemented API.
