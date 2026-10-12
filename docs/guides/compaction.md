# Compaction

A long session outgrows the model's context window. Then the next model call
fails and the session ends. Package `compaction` prevents this. It replaces
the older part of the history with a summary that a model writes, and the
record keeps what the summary covers.

## Use it with a session

Set `Session.Compactor`:

```go
info, _ := client.ModelInfo(model)
s := &session.Session{
	Runner:    dashi.Runner{Client: client, Recorder: rec},
	Compactor: compaction.New(compaction.Policy{Model: info}),
}
out, err := s.Send(ctx, text) // may run a compaction turn first
```

Before each new turn, the caller's or a queue turn, the session asks the
compactor whether the last turn's final model call reached the **due count**,
by default a fifth of the window. If so, it runs a **compaction turn** first, in the next turn slot: origin
`compaction`, command ID `compact:` and its turn ID, no tools, no steering. Its
one model call is the summary request, so its spend and recovery are those of
any turn, and the caller's `Start.Budget` caps it too. Then the caller's turn
runs with the **replacement history**: the kept messages, with the summary in
their middle, followed by the turns after the compaction turn.
[`examples/compaction`](../../examples/compaction) runs this without an API key.

- **Every reader sends the same history.** The compaction turn records the
  kept messages at its start, and its reply is the summary. A handle with no
  compactor, such as a viewer or a recovery worker, builds the same history
  from the record, and loads turns back to the newest compaction turn
  only.
- **The conversation is untouched.** `s.History` never includes compaction
  turns; `s.Turns` does. A UI hides them by `TurnView.Origin`, or shows a
  marker. `s.NextHistory` is what the next turn sends.
- **Steering waits.** While a compaction turn runs, and while the compactor
  builds its Start, `s.Steer` and a new `s.Send` refuse with a
  `*TurnBusyError` whose `Origin` is `compaction`. `Enqueue` works. The session calls `Compactor.Start`
  without holding its lock, with the context of the call that asked for the
  compaction, so a compactor may take its time, such as to ask a model which
  messages to keep, without blocking the handle.

## What the Compactor chooses

