# Durable summarization

**Status:** Accepted · **Date:** 2026-10-12 · **Follows:** [advancing
turns](advancing-turns.md) item 7, which deferred this until `Record`,
`Advance`, and `Drive` shipped (v0.7.0).

## Problem

A host that saves turns in a database and runs them in workers wants what a
`session.Session` gets from `Session.Compactor`: before the next turn of a
conversation, if the history is too long, summarize the older part first. The
summary must be paid for like any model call, survive a crash at any point,
and run once. And it must not fail in a way that leaves the conversation
stuck: a shorter history of lower fidelity beats a history too long to send.

Dashi has the pieces. `Compactor.Start` builds a compaction turn,
`Runner.Record` saves it, `Drive` runs it, and `BuildHistory` uses it once it
completes with a summary. Two gaps remain. A host must answer two questions
it must not get wrong: *is a compaction due after these turns?* and *how do
two attempts agree they are the same compaction?* And a compaction turn that
ends without a summary replaces nothing today, so the old, overflowing
history comes back. Sessions warn `WarningCompactionFailed` and send it
anyway.

## What hosts do today

Our first durable host answers both questions with its own state machine: an
operation row that freezes the work request, the history, the policy, and the
last usage under a digest; plan, bind, activate, and fail transitions; a
filter that counts only "activated" compaction turns; and admission code that
replays the frozen request after a lost acknowledgment.

| Host code | Lines today | After |
| --- | --- | --- |
| Compaction adapter | 369 | ~110 |
| Admission: frozen-request replay, two bind calls | ~40 | 0 |
| Conductor: fail planned operations on stop | ~20 | 0 |
| Repository: operation file, type, busy check, schema | ~310 | 0 |
| Repository contract test for operations | 104 | 0 |

Most of it restates facts the compaction turn's record already holds. The
saved Start *is* the frozen plan: its `Input` holds the transcript, and its
`CompactionFact` holds what is kept and the `Hash` of what is replaced. The
conversation's turn slot already keeps other work out while the compaction
turn runs.

## Proposal

**Idea 1: a compaction turn is named by the turn it follows.** Its command ID
is `"compact:" + lastTurnID`, and the host admits it under that key alone,
not a digest of the work request that asked for it. A retry, a restarted
worker, or a second worker for other work at the same position finds the same
turn; the first admitter's account pays. One position gets one attempt, as in
`session`. The host needs no operation row, no digest, and no activation
step.

**Idea 2: every ended compaction turn replaces the history.** `Start`
records, beside `Keep`, a mechanical stand-in for the summary:
`CompactionFact.SummaryFallback`. It is built with no model call: a note that
N earlier messages were removed without a summary, then, from that part, the
user's messages, the name of each tool call, and the agent's last reply, as
text. The tool names keep the model from redoing work, such as sending an
email twice, that the user's words alone would invite. It is cut with the
same `FitTranscript` as the summary request (failure 2), to half the
summary's output allowance, so even with the byte-based token estimate a
fallback history is no longer than a summarized one. `ReplacementHistory`
then uses:

- `Frame` and the summary, when the turn completed with one;
- `SummaryFallback` as recorded, with no `Frame` (it is not a summary), when
  the turn ended (`Stop.Ended`) any other way: failed, canceled, limited, or
  completed with an empty reply;
- nothing, while the turn has not ended. No turn is admitted after an unended
  compaction turn, so this history is never sent.

