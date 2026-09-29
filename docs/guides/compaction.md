# Compaction

A long session outgrows the model's context window. Then the next model call
fails and the session ends. Package `compaction` prevents this. It replaces
the older part of the history with a summary that a model writes, and it
records what the summary covers.

The package works on a history that your application owns: the messages you
pass as `Start.History`. It changes no history and keeps no state. You store
one `Record` next to your history.

## Use it

Keep three values: the raw history, the index where each Turn starts, and the
newest `Record` (nil at first).

```go
// Before each Turn, build the model view.
view, err := compaction.Project(history, rec)
if errors.Is(err, compaction.ErrCoverageMismatch) {
	log.Println("warning:", err) // the history changed; the raw history is in view
	rec = nil
}
out, err := runner.Run(ctx, turn, dashi.Start{History: view, Input: input})

// After each Turn, append its messages and note where it started.
msgs, err := dashi.Project(out.Turn)
if err != nil {
	log.Println("warning:", err)
} else if len(msgs) > 0 { // a Turn with no messages has no start
	starts = append(starts, len(history))
	history = append(history, msgs...)
}

// Then compact when the window is nearly full, or when the user asks.
last := compaction.LastUsage(out.Turn)
if policy.Due(last) {
	next, err := compaction.Compact(ctx, summarizer, history, rec,
		compaction.Options{Policy: policy, TurnStarts: starts, Last: last})
	if err == nil {
		rec = next // store it
	}
}
```

`policy` is `compaction.Policy{Model: info}`, where `info` comes from
`client.ModelInfo(model)`. `LastUsage` reads the usage of the last model call
of a Turn. Do not use `Spend.Usage`: it sums every call, and each call in a
tool loop sends the whole history again. The summarizer is
`compaction.NewSummarizer(client, compaction.SummarizerConfig{})`.
[`examples/compaction`](../../examples/compaction) is a complete program.

## What Compact does

1. It cuts only where a Turn starts, so it never splits a tool call from its
   result. A steer inside a Turn is not a Turn start.
2. It keeps the newest Turns that fit in a quarter of the context window
   (`Options.TailFraction`). If the newest Turn does not fit, it keeps none
   and summarizes all of the history. A negative `TailFraction` keeps none on
   purpose.
3. It sends the older messages to the summarizer as a text transcript. A large
   tool result is shortened in the middle to fit the summarizer's window.
   Reminders are removed. The default summarizer runs one Dashi Turn with no
   tools and the model that you choose.
4. It returns a `Record`: the summary, the model, the usage and cost of the
   summarizer call, the tokens before and after, and the coverage.

`Compact` runs between Turns. It never covers the request that comes next.
It does not run inside a Turn. Do not compact while a Turn waits to continue
(for example, for an approval, also across a restart). `Continue` must get the
same history view that the Turn started with. If not, Dashi records
`WarningHistoryMismatch` or rejects the Turn.

If `TurnStarts` has a repeat, has a wrong order, or points outside the
history, `Compact` corrects the list and lists each change in
`Record.Warnings`. Show them to the user. The compaction goes on.

If the summarizer model has a small window, the summary output allowance
shrinks to a quarter of the window. If the older messages still cannot fit,
`Compact` returns an error and no Record. The session goes on with the old
view.

## The Record

`Coverage` names the messages that the summary replaces: a count and a digest
of their canonical JSON. `Project` checks the digest each time. If a covered
message changed, moved, or is gone, `Project` returns the raw history and
`ErrCoverageMismatch`. Warn the user and go on. The session does not fail. The
next `Compact` starts again from the raw history.

Each Record covers from message 0, so store only the newest one. A restart
gives the same view: `Project` is pure, and equal inputs give equal messages.
The raw history stays on disk. Compaction erases nothing.

## When it is due

`Policy.Due(last)` is true when the last model call used 80% of the window
(`Policy.Fraction`; a negative value turns it off). It counts all input,
including cached input, plus output. `Policy.Window` returns the window from
the model's catalog entry. If the model does not state one, it returns 128,000
and `known` is false. Warn the user once.

## Errors

| Error | Meaning | Action |
| --- | --- | --- |
| `ErrNothingToCompact` | Fewer than two Turns, or nothing new to cover. No model call was made. | Tell the user. |
| `ErrNoSavings` | The summary is not smaller than what it replaces. | Keep the old view. |
| `ErrCoverageMismatch` | The Record does not match the history. | Warn. Use the raw history. |
| Any other error | The summarizer failed. The history and Record are unchanged. | Warn. Run the Turn with the old view. |

## What it costs, and what it loses

A summary can leave out a detail that the agent needs later. Raw history stays
available, so you can change the prompt (`SummarizerConfig.Prompt`) or the
tail share and compact again. The summarizer call has a price. It is in
`Record.Usage` and `Record.Cost`. Add it to your session total.

The summary is a user message with a fixed frame. It never has operator
authority, so text inside old tool output cannot become an instruction.
`Project` keeps each system or developer message of the covered part with its
own role, before the summary. The summarizer does not see them.

## Limits

- Compaction does not run inside a Turn. One Turn that fills the window is not
  helped. Overflow recovery is a later feature.
- Token counts in `TokensAfter` and the tail budget are estimates.
- The host should supply correct Turn starts. `Compact` corrects small errors
  and warns.
- A session with one long Turn cannot compact, because the only Turn is never
  covered.