1. It sizes the kept tail and the summary from the **room**: the due count
   less the fixed part, which is system and developer messages plus what the
   last call sent besides the history (tool definitions, instructions). The
   summary's output allowance is at most a quarter of the room
   (`Compactor.MaxTokens`, default 16,384, also at most a quarter of the
   summary model's window), and the tail at most a quarter
   (`Compactor.TailFraction`); together at most half. So the replacement
   history stays well below the due count.
2. It keeps the newest messages that fit the tail, cutting only where a user
   message other than a tool result starts, so it never splits a tool call
   from its result. If nothing fits, it keeps none and summarizes all of the
   history. A negative `TailFraction` keeps none on purpose.
3. It sends the older messages as a text transcript with the prompt
   (`Compactor.Prompt`). A large item is shortened in the middle to fit the
   summary model's window; if even 1 KB per item does not fit, whole items
   are dropped from the middle, keeping the oldest and newest, behind one
   "[N items omitted]" marker. Reminders are removed, except agent mail: a
   reminder with a `From`, such as a subagent's reply, becomes an item that
   names its sender.
4. It keeps each system or developer message of the covered part with its
   own role, ahead of the summary. The summarizer does not see them.
5. It records a **summary fallback** (`CompactionFact.SummaryFallback`),
   built with no model call: a note that N earlier messages were removed,
   then their user messages, the names of the tools called, and the agent's
   last reply, cut to half the summary's allowance.
6. `Compactor.Model` names the summary model; empty means the session's. When
   the catalog gives it a smaller window than the session model's, the
   transcript is sized to fit that window.

The summary is a user message: a fixed frame, then the text inside the reply's
last `<summary>` and `</summary>` tags, or the whole reply when it has none.
The default prompt asks for the tags, so a preamble such as "Here is the
summary:" is left out. A reader with no compactor applies the same rule. The
summary never has operator authority, so text inside old tool output cannot
become an instruction.
`compaction.IsSummary(m)` tells whether a message is such a summary, from
this package's `Compactor` or from `Agent.ActiveCompaction`, or a summary
fallback; match it rather than the frame text, which may change.

## When it is due

`Policy.Due(last)` is true when the last model call reached the due count:
`Policy.DueTokens` when positive, else `Policy.Fraction` of the window
(`DefaultFraction`, 0.2). `DueTokens` is capped at the window only when the
window is known. A negative `Fraction` turns automatic compaction off, even
with `DueTokens`; `Session.Compact` still works. `Due` counts all input,
including cached input, plus output. `Policy.Window` returns the window from
the model's catalog entry. If the model does not state one, it returns
200,000 (`DefaultWindow`) and `known` is false. Warn the user once. A zero
`Compactor` uses 200,000 whatever the session's model is, so pass the
model's `ModelInfo` as above.

`Compactor.Needed` also estimates tokens from the history's size, at about 4
bytes per token. This catches a large tool result added after the last model
call reported its usage. History with images, audio, video, or binary
documents skips the estimate, since its size says little about tokens.
`Needed` is false when nothing can be summarized, or when the replacement
would itself be due (the fixed part alone is too big), so compaction never
repeats after every turn.

The session checks after any ended turn but a compaction turn: completed,
canceled, ended at a limit, or closed by the new Start. It also checks after
a turn that stopped because its history no longer fit
(`session.NeedsCompaction`).

## When the summary fails

Compaction never leaves a history too long to send. Once a compaction turn
has ended without a summary (failed and closed, canceled, limited, or an
empty reply), its replacement history uses the summary fallback in place of
the summary, with no frame; a custom compactor that records none gets a
fixed note. Every history built from it warns `summary_fallback`
(`dashi.WarningSummaryFallback`, with the turn's ID). The session closes a
stopped compaction turn at once (`Cancel` with command ID `close:` and its
turn ID), except one held for budget (a host limit without `Ends`): that
one waits, and a new Start refuses with a `*TurnBusyError` until it ends. A
failed summary is not retried at that position; the next compaction comes
when the history is due again. A compactor whose `Start` fails returns its
error; `ErrNothingToCompact` means no compaction is needed. When two handles
reach the compaction slot at once, one runs the compaction turn and the other
uses its replacement history, with no warning.

Each compaction turn records a hash of the history it replaced. A handle
that already holds the covered turns checks it; a mismatch means a covered
turn changed, which is a bug. The session warns `compaction_mismatch` and
still uses the compaction turn, so every reader sends the same history.

## Durable hosts

A host that saves turns in a database and runs them in workers asks one
question at admission, before each work turn:

```go
start, needed, err := compactor.StartIfNeeded(ctx, turns) // oldest first
if needed {
	// Record under start.CommandID, "compact:" and the last turn's ID; a
	// worker drives it. Then admit again.
	runner.Record(ctx, dashi.NewTurn(id), start)
}
history, warnings := dashi.BuildHistory(turns) // starts from the replacement
```

`StartIfNeeded` applies the session's rule to the stored turns: not needed
while the last turn has not ended or is a compaction turn; needed when it
overflowed or `Needed` says so. It makes no model call and writes nothing.
Its command ID names the compaction turn by the turn it follows, so a retry,
a restarted worker, or a second worker finds the same turn, and the first
record wins. The Start holds everything: the summary request, the kept
messages, the hash, and the fallback, so a crash at any point leaves either
no compaction turn or one whose replay is already decided. The host keeps no
operation table.

A compaction turn that stops without ending (a failed summary) is the host's
to close: `Cancel` it through the inbox, unless it is held for budget, and
admit again; replay then uses the fallback. A host that would rather not wait
for a budget hold may cancel it too. `ExampleCompactor_StartIfNeeded` records,
drives, crashes midway, and resumes without an API key.

`BuildHistory` walks back from the newest turn; at the newest ended
compaction turn it uses that turn's replacement history and stops, so the
host may pass only the turns from there on. A turn that cannot be replayed is
left out with `turn_unreadable`. Set `Compactor.Model`, or the Start's
`Agent`, when the Runner has no default model. Do not compact while a turn
waits to continue: `Continue` must get the history the turn started with.

## Persistent context

Keep `llm.Message.Context` when retaining messages. `Message.Clone` and JSON
storage preserve this slot identity. `SeedContext` and skill catalogs do not
add the item again while their latest unchanged value remains visible. If
compaction removes it, they add it again. A summary mentioning the value does
not count as the item being present. Reconstructing retained messages from
text alone loses identity. Old compaction records without identity keep their
messages and receive one new identified copy.

## What it costs, and what it loses

A summary can leave out a detail that the agent needs later. The raw history
stays in the record: compaction erases nothing. The summary call has a price,
in the compaction turn's `Spend`.

## Manual Session compaction

Call `s.Compact(ctx, nil)` to request a summary before the automatic
threshold. Nil retains the budget supplied by the configured Compactor.
`s.Compact(ctx, &dashi.Budget{MaxOutputTokens: 1000})` caps that recorded
summary. Later Run commands keep their own budget selection.
`StreamCompact` accepts the same arguments and streams progress plus the
compaction turn's `RunResult`. Existing RunOption controls apply.

Manual compaction requires an ended last turn. It refuses interrupted or
waiting ordinary work with `*session.TurnBusyError`; continue or explicitly
cancel that work first. It preserves queued input and starts no queue turn.
No eligible history returns a zero `RunResult` and
`session.ErrNothingToCompact`, which is also `compaction.ErrNothingToCompact`.
A recorded failed or empty summary returns its `RunResult` with
`summary_fallback`, and the fallback replaces the history. Preparation and
recorder failures remain Go errors.

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

Before sending an ordinary text-only request, the Runner sizes the prepared
provider body, including tools, hooks, and replay data. At the default 80%
threshold it can summarize complete, finished exchanges. It keeps the original
request, caller-owned history, operator steering, and named context. A summary
retains the objective, findings, changed files, verification, and remaining work.
Completed tools remain recorded and are not executed again by compaction.

The policy uses the client's context-window evidence. Set `Window` when the
client has no window; a known smaller client window still caps that value.
If neither is known, the Turn records `active_compaction_unavailable` and runs
without active protection. `Fraction` defaults to 0.8 and `MaxTokens` to 2048.
`Disabled: true` explicitly overrides inherited protection in a Session.

The threshold is proactive, not a hard refusal. Without eligible coverage, a
request that still fits can be sent. A hard no-fit stops with
`LimitContextWindow` and reason `context_window`. Protected input may fill the
window even after all eligible exchanges are replaced. Preserve the recorded
work and start a new Turn with a larger model or smaller input. Estimates use
prepared bytes plus a margin; they do not guarantee provider acceptance. Dashi
does not compact and retry after a provider overflow.

Prepared image, document, audio, or video content makes active context sizing
unavailable for that request, including media in history, verified references,
and tool results. The Turn records `active_compaction_media_estimate_unavailable`
once. It skips proactive compaction and the byte-based context limit for that
request. The recorded policy stays active for later text-only requests. Provider
content preparation and actual window acceptance govern media requests; neither
preparation nor a known window proves token fit. Budget checks still apply:
their input estimate counts each media item at a fixed amount instead of its
bytes, so a cost cap can still refuse the request.

Summary calls use the same sender, hooks, billing identities, model-call
limit, and cumulative budget as ordinary work. `ModelPlan.Purpose`,
`SendPlan.Purpose`, and `ModelFact.Purpose` identify `ModelPurposeCompaction`.
Summary calls offer no tools or structured output and never become the Turn's
reply. They leave one ordinary model call available, which permits a final
reply but does not guarantee completion after another tool selection.

`TurnMessagesFull` and `Session.History` retain ordinary messages and omit
summary work.
`TurnMessages` and `Session.NextHistory` apply recorded replacements.
`TurnView.ContextCompactions` exposes the applied text and estimated savings;
actual summary usage and money remain in `Spend`. The two summary mechanisms
can coexist: `Session.Compactor` works between Turns and never recursively
starts active protection inside its own summary Turn.

Summary prompts state how many output tokens the summary may use. If a
summary is cut off at that limit, Dashi asks once for a shorter summary of the
same history. It does not retry when the cut-off summary has no text, calls a
tool, or refuses. Reloading the Turn does not allow a second retry. Both calls
count toward the Turn's limits and spend. A cut-off summary never replaces
history. Other failed, empty, or tool-calling summaries, and summaries cut off
for length again, stop the Turn with `compaction_failed`. Explicit `Continue`
can allow a new summary call. After a summary call's result is recorded,
reload applies its replacement without another model call. An interrupted
no-tools summary follows existing safe recovery. A recorder write with an
unknown result requires reloading from the recorder before any further call
is sent. Cancellation ends work; interruption preserves continuation. The raw facts always remain.

Readers must understand the new `context_compacted` step and compaction-purpose
model records. Upgrade readers before producing these records; older readers
reject unfamiliar step kinds. Existing records and between-Turn behavior stay
compatible. `ExampleActiveCompactionPolicy` runs the workflow without keys.

## Limits

- The optional `compaction` package manages history between Turns. Enable
  `Agent.ActiveCompaction` separately for the current Turn.
- Token counts for the kept tail and prepared-body sizing are estimates.

A summary hook can add media too. Its request receives the same unavailable-size
handling. Application still requires strict savings in the prepared replacement:
large new hook content can reject the summary with `compaction_failed`. The raw
result and accounting remain recorded, and no replacement is applied.
