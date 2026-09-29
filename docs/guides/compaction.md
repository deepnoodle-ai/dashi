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

## What it costs, and what it loses

A summary can leave out a detail that the agent needs later. The raw history
stays in the record: compaction erases nothing. The summary call has a price,
in the compaction turn's `Spend` (or `Record.Cost`).

## Limits

- Compaction does not run inside a Turn. One Turn that fills the window is not
  helped. Overflow recovery is a later feature.
- Token counts for the kept tail are estimates.
- The `dashi` CLI does not compact yet.
