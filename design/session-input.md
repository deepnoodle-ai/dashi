# Session input, streams, and compaction

**Status:** Proposed, 2026-09-29. Companion to [sessions.md](sessions.md); PRD
[item 14](roadmap-research/14-session-continuity-prd.md). Covers track 4
(steering, the in-memory queue, streams, and the `cmd/dashi` port, which needs
them) and track 5 (compaction turns).

## Two input modes

- **Steer** reaches the running turn at its next model call.
- **Enqueue** holds input for the next turn.

ACP's draft `session/inject` has the same `queue | steer` split, as do Codex
(`turn/steer` and its queue) and pi's harness. A `Start` never picks one of
these for the caller ([sessions.md](sessions.md#what-a-new-start-does)).

**Steering.** With `Session.Steering` nil, the handle owns an `*Inbox` and
passes it with `WithSteering` to every turn it runs, except compaction turns.
`s.Steer(ctx, text)` sends a user steer to it and returns `Queued` (below) with
the steer's ID and the `TurnID` it reached; `s.SteerItem(ctx, steer)` takes a
whole `Steer`, for an urgent one (the CLI's `!`) or a task notice. When the
handle runs no turn, the steer goes straight to the queue and `TurnID` is
empty. A turn the handle runs is one a `Start`, `Continue`, or queue turn of
this handle drives. When the handle runs a
compaction turn, `Steer` returns `*TurnBusyError{Origin: StartCompaction}`; the
caller can `Enqueue`. A host with its own durable steering source sets
`Session.Steering` and sends through its own system; `s.Steer` then returns an
error unless that source is an `*Inbox`. Missed-steer handling below applies
only to an `*Inbox`; a host source releases its own.

**Codex's steer guards.** The typed result is adopted: `Queued.TurnID` says
whether a steer reached a turn or was queued. An expected turn ID is not: the
handle's inbox reaches only the turn this handle runs, and a steer that misses
it is queued, not lost. An output-schema match is not: a steer carries only
context, never a schema.

## The in-memory queue

```go
func (s *Session) Enqueue(ctx context.Context, text string) (Queued, error)       // a user message
func (s *Session) EnqueueItem(ctx context.Context, item ContextItem) (Queued, error) // any item, e.g. a notice

type Queued struct {
    ID       string    // the item's ID
    TurnID   string    // Steer only: the running turn it reached; empty when queued
    Warnings []Warning // memory_only under a durable recorder
}
```

Queue items are `ContextItem`s, so each keeps its ID and origin. An empty ID is
generated and an empty origin means `ContextUser`, as for `Inbox.Send`; a
repeated ID (in the queue or the handle's inbox) is an error. An item's message
must use the user role, since it becomes `Start` input. Items leave the queue in
order. Reserving items for a turn runs under the handle's lock. `Withdraw` is
later, when a caller needs it.

**In memory only.** A server that builds a handle per request loses the queue
between requests. When the runner has a durable recorder (any host-set recorder
other than `*MemoryRecorder`), every item queued on the handle returns a
`memory_only` warning in `Queued.Warnings`, so a request that only enqueues
still sees it. This is the known exception to the handle rule. The v1 server
contract follows: the queue belongs to one handle; a turn another process runs
is steered through the host's own `Session.Steering` source; stopping it from
another process is later.

## Dispatch on idle

When a `Start` or `Continue` this handle runs ends its turn with `NextNone`
and the queue holds items, the same `Run` or `Stream` call starts the next
turn from them, and repeats until the queue is empty or a turn stops any other
way. `Resolve`, `AddMessages`, and `Cancel` run no model call, so nothing queued
runs after them. Every peer queue does
this (Codex's queue extension, pi's next design, LangGraph's `enqueue`, ACP's
inject); a queue that waits for the next send strands input. `RunQueue` does the
same for an idle session (last turn `NextNone`, or none) with no new input;
when it runs nothing it returns false and the last turn's outcome.

Done is not settled (pi #5886): the call returns when its own turns end, and the
outcome is the last one's. When that turn stopped with anything but `NextNone`,
items stay queued; `session.State.Queued` counts them. Background tasks belong to
`delegate.Tasks`, and `RunUntilQuiet` (below) is the call that waits for them.

A **queue turn** reserves every item under the lock and starts:

```go
Start{
    CommandID: "queue:" + jsonOf(sortedItemIDs), // a retry or a second driver is a duplicate
    Origin:    originOf(items),                 // StartUser if any item came from the user, else StartScheduled
    Input:     messagesOf(items),                // in queue order
}
```

The item IDs live in the command ID, as `delegate.FollowUp` records notices
today. The compaction check runs before a queue turn as before any turn.

A caller's **`Start` on an idle session** with items queued reserves the
user-origin items and puts their messages ahead of its `Input`, since the user
typed them first. This is the CLI's behavior: leftovers join the next prompt
(`cmd/dashi/control.go`). Items of other origins, such as delegate notices, stay
queued and run as a queue turn afterwards, so the `queue:` key keeps their IDs
in the record for the delivery rule below.

**Consuming items.** A reserved item keeps its ID and place in the queue, and no
other turn takes it, until the session knows whether this `Start`'s
`turn_started` step committed. A `turn_started` event the run reported is proof
that it did, since the engine emits a step only once it commits, so a `Stream`
consumer that breaks after it consumes the items. Only proof that it did not
restores the items, in order: an error after which `Run` promises nothing was
recorded (neither a `*CommitError` carrying a `Step` nor `ErrRecordInvalid`;
`recorded` in `runner.go`), or a `*CommitError` whose `Step.Kind` is
`turn_started` with `PersistenceRejected`, `ErrRevisionConflict`, or
`ErrCommandConflict`. Anything else is inconclusive: an unknown outcome for the
start, a failed later step (a rejected model step included), or
`ErrRecordInvalid`, which follows recorded steps. The session then loads `S-n`:
a `turn_started` step with this command ID consumes the items, a missing one
restores them, and a failed load keeps them reserved and returns the error. The
same rule covers user-origin items put ahead of a caller's `Start`, checked
against that `Start`'s command ID. Each later command tries to settle it the
same way, but only the queue waits on it: `Open` and new commands run without
the reserved items, each outcome warns `queue_blocked`, and `RunQueue` returns
the error. A retried queue turn reuses its deterministic `queue:` key, and the
session scans the idempotency window for it before starting one; a match means
the items were delivered, so it consumes them and starts nothing. A turn in the
window that cannot be read fails the scan, even an unreadable last turn under
`Exclusive`, since it may hold the key: `RunQueue` returns the error and the
items wait until a caller's `Start` moves past that turn.

**Missed steers move to the queue.** After a turn ends `NextNone`, and when a
new `Start` closes a stopped turn, the session calls `Inbox.Leftovers` on the
freshly loaded turn and appends each steer to the queue, in order, with its ID.
While a turn stays continuable, its missed steers wait in the inbox for its
`Continue`, as the CLI does. Some peers drop them: Codex loses undelivered
steers on interrupt, and pi stranded follow-ups on abort (#10017). A message the
user typed should still be answered, so Dashi keeps it, unlike a nudge that
expires with its turn.

## Delegation

A finished background task's notice is enqueued on the parent session, and its
follow-up is a queue turn. `delegate.FollowUp` and `delegate.FollowUpTurnID` are
removed. The delivery rule (a notice counts as delivered when the record holds
its ID in a turn's context or in a start's command ID) reads the `queue:` prefix
in place of the follow-up prefix. One `Tasks` serves one session.

```go
// RunUntilQuiet runs the session's queue and waits while tasks run. It
// returns when no task is pending and the queue is empty, with the outcome
// of the last turn it ran, or of the session's last turn when it ran none.
func RunUntilQuiet(ctx context.Context, s *session.Session, tasks *Tasks,
    opts ...dashi.RunOption) (dashi.RunResult, error)
```

It loops over `s.RunQueue` and `tasks.Wait`, which returns false after each
report and true when nothing is pending, and also returns when a turn stops
with anything but `NextNone`. The host's `OnTask` enqueues
`e.Task.Notice().Steer().Item`, or sends `e.Task.Notice().Steer()` with
`SteerItem` so a running turn reads it at once.

**Agent messages** ([spec](subagent-messages.md), decisions 42–44) extend this.
Each task runs in a child session. Mail to the parent uses the same two calls:
`SteerItem` for a steer, `EnqueueItem` for a queued message. Mail to a running
child is a `SteerItem`; mail to an idle or ended child starts its next turn
with a plain `Start` whose command ID is the message ID, which closes a
stopped turn as any `Start` does. With `delegate.Config.Session` set, `Tasks`
puts notices and messages on the parent session itself, and `OnTask` is for
display only. `Tasks.Wait` and `RunUntilQuiet` wake on messages as on
notices. `Session` gains no field.

## Streams and transcripts

**`Session.Stream`** yields the events of every turn the call runs, including a
closing step, a compaction turn, the caller's turn, and queue turns. Each event
carries its `TurnID`, and each turn ends with its own `Outcome` event; the last
is the call's result. Breaking the range ends the context, which records an
interruption (or a cancel under `ContextCancel`), and no further queue turn
starts. Queue and follow-up turns start with no new request, so without this
nobody would stream them.

**Following from another process** uses `dashi.Follow` on each turn ID in v1. A
session cursor (`SessionCursor`, `FollowSession`) that follows across turns is
later, when the ACP adapter or another caller needs it; Codex has none and pays
for it in resume cost (#38787).

**Transcripts.** `s.Turns(ctx)` yields every turn record in order, compaction
and closed turns included; an unreadable turn yields a `Turn` holding only its
ID, with its error, and the iteration goes on. For ACP replay, message IDs
derive from the turn ID and the message's index in the turn (`MessageID(turnID,
i)`, added with the ACP adapter), so they are stable across replays. A turn the
session closed has a canceled stop with reason `CloseReason` ("a new message
moved on"), so replay can mark it. `s.History` skips a turn it cannot read,
without a warning; `Turns` reports it.

## Compaction turns

When the projected history grows past the compactor's threshold, the session
runs a **compaction turn** in the next slot, before the caller's turn or a queue
turn:

- Origin `compaction`, a summarizer agent with `NoTools`, and command ID
  `"compact:" + TurnID(s, n)`, so two workers collide as duplicates: a revision
  or command conflict at any step of the compaction turn means another worker
  took the slot, and the loser plans again and uses that worker's turn, with no
  warning.
- Its one model call is the summary request, so spend and recovery work as for
  any turn, and the caller's `Start.Budget` caps it (before a queue turn there
  is none). Its `Input` is the summary prompt with a text transcript of the
  projected history, as package `compaction` builds today (item 15, decision 8);
  `History` is empty.
- Its output is the **replacement history**: the summary plus the kept messages,
  which need not be a contiguous tail. The package keeps system and developer
  messages from the covered part.

```go
const StartCompaction StartOrigin = "compaction"

// CompactionFact is recorded in the start fact of a compaction turn.
type CompactionFact struct {
    Through   int           `json:"through"`   // last turn covered
    Digest    string        `json:"digest"`    // fingerprint of the projected history replaced
    Keep      []llm.Message `json:"keep,omitempty"`
    SummaryAt int           `json:"summary_at"` // index in Keep where the summary goes
    Frame     string        `json:"frame"`      // text that introduces the summary
}

// Compactor decides when and how to compact. Package compaction implements it.
type Compactor interface {
    // Needed reports whether to compact before the next turn.
    Needed(projected []llm.Message, last llm.Usage) bool
    // Start builds the compaction turn's Start, with Compaction set.
    Start(projected []llm.Message) (Start, error)
}
```

The compactor fills `Keep`, `SummaryAt`, `Frame`, the agent, and `Input`; the
session sets `Through`, `Digest` (the history fingerprint of `projected`), the
command ID, the origin, an empty `History`, `NoTools`, and the caller's
`Budget`, and an empty model becomes the session's. `last` is the usage of the
last turn's final model call.

`Start` gains `Compaction *CompactionFact`, required with origin `compaction`
and allowed only with it (the engine checks both, and that `SummaryAt` is in
range), and `TurnView` gains `Compaction` (it gains `Origin` in track 2). The
replacement history is `Keep[:SummaryAt]`, then one user text message of
`Frame`, a blank line, and the summary, then `Keep[SummaryAt:]`. The summary is
the trimmed text inside the answer's last `<summary>` and `</summary>` tags, or
the whole trimmed answer when it has none. A compaction turn that did not
complete with a non-empty summary replaces nothing. It is a pure function of the
record, so a reader with no `Compactor` builds it. This is the one place a turn
records earlier messages (decision 39).

An earlier draft also had `Compactor.Replace(projected, out)`. It is dropped: a
completed turn accepts no further steps (`acceptsAfterEnd`), so a replacement
built after the turn could not be recorded, and a reader without a compactor
could not rebuild it. The kept messages are chosen before the call, so they are
recorded at `Start` instead.

**Projected history** for turn *m* is the replacement history of the newest
completed compaction turn *C* before *m*, followed by the projections of the
non-compaction turns *C+1…m−1*. A fresh handle loads turns back to *C* only.
Every reader honors a compaction turn, whatever its configuration, so a viewer,
a recovery worker, and an older build all send the same history. ADK-Go honors
compaction records only when compaction is enabled, since any writer of one can
rewrite what the model sees; that worry does not apply here, because only the
session writes compaction turns and the host authorizes compaction starts on the
engine as it does operator starts. The digest is checked only when the handle
already holds the covered turns (it ran them, or `History` loaded them), since
loading them just to check would undo the short load. Ended turns freeze, so a
mismatch means a bug: the session warns `compaction_mismatch` and still honors
the compaction turn, so every reader keeps the same history. The result is
cached, and the warning repeats on each outcome whose history the compaction
turn builds, as `turn_unreadable` does.

**The conversation history is untouched.** `s.History` never includes compaction
turns, and `s.Turns` does: the summary is a session event, not conversation. UIs
hide compaction turns by `TurnView.Origin` or show a marker.

**Failure.** If the compaction turn does not complete with a summary, the
session closes it (`close:` cancel), keeps the previous projected history, adds
`compaction_failed`, and starts the caller's turn. The session runs the
compaction check only when the last turn completed, is not a compaction turn,
and was not just closed by this `Start` (closing completes a turn that recorded
structured output), so it never compacts a broken tail (ADK-Go, Nvoken), and a
failing summary is tried at most once per completed turn, each time with a
warning. A compactor whose `Start` fails warns the same way and records no turn.
When the caller's context ends or a `Stream` consumer stops during a compaction
turn, the call returns; the next `Start` closes that turn with
`compaction_failed`.

**Package `compaction`.** `compaction.New(policy Policy) *Compactor` implements
the interface; `Compactor`'s zero value works, and its fields add `Model`,
`Prompt`, `MaxTokens`, and `TailFraction`. `Needed` is `Policy.Due(last)` when
the projected history holds a message other than system and developer ones;
`Start` chooses the cut, the kept messages, and the transcript input. With no
turn starts to go by, it cuts only where a user message that is not a tool
result starts, never splitting a tool call from its result. The kept tail is
sized by `Policy`'s window; the transcript by the smaller of that window and the
catalog window of `Model`, when set. It cannot check savings before the summary
exists. `Compactor` makes no model call. A host that keeps its own turns
builds history with `dashi.ProjectHistory`, the rule `Session` uses, and runs
`Compactor.Start` through its own `Runner`, so the summary call is recorded
and charged like any turn. The earlier `Compact`, `Project`, `Record`, and
`NewSummarizer` path, whose summary call bypassed the host's recorder, was
removed (2026-10-01).

**Precedent.** pi makes compaction an operation that goes through the same
admission as a prompt; Codex runs an unsteerable `Compact` task and stores
`replacement_history`; Eino v0.10 stores `MessagesReplaced`.

## Later: the durable queue

The first note after this design. Requirements from the design reviews:

- Take items for a turn atomically with committing its `Start`, and record the
  item IDs in the start fact; reconcile items taken but not recorded on open.
- Withdrawing a taken item fails with "already used"; cancel and consume never
  live in two stores that can disagree (Codex #46911, #43819).
- Callers supply item IDs on `Enqueue`, so a retried enqueue is not a duplicate;
  the item ID becomes the delivered message's ID, which ACP replay needs.
- A dispatcher starts the next turn when a session goes idle, including after a
  crash (Codex polls idle threads every ten seconds).
- Each item has a status the caller can query by ID.

Mid-turn compaction, which a long coding turn needs, is an engine concern with
its own note before the CLI relies on sessions for long turns.
