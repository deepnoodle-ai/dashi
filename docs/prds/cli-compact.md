# Compact a CLI conversation without losing its record

Status: Draft\
PRD PR: pending  Implementation PR: pending  Updated: 2026-09-29

## Problem and users

A CLI user needs to continue a long coding conversation before it exceeds the model's context window. [Issue #103](https://github.com/deepnoodle-ai/dashi/issues/103) requests automatic compaction, `/compact`, and transcript markers. The CLI currently configures no `Session.Compactor`. Session also lacks a public operation for manual compaction. Application developers need that operation without rebuilding Session history, recovery, and concurrency rules.

## Context and concepts

Session already runs automatic compaction before a new caller or queue turn. It records a compaction turn, its spend, and its replacement history. Fresh handles honor that history without a Compactor. The CLI uses one renderer for live events and reopened records.

**Compaction** summarizes projected history while preserving the conversation and execution record. A **compaction turn** records the summary request and result. Its **replacement history** contains the summary and retained messages for later model calls. These existing concepts retain their names, including `Outcome` and `Stop`.

[OpenCode](https://opencode.ai/docs/tui/#compact) offers `/compact` for the current session. [Claude Code](https://code.claude.com/docs/en/context-window#when-your-context-fills-up) combines automatic compaction with `/compact` and optional focus instructions. Dashi adopts the plain command and shared execution path. Focus instructions and new policy controls are outside this track.

## Use cases and acceptance

### UC-1: Continue automatically

The user sends another prompt after a completed turn reaches 80% of the context window.

- [ ] The CLI configures Session compaction in interactive, plain, and `-p` modes, including reopened sessions.
- [ ] The check uses the last completed ordinary turn's final reported model-call usage, including cached input and output.
- [ ] The selected model supplies the window and summary model. `/model` updates both for subsequent compaction.
- [ ] An unknown window uses the existing 128,000-token fallback and warns once per selected model during the process.
- [ ] Session compacts before the new caller or queue turn. That turn uses replacement history and retains its input.
- [ ] Existing skip rules remain: no automatic compaction after failure, cancellation, a just-closed turn, or a compaction turn.

### UC-2: Compact deliberately

The user types `/compact` at an idle prompt. A developer uses the configured summary model and default or explicit budget:

```go
out, err := s.Compact(ctx, nil)
out, err = s.Compact(ctx, &dashi.Budget{MaxOutputTokens: 2_000})
for event, err := range s.StreamCompact(ctx, nil) { /* show progress */ }
```

- [ ] Both operations force one compaction attempt without checking the usage threshold. They use the configured `Session.Compactor`.
- [ ] The collected result is the compaction `Outcome`. Streaming uses existing `Event` values and a final `Outcome`.
- [ ] Neither operation sends ordinary user input, consumes queued input, nor starts queue turns afterward.
- [ ] An empty new session or nothing eligible to summarize makes no model call and records no turn. The CLI explains that nothing needs compaction.
- [ ] A missing Compactor returns a clear configuration error. Compactor preparation errors remain available to the caller.
- [ ] Both methods accept a `*Budget`, then existing `RunOption` values for ownership, observation, and cancellation controls. The recorded start preserves the effective budget.
- [ ] A nil budget keeps the Compactor's budget or ordinary defaults. The Compactor selects the summary model, otherwise Session's configured or carried model applies.
- [ ] A supplied budget overrides the Compactor's budget. Missing session IDs retain the existing `ErrSessionNotFound` behavior.
- [ ] `/help` documents `/compact`. Arguments receive a usage notice rather than entering the conversation.

### UC-3: Handle busy, failure, and interruption

The user requests compaction while work remains open, or the summary fails.

- [ ] Manual compaction requires the last turn to have ended. Otherwise it returns `*TurnBusyError` with the turn ID, origin, and next action.
- [ ] Manual compaction never closes an ordinary interrupted, limited, waiting, or uncertain turn. The CLI explains how to continue or discard it first.
- [ ] `/compact` typed during execution receives a wait-and-retry notice. It never becomes a steer or a deferred command.
- [ ] During compaction, steering refuses and `/queue` remains available. Queued items retain their order for the next ordinary turn.
- [ ] Racing handles cannot run two summaries for the same compaction slot. The loser reloads the recorded result or reports busy while it runs.
- [ ] Preparation failure, failed execution, or an empty summary keeps previous projected history. The CLI displays the cause and `compaction_failed` when applicable.
- [ ] Automatic failure still permits the original caller or queue turn. Manual failure starts no ordinary turn and preserves queued items.
- [ ] Ctrl-C interrupts the compaction turn. Enter or reopening can continue it with its recorded input. A later ordinary prompt can close it under existing Session rules.
- [ ] Recorder errors and context cancellation retain their existing error meanings. The CLI never reports successful replacement without a recorded completed summary.

### UC-4: Read and reopen the transcript

The user sees compaction progress, then reopens the session and sends another prompt.

- [ ] Live and reopened transcripts identify compaction by `StartCompaction` origin, with running, completed, interrupted, and failed states.
- [ ] The transcript hides the summary request and summary text. A compaction marker and footer retain the turn number, usage, and cost.
- [ ] `-p` prints only the ordinary answer to stdout. Compaction markers and warnings go to stderr.
- [ ] Reopening preserves ordinary scrollback, session title, queued-input rules, and the existing replacement history without separate summary storage.

## Decisions and scope

- Add Session methods, with a streaming counterpart and explicit nullable budget. Reject a sixth Runner command, caller-authored facts, or a global budget option.
- Compact only ended turns manually. Preserve the user's continuation choice. Reject implicitly canceling an interrupted ordinary turn.
- Leave queued input untouched. `/compact` means summarize now. Reject running pending user work as a side effect.
- Reuse the existing 80% policy and compaction records. Reject a CLI history store, threshold flag, focus prompt, or new record format.
- Show markers instead of summary content. Keep the coding transcript readable. Reject presenting internal summary requests as user prompts.

No mid-turn compaction, overflow recovery, provider-native compaction, summary-quality redesign, or persistent queue is included. Summaries can omit details and cost an extra model call. Existing authority, context identity, tool-pair integrity, budget, and recovery behavior must remain intact.

## Success and remaining work

Keyless Session examples and CLI transcripts demonstrate automatic and manual success, busy refusal, failure, interruption, and reopening. Tests compare later model input across handles. Terminal evidence covers each marker state. The named PRD review precedes the technical spec and implementation. No product question blocks this draft.
