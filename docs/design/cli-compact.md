# Session compaction and CLI input recovery

Status: Accepted  Author: Codex  Date: 2026-09-29\
Workflow: spec, orchestrator checkpoint, then build. Named PRD review is ready.

## Context and boundaries

The [approved PRD](../prds/cli-compact.md) adds manual Session compaction and enables it in the CLI. Existing turn records already supply durable replacement history. Reuse them without a record migration or queue store. Preserve automatic skip rules, authority, context identity, tool pairs, and budget enforcement. Wonton remains the mandatory full-screen UI. Plain and print modes share its transcript renderer. No paid probes or mid-turn compaction belong here.

## Shared Session operation

Add these methods in `session/compaction.go`:

```go
Compact(ctx context.Context, budget *dashi.Budget, opts ...dashi.RunOption) (dashi.RunResult, error)
StreamCompact(ctx context.Context, budget *dashi.Budget, opts ...dashi.RunOption) iter.Seq2[dashi.Event, error]
```

Move the no-work sentinel to `session.ErrNothingToCompact`. Make `compaction.ErrNothingToCompact` an alias to the same error value. Existing `errors.Is` checks continue to work without a dependency cycle. Keep the existing error message. Missing Compactor is a configuration error. A missing existing session remains `session.ErrNotFound`.

Separate automatic eligibility from shared plan construction. Automatic `planCompaction` checks completed ordinary-turn usage and `Needed` as today. Manual planning bypasses `Needed`, but refuses every unended last turn with `TurnBusyError`. It never closes ordinary work. Both planners call one builder that clones retained messages, sets coverage/hash, assigns `compact:<turn ID>`, disables tools and steering, and resolves the summary model. A supplied budget overrides Compactor.Start's budget. Nil retains it. Existing RunOption controls and Runner.Observer continue to apply.

Refactor `call.compact` into a shared execution result: final compaction RunResult, warnings, slot-loss state, and Go error. Automatic callers attach warnings and resume their original flow. Manual callers return immediately without `dispatch`. Report close steps before the final RunResult when a failed summary needs closing. Do not emit an earlier apparent success for an empty summary.

| Result | RunResult and error | Built history |
| --- | --- | --- |
| No eligible work | Zero RunResult, ErrNothingToCompact. No record or model call. | Unchanged |
| Preparation failure | Go error, no new record | Unchanged |
| Completed nonempty extracted summary | Recorded RunResult, nil error, no compaction_failed | Replacement |
| Recorded failed or empty summary | Compaction RunResult, nil error, compaction_failed | Unchanged |
| Recorder failure | Existing Go error and any known recorded RunResult | No unacknowledged replacement |
| Cancellation or early iterator exit | Existing context/iterator semantics and known RunResult | Interrupted summary remains resumable |

Both streaming methods remain single-consumption iterators. Breaking a range cancels execution, drains recording internally, and starts no queue turn. Errors before recording yield no recorded-turn RunResult. A canceled summary remains available to Session.Continue. Its history remains the summary's recorded input, never the later replacement.

## Concurrent slots and recovery

Plan under Session.mu and release it before model execution. Maintain running/compacting ownership until finish releases each claim. Same-handle callers see busy while another summary runs. Callers on other handles can reserve the same turn number, but the recorder's stale-turn check and command fencing permit one Start.

A manual loser reloads the chosen slot. A matching completed compaction returns its recorded result. An active matching compaction reports busy. A stopped matching compaction reports its current failure/interruption state. If an ordinary Start took that slot, retain the conflict as an error. Never allocate another manual summary merely to avoid a collision. Automatic planning retains its existing retry rules. Recorder errors after Start cannot mean a fresh summary is safe.

Built history still uses the newest completed nonempty summary. Failed summaries replace nothing. Reopening uses records without a Compactor. Hash mismatch retains its existing warning and replacement behavior. CLI metadata remains title/directory/model/timestamps only.

## CLI input ownership and order

The Engine owns one private pending Start and a FIFO of later submitted lines. The Start gets a command ID and frozen input/agent when submitted. A brand-new prompt that first encounters unreadable history may use the existing Exclusive fresh-Start path. The library generates its identity; the first acknowledgment or CommitError freezes it before retry. This narrow exception preserves damaged-log skipping for new input. It never applies after a try to record the Start. This is memory-only input coordination, not another Session history. Existing Session queued items remain where they are.

While this Start lacks an ordinary StepTurnStarted acknowledgment, later `/queue`, ordinary text, and `!text` enter the Engine FIFO. Acknowledge their queued state immediately. Strip the urgent prefix and explain that it cannot interrupt compaction. Ctrl-C still interrupts. Serialize Start recording and FIFO transfer under an input mutex, independently of screen events.

