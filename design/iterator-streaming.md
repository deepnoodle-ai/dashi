# Iterator streaming for model calls and turns

**Status:** Implemented, API experimental · **Date:** 2026-09-27 · **Workflow:** Spec, then build

## Context

`llm.Client.Send` reports provisional deltas through a callback and returns one
authoritative result. `Runner.Run` forwards those deltas to a bounded, lossy
observer. A caller cannot range over progress with backpressure or end the work
by ending the range. The research survey ranks an `iter.Seq2` stream from model
calls through the agent as its first reusable API idea.

## Contract

- `llm.Stream(ctx, client, request)` yields provisional deltas and exactly one
  final result on a fully consumed sequence. A final error includes the partial
  result. The request's pinned `Input.Stream` selects the provider transport.
  The sequence is single-use: its first range sends once; constructing it does
  nothing, and a second range returns an error.
- `Runner.Stream(ctx, turn, command, opts...)` yields recorded steps and
  provisional deltas, then one final `RunResult`. Command validation errors
  arrive as iterator errors. It is also single-use. Recorded steps are yielded
  only after acknowledgement; deltas never imply a committed result.
- Stopping either range cancels its derived context. For a turn, the runner
  finishes recording the started model call's result and records cancellation or
  an unknown result before the iterator returns. The caller reloads the turn by
  ID after stopping early; there is no final result to consume after `break`.
- Yield calls are synchronous and backpressured. No goroutine or channel is
  needed to adapt the existing callback clients. `Send` remains the lower-level
  compatibility and collection primitive, and `Run` remains the collected turn
  primitive. The lossy `Observer` remains independent of the iterator.

## Tradeoffs and checks

This keeps existing provider adapters and custom clients compatible. A client
that ignores context may delay return after the consumer stops; clients must
invoke sinks synchronously and stop invoking them before `Send` returns. Tests
must cover laziness, ordered deltas and final result, backpressure, early break
before and after sending, authoritative step order, and cancellation that
leaves a partial model result. Native provider iterators can replace the
callback implementation later without changing the `iter.Seq2` contract.
