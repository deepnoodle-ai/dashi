# Compact a CLI conversation without losing its record

Status: Approved\
PRD PR: #124  Implementation PR: pending  Updated: 2026-09-30

## Problem and users

A CLI user needs to continue before the conversation exceeds the model's window. [Issue #103](https://github.com/deepnoodle-ai/dashi/issues/103) requests automatic compaction, `/compact`, and transcript markers. The CLI configures no `Session.Compactor`, and Session lacks a manual operation. Developers need both without rebuilding history and recovery.

## Context and concepts

Session already compacts before a caller or queue turn. Turn records preserve the summary, replacement history, and spend. Fresh handles honor them without a Compactor. One CLI renderer handles live and reopened records.

**Compaction** summarizes projected history while preserving the conversation and record. A **compaction turn** records the summary request and result. Its **replacement history** supplies the summary and retained messages to later calls. `Outcome` and `Stop` retain their names.

A **pending prompt** is submitted CLI input awaiting ordinary Start acknowledgment. The CLI retains it in memory.

[OpenCode](https://opencode.ai/docs/tui/#compact) offers `/compact`. [Claude Code](https://code.claude.com/docs/en/context-window#when-your-context-fills-up) combines automatic compaction with `/compact` and optional focus instructions. Dashi adopts the plain command and shared execution path.

## Use cases and acceptance

### UC-1: Continue automatically

The user sends another prompt after a completed turn reaches 80% of the context window.

- [ ] The CLI configures Session compaction in interactive, plain, and `-p` modes, including reopened sessions.
- [ ] The check uses the last completed ordinary turn's final reported model-call usage, including cached input and output.
- [ ] The selected model supplies the window and summary model. `/model` updates both for subsequent compaction.
- [ ] An unknown window uses the existing 128,000-token fallback and warns once per selected model during the process.
- [ ] Session compacts before the new caller or queue turn. That turn uses replacement history and retains its input.
- [ ] The CLI retains the triggering prompt until the recorder acknowledges the ordinary Start containing it. Starting or completing compaction does not clear it.
- [ ] Existing skip rules remain: no automatic compaction after failure, cancellation, a just-closed turn, or a compaction turn.

### UC-2: Compact deliberately

The user types `/compact` at an idle prompt. A developer uses the configured summary model and default or explicit budget:

```go
out, err := s.Compact(ctx, nil)
failed := slices.ContainsFunc(out.SessionWarnings, func(w dashi.Warning) bool {
    return w.Code == dashi.WarningCompactionFailed
})
switch {
case errors.Is(err, compaction.ErrNothingToCompact):
    fmt.Println("No eligible history; no turn recorded.")
case err != nil:
    fmt.Println("Compaction error:", err)
case failed:
    fmt.Println("Summary failed; previous history retained.")
default:
    fmt.Println("Recorded summary replaced projected history.")
}
out, err = s.Compact(ctx, &dashi.Budget{MaxOutputTokens: 2_000})
for event, err := range s.StreamCompact(ctx, nil) { /* show progress */ }
```

- [ ] Both operations force one compaction attempt without checking the usage threshold. They use the configured `Session.Compactor`.
- [ ] Successful replacement returns the recorded compaction `Outcome` and nil error. It has a completed, nonempty extracted summary and no `compaction_failed` warning.
- [ ] A recorded failed or empty summary returns the compaction `Outcome`, nil error, and `WarningCompactionFailed`. No replacement occurs, even when an empty summary stopped completed.
- [ ] Preparation and recorder errors remain Go errors. Callers can distinguish them from recorded summary failures without parsing text.
- [ ] Streaming uses existing `Event` values with the same result contract. Recorded turns end with an `Outcome`; errors before any record yield no recorded-turn `Outcome`.
- [ ] Neither operation sends ordinary user input, consumes queued input, nor starts queue turns afterward.
- [ ] No eligible work returns zero `Outcome` and canonical `ErrNothingToCompact`, matching `errors.Is(err, compaction.ErrNothingToCompact)`. No call or record occurs, including empty new sessions.
- [ ] The CLI explains this no-op. A missing Compactor returns a clear configuration error.
- [ ] Both methods accept a `*Budget`, then existing `RunOption` values for ownership, observation, and cancellation controls. The recorded start preserves the effective budget.
- [ ] A nil budget keeps the Compactor's budget or ordinary defaults. The Compactor selects the summary model, otherwise Session's configured or carried model applies.
- [ ] A supplied budget overrides the Compactor's budget. Missing session IDs retain the existing `ErrSessionNotFound` behavior.
- [ ] `/help` documents `/compact`. Arguments receive a usage notice rather than entering the conversation.

### UC-3: Handle busy, failure, and interruption

The user requests compaction while work remains open, or the summary fails.

- [ ] Manual compaction requires the last turn to have ended. Otherwise it returns `*TurnBusyError` with the turn ID, origin, and next action.
- [ ] Manual compaction never closes an ordinary interrupted, limited, waiting, or uncertain turn. The CLI explains how to continue or discard it first.
- [ ] `/compact` typed during execution receives a wait-and-retry notice. It never becomes a steer or a deferred command.
- [ ] During compaction, Session steering refuses and `/queue` remains available. The CLI enqueues an ordinary submitted line when steering reports busy with compaction origin.
- [ ] The CLI acknowledges the queued line and preserves submission order. Submitted text cleared from the input never disappears without delivery or a visible queued acknowledgment.
- [ ] `!text` during compaction enqueues `text` in the same order. A notice explains that this input cannot interrupt compaction. Ctrl-C remains the interruption action.
- [ ] An enqueue error displays the rejected text and cause so the user can resubmit it.
- [ ] After manual `/compact`, the CLI shows that queued lines wait for the next ordinary prompt under existing Start rules.
- [ ] Automatic compaction preserves caller/queue dispatch. After interruption, a retained triggering prompt takes priority over later lines as UC-4 specifies.
- [ ] Racing handles cannot run two summaries for the same compaction slot. The loser reloads the recorded result or reports busy while it runs.
- [ ] Failure retains previous projected history. The CLI shows the cause and warning using UC-2's result contract.
- [ ] Automatic failure still permits the original caller or queue turn. Manual failure starts no ordinary turn and preserves queued items.
- [ ] Ctrl-C interrupts the compaction turn. Enter or reopening can continue it with its recorded input. A later ordinary prompt can close it under existing Session rules.
- [ ] Recorder errors and context cancellation retain their existing meanings. Only a recorded completed summary permits a success marker.

### UC-4: Continue an interrupted automatic compaction

The user submits a prompt, then presses Ctrl-C while automatic compaction runs before the ordinary turn starts.

- [ ] The CLI shows the triggering prompt as pending. Its text remains available after the input field clears.
- [ ] Enter continues the recorded compaction. After successful completion, the CLI sends the original prompt once, before text submitted later.
- [ ] Pre-existing queued input keeps its order before the triggering prompt. Later queued lines retain submission order after it.
- [ ] The CLI clears retained input only after recording acknowledgment for its ordinary Start. A recorder error or unknown acknowledgment retains it for reconciliation.
- [ ] A brand-new prompt may use existing Exclusive damaged-history skipping before any admission attempt. The first acknowledgment or CommitError freezes its generated identity. This exception never bypasses reconciliation of an attempted Start. Ordinary pre-admission refusals require resubmission after recovery.
- [ ] Reconciliation uses the original command identity before resending. Recorded input is never delivered twice, including after a lost acknowledgment.
- [ ] If continuation stops again, the prompt stays pending. An ordinary new prompt follows existing close rules and preserves pending input order.
- [ ] Exit and session-switch actions warn about both pending prompts and queued lines held only in memory. Repeating the action accepts their loss.
- [ ] Reopening can recover recorded compaction and ordinary turns. It cannot recover memory-only pending input, and the exit warning states this limitation.
- [ ] Keyless evidence checks interruption before ordinary Start, continuation, input order, and one original-prompt delivery, including a lost acknowledgment.

### UC-5: Read and reopen the transcript

The user sees compaction progress, then reopens the session and sends another prompt.

- [ ] Live and reopened transcripts identify compaction by `StartCompaction` origin, with running, completed, interrupted, and failed states.
- [ ] The transcript hides the summary request and summary text. A compaction marker and footer retain the turn number, usage, and cost.
- [ ] `-p` prints only the ordinary answer to stdout. Compaction markers and warnings go to stderr.
- [ ] Reopening preserves ordinary scrollback, session title, queued-input rules, and the existing replacement history without separate summary storage.

## Decisions and scope

- Add Session methods, with a streaming counterpart and explicit nullable budget. Reject a sixth Runner command, caller-authored facts, or a global budget option.
- Compact only ended turns manually. Preserve the user's continuation choice. Reject implicitly canceling an interrupted ordinary turn.
- Leave queued input untouched in manual compaction. Enqueue CLI input that compaction cannot steer. Reject dropping submitted lines or executing pending work implicitly.
- Retain the triggering prompt until its ordinary Start is acknowledged. Preserve its command identity and input order. Reject requiring silent resubmission or adding persistent queue storage.
- Reuse the existing 80% policy and compaction records. Reject a CLI history store, threshold flag, focus prompt, or new record format.
- Show markers instead of summary content. Keep the coding transcript readable. Reject presenting internal summary requests as user prompts.

Exclude mid-turn compaction, overflow recovery, provider-native compaction, summary-quality changes, focus arguments, policy controls, and persistent queues. Summaries can omit details and cost a call. Preserve authority, context identity, tool pairs, budgets, and recovery.

## Success and remaining work

Keyless examples and CLI transcripts cover automatic/manual success, busy refusal, failure, interruption, reopening, input order, and exactly-once prompt delivery. Tests compare later model input across handles. Terminal evidence covers each marker state. Named PRD review precedes the technical spec and implementation. No product question blocks this draft.
