# Compaction

A long session outgrows the model's context window. Then the next model call
fails and the session ends. Package `compaction` prevents this. It replaces
the older part of the history with a summary that a model writes, and the
record keeps what the summary covers.

## Use it with a session

Set `Session.Compactor`:

```go
info, _ := client.ModelInfo(model)
s := &dashi.Session{
	Runner:    dashi.Runner{Client: client, Recorder: rec},
	Compactor: compaction.New(compaction.Policy{Model: info}),
}
out, err := s.Send(ctx, text) // may run a compaction turn first
```

Before each new turn, the caller's or a queue turn, the session asks the
compactor whether the last turn's final model call used 80% of the window. If
so, it runs a **compaction turn** first, in the next turn slot: origin
`compaction`, command ID `compact:` and its turn ID, no tools, no steering. Its
one model call is the summary request, so its spend and recovery are those of
any turn, and the caller's `Start.Budget` caps it too. Then the caller's turn
runs with the **replacement history**: the kept messages, with the summary in
their middle, followed by the turns after the compaction turn.
[`examples/compaction`](../../examples/compaction) runs this without an API key.

- **Every reader sends the same history.** The compaction turn records the
  kept messages at its start, and its answer is the summary. A handle with no
  compactor, such as a viewer or a recovery worker, builds the same projected
  history from the record, and loads turns back to the newest compaction turn
  only.
- **The conversation is untouched.** `s.History` never includes compaction
  turns; `s.Turns` does. A UI hides them by `TurnView.Origin`, or shows a
  marker. `s.ProjectedHistory` is what the next turn sends.
- **Steering waits.** While a compaction turn runs, `s.Steer` refuses with a
  `*TurnBusyError` whose `Origin` is `compaction`. `Enqueue` works.

## What the Compactor chooses

1. It keeps the newest messages that fit in a quarter of the window
   (`Compactor.TailFraction`), cutting only where a user message other than
   a tool result starts, so it never splits a tool call from its result. If
   nothing fits, it keeps none and summarizes all of the history. A negative
   `TailFraction` keeps none on purpose.
2. It sends the older messages as a text transcript with the prompt
   (`Compactor.Prompt`). A large tool result is shortened in the middle to fit
   the window. Reminders are removed, except agent mail: a reminder with a
   `From`, such as a subagent's answer, becomes an item that names its sender.
3. It keeps each system or developer message of the covered part with its
   own role, ahead of the summary. The summarizer does not see them.
4. `Compactor.Model` names the summary model; empty means the session's. When
   the catalog gives it a smaller window than the session model's, the
   transcript is sized to fit that window.

The summary is a user message: a fixed frame, then the text inside the answer's
last `<summary>` and `</summary>` tags, or the whole answer when it has none.
The default prompt asks for the tags, so a preamble such as "Here is the
summary:" is left out. A reader with no compactor applies the same rule. The
summary never has operator authority, so text inside old tool output cannot
become an instruction.

## When it is due

`Policy.Due(last)` is true when the last model call used 80% of the window
(`Policy.Fraction`; a negative value turns it off). It counts all input,
including cached input, plus output. `Policy.Window` returns the window from the
model's catalog entry. If the model does not state one, it returns 128,000 and
`known` is false. Warn the user once. A zero `Compactor` uses 128,000 whatever
the session's model is, so pass the model's `ModelInfo` as above.

The session checks only after a turn that completed. It never compacts after a
turn that failed or was canceled, after one the new turn had to close, or right
after a compaction turn.

## When it fails

If the compaction turn does not complete with a summary, the session closes it
(`Cancel` with command ID `close:` and its turn ID), keeps the previous
projected history, and runs the caller's turn with the warning
`compaction_failed` in `Outcome.SessionWarnings`. A failing summary is tried
again only after the next turn completes. A compactor that cannot build its
request warns the same way, and no compaction turn is recorded. When two handles
reach the compaction slot at once, one runs the compaction turn and the other
uses its replacement history, with no warning.

Each compaction turn records a digest of the history it replaced. A handle
that already holds the covered turns checks it; a mismatch means a covered
turn changed, which is a bug. The session warns `compaction_mismatch` and
still uses the compaction turn, so every reader sends the same history.

## Without a session

A host that drives `Runner.Run` itself and owns its history uses `Compact`,
`Project`, and a stored `Record`. Keep the raw history, the index where each
Turn starts, and the newest `Record` (nil at first):

```go
view, err := compaction.Project(history, rec) // before each Turn
if errors.Is(err, compaction.ErrCoverageMismatch) {
	rec = nil // the history changed; the raw history is in view
}
// ... run the Turn with Start.History = view, then append its messages
// and note where it started ...
if last := compaction.LastUsage(out.Turn); policy.Due(last) {
	next, err := compaction.Compact(ctx, summarizer, history, rec,
		compaction.Options{Policy: policy, TurnStarts: starts, Last: last})
	if err == nil {
		rec = next // store it
	}
}
```

The summarizer is `compaction.NewSummarizer(client,
compaction.SummarizerConfig{})`; it runs one Turn with no tools and discards
its record. `Compact` cuts as the Compactor does, but only at the Turn starts
you pass; bad starts are corrected and listed in `Record.Warnings`. Do not
use `Spend.Usage` for `Due`: it sums every call, and each call in a tool loop
sends the whole history again. Do not compact while a Turn waits to continue:
`Continue` must get the history the Turn started with.

