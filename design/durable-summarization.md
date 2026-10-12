# Durable summarization

**Status:** Proposed · **Date:** 2026-10-12 · **Follows:**
[advancing turns](advancing-turns.md) item 7, which deferred this until
`Record`, `Advance`, and `Drive` shipped (v0.7.0).

## Problem

A host that saves turns in a database and runs them in workers wants what a
`session.Session` gets from `Session.Compactor`: before the next turn of a
conversation, if the history is too long, summarize the older part first. The
summary must be paid for like any model call, survive a crash at any point,
and run once. If it fails, the next turn runs with the old history, unless
the old history is what overflowed.

Dashi has the pieces. `Compactor.Start` builds a compaction turn,
`Runner.Record` saves it, `Drive` runs it, and `BuildHistory` uses it once it
completes with a summary and ignores it otherwise. A host still has to answer
two questions it must not get wrong: *is a compaction due after these turns?*
and *how do two attempts agree they are the same compaction?*

## What hosts do today

Our first durable host answers both with its own state machine: an operation
row that freezes the work request, the history, the policy, and the last
usage under a digest; plan, bind, activate, and fail transitions; a filter
that counts only "activated" compaction turns; and admission code that
replays the frozen request after a lost acknowledgment.

| Host code | Lines today | After |
| --- | --- | --- |
| Compaction adapter | 369 | ~120 |
| Admission: frozen-request replay, two bind calls | ~40 | 0 |
| Conductor: fail planned operations on stop | ~20 | 0 |
| Repository: operation file, type, busy check, schema | ~310 | 0 |
| Repository contract test for operations | 104 | 0 |

Most of it restates facts the compaction turn's record already holds. The
saved Start *is* the frozen plan: its `Input` holds the transcript, and its
`CompactionFact` holds what is kept and the `Hash` of what is replaced.
"Activated" is "completed with a summary", the rule `BuildHistory` already
applies; "failed" is "ended without one". The conversation's turn slot
already keeps other work out while the compaction turn runs.

## Proposal

**Idea 1: a compaction turn is named by the turn it follows.** Its command
ID is `"compact:" + lastTurnID`, and the host admits it under that key alone,
not a digest of the work request that asked for it. A retry, a restarted
worker, or a second worker for other work at the same position finds the same
turn; the first admitter's account pays. One position gets one attempt, as in
`session`: a failed summary is not retried until a later turn exists. The
host needs no operation row, no digest, and no activation step.

**Idea 2: one rule, read from the record.** `compaction` exports it, and
`session.planCompaction` changes to the same rule (a shared test runs both
over the same turns):

```go
// StartIfNeeded returns the Start of the compaction turn to run before the
// turn that follows turns (oldest first, as BuildHistory takes them).
//   - The last turn has not ended, or is a compaction turn that completed:
//     not needed.
//   - The last turn is a compaction turn that ended without a summary, and
//     the turn before it overflowed (session.NeedsCompaction): ErrHistoryTooLong,
//     since the old history cannot be sent again. Otherwise not needed.
//   - The last turn overflowed: needed, whatever its usage.
//   - Otherwise: needed when Needed(BuildHistory(turns), LastUsage(last)).
// The Start's CommandID is "compact:" and the last turn's ID. It makes no
// model call and writes nothing. Start's errors pass through, including
// ErrNothingToCompact.
func (c *Compactor) StartIfNeeded(ctx context.Context, turns []dashi.Turn) (
	start dashi.Start, needed bool, err error)
```

Because the refusal after a failed forced summary is read from the record, a
crash between the failure and the next admission cannot loop the overflow.
Session's "only after a completed turn" becomes "after any ended turn": a
host that ends turns at limits (`Limits.NoRaise`) still grows history. The
host's "at least two ordinary turns" minimum is dropped on purpose;
`ErrNothingToCompact` already covers a history with nothing old enough.

**Also: a working-window cap.** `Policy.MaxWindow int`: compaction is due at
`Fraction` of the smaller of `MaxWindow` and the model's window, and the kept
tail is sized from it; the summary request still gets the model's full
window, so a long history can always be summarized. When the model's window
is unknown, `MaxWindow` replaces `DefaultWindow`. Every call resends the
history, so on a 1M-token model an uncapped history makes each reply wait on
prefilling it. The host's ~30 lines of fraction arithmetic become one field.

Docs: a "Durable hosts" section in the compaction guide and an
`ExampleCompactor_StartIfNeeded` that records, drives, and crashes midway.

## Host code

**Before** (admission of the next work turn, condensed): load operation rows;
resume any planned one; filter turns to activated compaction turns; find the
last ordinary turn and whether it overflowed; count ordinary turns; plan a
row with frozen JSON and a digest; build the Start from the thawed history
and policy; admit, bind, watch; activate or fail the row.

**After** (all of it runs at admission, so a restart repeats it):

```go
turns := conversationTurns(conv) // oldest first
if last := lastOf(turns); isCompaction(last) && stoppedFailed(last) {
	inbox.Put(ctx, last.ID, dashi.Cancel{CommandID: "close:" + last.ID})
	return errBusy // retry once it has ended
}
if !ended(lastOf(turns)) {
	return errBusy // includes a summary held for budget: it waits like any turn
}
start, needed, err := compactor.StartIfNeeded(ctx, turns)
switch {
case errors.Is(err, compaction.ErrHistoryTooLong):
	return errConversationFull // host policy: tell the operator
case err != nil && !errors.Is(err, compaction.ErrNothingToCompact):
	return err
case needed:
	admit(ctx, conv, start, start.CommandID) // Runner.Record; a worker Drives it
	waitStopped(ctx, start.CommandID)        // FollowNext
	return retry()                           // run these steps again
}
history, _ := dashi.BuildHistory(turns)
// ... admit the work turn with Start.History = history ...
```

Only a failure stop is closed; a budget hold keeps the summary, as today. The
work request is not frozen: until the work turn is admitted nothing of it is
recorded, and the host rebuilds it for the same wake, so a retry that picks
up a newer input is still correct.

## What the host deletes

About 720 of ~840 lines (table above), ~620 of them outside tests, plus the
operation table, its generated Ent code, and the `CompactionID` request
field. Four adapter tests shrink; "activation acknowledgment lost" has nothing
left to test. Idea 1 with v0.7.0 does most of it; `StartIfNeeded` and
`MaxWindow` remove the ~70 lines of decision and window code and keep the
host and `session` on one rule.

Known limit: the key has no model. If the conversation's model changes, the
compaction turn already at that position stands; the next one uses the new
model.

## Rejected

- **Compaction inside the work turn** (extend `Agent.ActiveCompaction` to
  summarize `Start.History`): no second turn, but `Continue` must get the
  history the turn started with, and every turn would become a possible
  history boundary for `BuildHistory`.
- **A Start whose history is built when it first runs**, so the work turn can
  be saved before the summary ends: an engine change to save one wait.
- **A Dashi compaction runner or conversation store:** Dashi does not own
  turn sequences or workers ([advancing turns](advancing-turns.md)).
- **Ending a failed compaction turn automatically:** saves the host three
  lines but stops `Drive` from leaving retries to the host.

## Open question

**Default cap.** Should `MaxWindow` default to 128,000, with a negative value
turning the cap off? Sessions and the CLI would then also stop growing a
1M-token history. The reviewer recommends it; Dashi has no users, so the
behavior change costs nothing, but it is a product choice about latency
versus remembered detail.