| Boundary or action | Required action |
| --- | --- |
| Automatic summary starts/completes | Keep the pending Start and later FIFO. Summary acknowledgment does not acknowledge ordinary input. |
| Ordinary StepTurnStarted acknowledges the pending command | Clear the pending Start. Transfer later FIFO items to Session.Enqueue in order before accepting later queue writes. |
| Ctrl-C before ordinary Start | Show original text as pending and retain both input groups. |
| Enter with pending original and interrupted summary | Continue that summary, then retry the original keyed Start once it completes. |
| Continuation stops again | Retain original and later input. Show the next recovery action. |
| New ordinary prompt while original remains pending | Append it after the original. Retry the original using existing close rules. |
| Manual /compact | Keep Session queue and Engine input unchanged. Its completion starts no ordinary work. |

During Enter recovery, consume Session.Stream only through the compaction RunResult, then break the range. Session finishes that turn before yielding its RunResult. Early exit therefore stops `dispatch` without interrupting completed work. Pre-existing Session queue items remain available for the original Start. Sending that Start places those items before its own input. The later FIFO transfers only after ordinary recording acknowledgment, so later lines cannot overtake it. No public queue-suppression option is needed.

Without a pending original, Continue retains normal queue dispatch. After manual compaction, queued lines wait for the next ordinary prompt. Include both Engine and Session counts in unsaved-input notices. `/quit`, Ctrl-D, idle Ctrl-C, `/new`, and `/sessions` warn first and proceed on repetition. State explicitly that memory-only input cannot survive exit. Ordinary recorded turns and compaction can be reopened.

## Recording acknowledgment and duplicate prevention

Only an acknowledged ordinary StepTurnStarted with the pending CommandID releases the prompt. Compaction events, deltas, metadata saves, and StopCompleted do not. Keep the pending key on rejected or unknown Start writes. Ordinary refusals before the Start is recorded remain visible and require resubmission after recovery; /discard does not run a refused prompt.

Before resending after an unknown acknowledgment, load the affected turn from CommitError.Step and compare its initial CommandID/input. Use the existing Session duplicate/reservation checks when retrying the same Start. A matching recorded Start proves it was recorded and consumes its reservations. Missing Start permits retry with the same key. Unreadable or conflicting records block resend and retain input with a warning. Never send a fresh key to bypass an unknown result. No later caller Start may evict the idempotency window before this check.

After confirming an existing Start, clear the pending prompt and enqueue later input. Continue the recorded ordinary turn when needed instead of opening another. The CLI session lock supplies Exclusive ownership, while Session retains its rules for unsafe calls whose result is unknown. The boundary guarantees one ordinary input delivery, not one model call across recoverable interruptions.

## Model policy and Wonton presentation

Engine configures `compaction.New` from the selected client's ModelInfo when opening a session and changing models. Refresh it before new/manual work. Unknown windows use Policy's 128,000 fallback and warn once per selected model per process. A pending Start retains its chosen model through retry. Do not change an already recorded summary's model on Continue.

Renderer detects StartCompaction at StepTurnStarted. Suppress its user/assistant blocks and deltas. Add an origin-based compaction block with a stable turn-derived ID. Wonton replaces that block for running, completed, failed/canceled, and interrupted states. Pending input uses a separate stable block, resolved after acknowledgment. Plain mode prints each state transition. `-p` sends markers/warnings to stderr and ordinary reply only to stdout.

Derive replayed marker state from records, including empty tagged summaries and failed summaries later closed. Move existing summary-text extraction to a small internal helper shared by Session and renderer. This avoids a new public history-building API and divergent tag rules. RenderTurn collapses marker updates to the current recorded state. Reopening never shows summary content or invents a running worker. Keep Session titles derived only from ordinary user input. Existing printable sanitization applies.

Use the existing footer unchanged. #116 exclusively owns `FooterText` and its focused token-presentation test. This track may update other renderer code and new compaction tests, then rebase to combine #116.

## Alternatives, consequences, and verification

A sixth Runner command would confuse turn and session responsibilities. A public queue-suppression option would add a host policy for one CLI recovery boundary. Persisting pending input would require a separate durable-input contract. The private buffer and stream boundary avoid those changes. They add input coordination and require exit warnings because pending input remains volatile.

Keyless core tests cover manual results, model/budget choices, slot races, streaming cancellation, and fresh-handle built history. CLI tests use scripted windows/usage and recorder failures. Pin pre-existing queue → original prompt → later FIFO ordering, repeated interruption, lost acknowledgment, and one original delivery. Cover manual queue retention, automatic failure fallback, Enter recovery, busy/urgent input, title preservation, model changes, and all output modes. Render real Wonton screens at terminal sizes for every marker and pending-input state. Save transcripts and terminal screenshots outside Git. Update Session/compaction/CLI guides and commit a public Example.

Run formatter, vet, tests, build, and race tests in the root and separate CLI modules. No migration or feature flag is required. No design question remains open. The orchestrator checks this spec before publication or code.