The stop that ends a compaction turn without a summary records
`WarningSummaryFallback` (with the turn's ID), once, so it is in that turn's
`RunResult` and `TurnView.Warnings`, and in the stop step a `Drive` or
`FollowNext` host reads; `BuildHistory` does not repeat it. Hosts show
"summarized" or "shortened without a summary", and `session` replaces
`WarningCompactionFailed` with it. `IsSummary` also recognizes the stand-in.
An empty `SummaryFallback` (from a custom compactor) replays as a fixed note.

The fallback lives in the record and in replay because the outcome is then a
pure function of the record. There is no second write for a crash to skip, no
repair step, and no extra money: the stand-in was paid for in bytes at Start.
Building it at replay from `Input` instead would mean parsing the summary
request, which a custom compactor shapes as it likes, and a library upgrade
would change old histories. A fixed note alone needs no field, but leaves the
model with no trace of what was done.

**Idea 3: one rule, read from the record.** `compaction` exports it, and
`session.planCompaction` changes to the same rule (a shared test runs both
over the same turns):

```go
// StartIfNeeded returns the Start of the compaction turn to run before the
// turn that follows turns (oldest first, as BuildHistory takes them).
//   - The last turn has not ended, or is a compaction turn: not needed.
//     Its replacement, summarized or fallback, is the history.
//   - The last turn overflowed (session.NeedsCompaction): needed.
//   - Otherwise: needed when Needed(BuildHistory(turns), LastUsage(last)).
//   - Not needed when nothing is old enough to compact, or when the
//     replacement would itself be due again (see failure 3), so compaction
//     never repeats after every turn.
// The Start's CommandID is "compact:" and the last turn's ID. It makes no
// model call and writes nothing. Its only errors are encoding errors.
func (c *Compactor) StartIfNeeded(ctx context.Context, turns []dashi.Turn) (
	start dashi.Start, needed bool, err error)
```

Session's "only after a completed turn" becomes "after any ended turn": a
host that ends turns at limits (`Limits.NoRaise`) still grows history. The
host's "at least two ordinary turns" minimum is dropped on purpose.

**Also: when compaction is due.** At the *due count*: `Fraction` of the
model's window, now 0.2 by default (`DefaultFraction`, was 0.8), or the new
`Policy.DueTokens`, an exact count that overrides `Fraction` when positive
(zero or negative is unset; capped at the window only when the window is
known). An unknown window is `DefaultWindow`, now 200,000
(was 128,000). A negative `Fraction` still turns automatic compaction off,
even with `DueTokens`; manual compaction remains, which covers a host's
"manual" and "disabled". The kept tail (`TailFraction`) and the summary's
output allowance are shares of the room the due count leaves after the *fixed
part* (system and developer messages, and tool definitions: the last call's
input minus the history's estimate), together at most half of that room: the
allowance at most a quarter of it, and the tail `TailFraction` of it (a
quarter by default) and no more than the half less the allowance. The
allowance is also at most a quarter of the summarizer's window, whose full
size the summary request's input gets. Since `Start` must estimate the tool
definitions too, `session.Compactor.Start` takes the last call's usage, as
`Needed` does. Size presets (10% to 80%) are product
vocabulary; a host maps them to `Fraction`. `session` and the CLI then
compact at 20% instead of 80%, which costs nothing before Dashi has users.
The host's ~30 lines of window arithmetic become one field.

## Each failure, and its answer

1. **The summary call fails** after its retries, or the reply has no summary.
   The engine ends the turn at that stop, so replay uses `SummaryFallback`
   with no host step. Not retried at this position; the next compaction comes
   when the history is due again.
2. **The summary request is too big for the summarizer.** `Start` already
   cuts large transcript items in the middle. When even 1 KB per item does
   not fit, it now drops whole items from the middle with one "[N items
   omitted]" marker, keeping the oldest (the task) and the newest. `Start` no
   longer fails on size.
3. **Nothing old enough to compact, or the kept part too big.** The kept tail
   fits its budget, and the summary or fallback its allowance, both sized
   from the room left after the fixed part. A single oversized turn has no
   tail: it is all summarized, its transcript cut as in 2. What compaction
   cannot shrink is the fixed part; `Keep` holds system and developer
   messages outside the tail budget and never changes them, since they are
   operator instructions. When the fixed part alone
   would make the replacement due (or are all there is,
   `ErrNothingToCompact`), `StartIfNeeded` says "not needed", and the next
   turn reports its own context-window stop: a configuration problem, not a
   compaction one. A custom `session.Compactor` whose `Start` fails otherwise
   is a bug in that code, and the session returns its error instead of
   sending a history it knows is due.
4. **A crash, or a second worker.** `SummaryFallback` is written in the same
   `Record` as the Start, and the outcome is read from the record, so a crash
   at any point leaves either no compaction turn (the next admission builds
   the same command ID) or one whose replay is already decided. A second
   worker meets the same key; the first record wins.
5. **Budget.** A summary held for budget has not ended, so it waits like any
   held turn; the work turn after it would usually need the same money. The
   fallback needs none, so a host that would rather not wait, for example
   when the waiting work bills another account than the one the summary holds
   (Idea 1), may `Cancel` the held turn and continue with the fallback.

## Host code

**After** (all of it runs at admission, so a restart repeats it):

```go
turns := conversationTurns(conv) // oldest first
last := lastOf(turns)
if !last.Stop.Ended {
	return errBusy // running, or a summary held for budget: wait
}
start, needed, err := compactor.StartIfNeeded(ctx, turns)
if err != nil {
	return err
}
if needed {
	admit(ctx, conv, start, start.CommandID) // Runner.Record; a worker Drives it
	waitStopped(ctx, start.CommandID)        // FollowNext
	return retry()                           // run these steps again
}
history, warnings := dashi.BuildHistory(turns)
notify(conv, warnings)
// The compaction turn's own view warns WarningSummaryFallback ("earlier
// messages shortened") once, when it ends.
// ... admit the work turn with Start.History = history ...
```

The work request is not frozen: until the work turn is admitted nothing of it
is recorded, and the host rebuilds it for the same wake, so a retry that
picks up a newer input is still correct.

## What the host deletes

About 730 of ~840 lines (table above), ~630 of them outside tests, plus the
operation table, its generated Ent code, the `CompactionID` request field,
and its "conversation full" error path. Four adapter tests shrink;
"activation acknowledgment lost" has nothing left to test. Idea 1 with v0.7.0
does most of it; `StartIfNeeded` and `DueTokens` remove the ~70 lines of
decision and window code and keep the host and `session` on one rule.

Known limit: the key has no model. If the conversation's model changes, the
compaction turn already at that position stands; the next one uses the new
model.

## Implementation notes

- The engine ends a compaction turn (origin `StartCompaction`) at every
  recorded stop except two: a host hold, a host limit without `Ends` (the
  stop Drive waits on), and an interruption with no model call in flight,
  which `Continue` resumes on any worker. `replayState.stopEnds` holds the
  rule, so Run, Continue, Advance, and Drive record the same `Stop.Ended`
  and replay checks it. A stop at its own `Start.Budget` or model-call cap,
  a failure, a refusal, or an interruption mid-call ends it; a late model
  result still corrects its spend. The same stop records
  `WarningSummaryFallback` when it ends the turn without a summary.
- `session` closes nothing itself; a new Start still closes an interrupted
  compaction turn, as it closes any stopped turn, and refuses after a held
  one. A `session` caller that never sees the compaction turn's result gets
  its warning in the next turn's `SessionWarnings`.
- `session` returns any error from recording or running the compaction turn,
  as it does a custom `Start` error: no path sends the history that is due.
- When the fixed part takes more than half the due count, the due count
  rises to the fixed part plus half of it, up to the window. At the 0.2
  default, many tool definitions would otherwise turn compaction off until
  the window overflowed, and then leave no room. Only a fixed part that fills
  the window gives "not needed", and `Start` then returns
  `ErrNothingToCompact`, so neither the session's overflow path nor a manual
  compaction makes a summary call with no room.
- `StartIfNeeded` sees an overflow only while the overflowed turn is the last
  and has ended; a turn the host closed reads as canceled, and the size
  estimate decides.

## Rejected

- **Keep the old history when the summary fails** (the previous version, with
  `ErrHistoryTooLong`; sessions today): one failed model call leaves a
  conversation that cannot be sent.
- **Record the fallback after the failure** (a step Drive or the host
  writes): a second write that a crash can skip, needing its own recovery;
  the Start already holds everything.
- **Retry the summary with a cheaper model:** costs money and can fail too.
- **Compaction inside the work turn** (extend `Agent.ActiveCompaction` to
  summarize `Start.History`): `Continue` must get the history the turn
  started with, and every turn becomes a possible history boundary.
- **A Start whose history is built when it first runs:** an engine change to
  save one wait.
- **A Dashi compaction runner or conversation store:** Dashi does not own
  turn sequences or workers ([advancing turns](advancing-turns.md)).
- **Leaving a failed compaction turn for the host to close** (the first
  version): every host copied a "held for budget" check and a `Cancel`, to
  keep a retry that the free fallback makes pointless. The engine now ends
  it.