`Record.Covers` names the messages the summary replaces: a count and a
digest. `Project` checks the digest each time and, on a mismatch, returns the
raw history with `ErrCoverageMismatch`. Each Record covers from message 0, so
store only the newest one.

| Error | Meaning | Action |
| --- | --- | --- |
| `ErrNothingToCompact` | Fewer than two Turns, or nothing new to cover. No model call was made. | Tell the user. |
| `ErrNoSavings` | The summary is not smaller than what it replaces. | Keep the old view. |
| `ErrCoverageMismatch` | The Record does not match the history. | Warn. Use the raw history. |
| Any other error | The summarizer failed. The history and Record are unchanged. | Warn. Run the Turn with the old view. |

## Persistent context

Keep `llm.Message.Context` when retaining messages. `Message.Clone` and JSON
storage preserve this slot identity. `SeedContext` and skill catalogs avoid
another delivery while their latest unchanged value remains visible. If
compaction removes it, they deliver it again. A summary mentioning the value
does not establish presence. Reconstructing retained messages from text alone
loses identity. Old compaction records without identity keep their messages
and receive one new identified delivery.

## What it costs, and what it loses

A summary can leave out a detail that the agent needs later. The raw history
stays in the record: compaction erases nothing. The summary call has a price,
in the compaction turn's `Spend` (or `Record.Cost`).

## Manual Session compaction

Call `s.Compact(ctx, nil)` to request a summary before the automatic
threshold. Nil retains the budget supplied by the configured Compactor.
`s.Compact(ctx, &dashi.Budget{MaxOutputTokens: 1000})` caps that recorded
summary. Later Run commands keep their own budget selection.
`StreamCompact` accepts the same arguments and streams progress plus the
compaction Outcome. Existing RunOption controls apply.

Manual compaction requires an ended last turn. It refuses interrupted or
waiting ordinary work with `*dashi.TurnBusyError`; continue or explicitly
cancel that work first. It preserves queued input and starts no queue turn.
No eligible history returns zero Outcome and `dashi.ErrNothingToCompact`,
which is also `compaction.ErrNothingToCompact`. A recorded failed or empty
summary returns its Outcome with `compaction_failed` and keeps the old
projection. Preparation and recorder failures remain Go errors.

The CLI enables automatic compaction and offers `/compact`. See the
[CLI guide](cli.md) for markers, pending input, and recovery.

## Protect one long request

Set `Agent.ActiveCompaction` when a single request makes many tool calls:

```go
agent := dashi.Agent{
    Model: model,
    ActiveCompaction: &dashi.ActiveCompactionPolicy{},
}
```

Before ordinary dispatch, the Runner sizes the prepared provider body, including
verified attachment bytes, tools, hooks, and replay data. At the default 80%
threshold it can summarize complete settled exchanges. It keeps the original
request, caller-owned history, operator steering, and named context. A summary
retains the objective, findings, changed files, verification, and remaining work.
Completed tools remain recorded and are not executed again by compaction.

The policy uses the client's context-window evidence. Set `Window` when the
client has no window; a known smaller client window still caps that value.
If neither is known, the Turn records `active_compaction_unavailable` and runs
without active protection. `Fraction` defaults to 0.8 and `MaxTokens` to 2048.
`Disabled: true` explicitly overrides inherited protection in a Session.

The threshold is proactive, not a hard refusal. Without eligible coverage, a
request that still fits can dispatch. A hard no-fit stops with
`LimitContextWindow` and reason `context_window`. Protected input may fill the
window even after all eligible exchanges are replaced. Preserve the recorded
work and start a new Turn with a larger model or smaller input. Estimates use
prepared bytes plus a margin; they do not guarantee provider acceptance. Dashi
does not compact and retry after a provider overflow.

Summary attempts use the same sender, hooks, billing identities, attempts,
and cumulative budget as ordinary work. `ModelPlan.Purpose`,
`SendPlan.Purpose`, and `ModelFact.Purpose` identify `ModelPurposeCompaction`.
Summary calls offer no tools or structured output and never become the Turn's
answer. They leave one ordinary attempt available, which permits a final
response but does not guarantee completion after another tool selection.

`Project` and `Session.History` retain ordinary messages and omit summary work.
`ProjectContext` and `Session.ProjectedHistory` apply recorded replacements.
`TurnView.ContextCompactions` exposes the applied text and estimated savings;
actual summary usage and money remain in `Spend`. The two summary mechanisms
can coexist: `Session.Compactor` works between Turns and never recursively
starts active protection inside its own summary Turn.

Failed, empty, tool-requesting, or non-shrinking summaries record their raw
result and accounting, then stop with `compaction_failed`. Explicit `Continue`
can admit a new summary attempt. After a successful recorded settlement,
reload applies its replacement without another model call. An interrupted
no-tools summary follows existing safe recovery. An ambiguous recorder write
requires authoritative reload before any further dispatch. Cancellation ends
work; interruption preserves continuation. The raw facts always remain.

Readers must understand the new `context_compacted` step and compaction-purpose
model records. Upgrade readers before producing these records; older readers
reject unfamiliar step kinds. Existing records and between-Turn behavior stay
compatible. `ExampleActiveCompactionPolicy` runs the workflow without keys.

## Limits

- The optional `compaction` package manages history between Turns. Enable
  `Agent.ActiveCompaction` separately for the current Turn.
- Token counts for the kept tail and prepared-body sizing are estimates.
