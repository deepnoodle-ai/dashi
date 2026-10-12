# Durable summarization

**Status:** Proposed · **Date:** 2026-10-12 · **Follows:**
[advancing turns](advancing-turns.md) item 7, which deferred this until
`Record`, `Advance`, and `Drive` shipped (v0.7.0).

## Problem

A host that saves turns in a database and runs them in workers wants what a
`session.Session` gets from `Session.Compactor`: before the next turn of a
conversation, if the history is too long, summarize the older part first. The
summary must be paid for like any model call, survive a crash at any point,
never run twice, and never send a half-built history. If it fails, the next
turn runs with the old history, unless the old history is what overflowed.

Dashi already has the pieces. `Compactor.Start` builds a compaction turn,
`Runner.Record` saves it, `Drive` runs it, and `BuildHistory` uses it once it
completes with a summary and ignores it otherwise. What a host lacks is the
answer to two questions it must not get wrong: *is a compaction due after
these turns?* and *how do two attempts agree they are the same compaction?*

## What hosts do today

Our first durable host answers both with its own state machine: about 330
lines in its compaction adapter, a `model_compactions` table, and glue in
admission and the conductor.

| Host piece | Lines | Why it exists |
| --- | --- | --- |
| Operation row: plan, bind, activate, fail (repository) | ~280 | identity and state of one compaction |
| Frozen request, history, policy, usage, and digest | ~45 | rebuild the same summary after a crash |
| `executeCompaction`, `activationRecord` | ~80 | "activate" a summary that completed |
| Activated-only filter and decision bookkeeping in `history` | ~60 | last ordinary turn, forced after overflow, two-turn minimum |
| Admission replay of the frozen request, two bind calls | ~40 | lost acknowledgment after activation |
| Fail planned operations on stop; busy check in admission | ~30 | the operation row gates the conversation |
| Working-window policy (`workingWindow`, `newCompactor`) | ~25 | compact at 128k on a 1M-token model |

Most of it restates facts the compaction turn's record already holds. The
saved Start *is* the frozen plan: its `Input` holds the transcript, and its
`CompactionFact` holds what is kept and the `Hash` of what is replaced.
"Activated" is "completed with a summary", which is the rule `BuildHistory`
already applies. "Failed" is "ended without one". The conversation's own
turn slot already keeps other work out while the compaction turn runs.

## Proposal

**Idea 1: a compaction turn is named by the turn it follows.** Its command
ID is `"compact:" + lastTurnID`. A retry, a restarted worker, or a second
worker derives the same ID from the same record, so the host saves it once
(its usual duplicate-admission check) and the recorder deduplicates the rest.
One position gets one attempt: a failed summary is not retried until a later
turn exists, which is the rule `session` already follows. The host needs no
operation row, no digest, and no activation step.

**Idea 2: one rule for "is a compaction due".** Session has it; the host
re-derived it, slightly differently. Package `compaction` exports it:

```go
// StartIfNeeded returns the Start of the compaction turn to run before the
// turn that follows turns (oldest first, as BuildHistory takes them), and
// true; or false when none is due. One is due when the last turn has ended,
// is not a compaction turn, and either stopped because its history no
// longer fit (session.NeedsCompaction) or Needed(BuildHistory(turns),
// LastUsage(last)) is true. The Start's CommandID is "compact:" and the last
// turn's ID. Errors are those of Start, including ErrNothingToCompact.
func (c *Compactor) StartIfNeeded(ctx context.Context, turns []dashi.Turn) (
	start dashi.Start, needed bool, err error)
```

It makes no model call and writes nothing. It lives in `compaction`, not the
root: the Turn engine does not need it.

**Also: a working-window cap.** `Policy.MaxWindow int`: compaction is due at
`Fraction` of the smaller of `MaxWindow` and the model's window, and the kept
tail is sized from it too; the summary request still gets the model's full
window, so a long history can always be summarized. Zero means no cap; when
the model's window is unknown, `MaxWindow` replaces `DefaultWindow`, as
`ActiveCompactionPolicy.Window` does. Every
call resends the history, so on a 1M-token model an uncapped history makes
each reply wait on prefilling it. The host's 25 lines become one field.

Docs: a "Durable hosts" section in the compaction guide and an
`ExampleCompactor_StartIfNeeded` that records, drives, and crashes midway.

## Host code

**Before** (admission of the next work turn, condensed):

```go
ops := repo.ModelCompactions(conv)            // planned/bound/active/failed rows
history := buildHistory(onlyActivated(turns, ops))
if op := pending(ops); op != nil { return executeCompaction(op) }
force := lastOrdinary(turns).Stop.Limit.Kind == dashi.LimitContextWindow
if !force && (ordinary < 2 || !compactor.Needed(history, last)) { return history }
op := plan(key, freeze(req, history, last, policy), digest)  // new row
start := newCompactor(thaw(op.Policy)).Start(ctx, thaw(op.History))
turn := admit(compactionRequest(req, start)); repo.BindCompactionTurn(op, turn)
view := watch(turn)                           // cancel if it stops without ending
if completed(view) { repo.ActivateCompaction(op, activationRecord(view)) }
else { repo.FailCompaction(op, reason) }      // forced: permanent error
```

**After:**

```go
turns := endedTurns(conv) // oldest first; busy while one has not ended
start, due, err := compactor.StartIfNeeded(ctx, turns)
if err != nil && !errors.Is(err, compaction.ErrNothingToCompact) {
	return nil, err
}
if due {
	turn := admit(ctx, conv, start) // key start.CommandID; Runner.Record; worker Drives
	view := waitEnded(ctx, turn)    // FollowNext; put Cancel at a stop that is not an end
	if _, ok := view.ReplacementHistory(); !ok && session.NeedsCompaction(lastView) {
		return nil, errContextFull // optional host policy: old history cannot fit
	}
	turns = append(turns, loaded(turn))
}
history, _ := dashi.BuildHistory(turns)
```

## What the host deletes

About 520 hand-written lines: ~200 of the adapter's 330 (all of
`executeCompaction`, `activationRecord`, `compactionDigest`, the key prefixes,
the policy helpers, most of `history`), ~40 in admission, ~20 in the
conductor, the 252-line repository file, its 31-line schema, and the
104-line repository contract test. Also the `model_compactions` table, its
generated Ent code, and the `CompactionID` request field. Four adapter tests
shrink; the "activation acknowledgment lost" test has nothing left to test.
Most of this needs only v0.7.0 and Idea 1; the API saves the ~60 lines of
decision and policy code and keeps the host and `session` on one rule.

## Rejected

- **Compaction inside the work turn** (extend `Agent.ActiveCompaction` to
  summarize `Start.History`): no second turn to wait for, but it breaks the
  rule that `Continue` gets the history the turn started with, and makes
  every turn a possible history boundary for `BuildHistory`.
- **A Start whose history is built when it first runs**, so the host can save
  the work turn before the summary ends: an engine change to save one wait.
- **A Dashi compaction runner or conversation store:** Dashi does not own
  turn sequences or workers ([advancing turns](advancing-turns.md)).
- **Ending a failed compaction turn automatically:** it would delete a few
  host lines but stop `Drive` from leaving retries to the host.

## Open questions

1. **Default cap.** Should `MaxWindow` default to 128,000 instead of zero,
   so sessions and the CLI also stop growing a 1M-token history? It changes
   behavior for existing users of 1M-token models.
2. **After a failed or limited turn.** Session compacts only after a turn
   that completed (or overflowed); `StartIfNeeded` checks after any ended
   turn, because a host that ends turns at limits (`NoRaise`) still grows
   history. Should `session` adopt the same rule so there is one?
