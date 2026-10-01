# Sessions: tech spec

**Status:** Proposed, 2026-09-29. **PRD:** [roadmap item
14](roadmap-research/14-session-continuity-prd.md). **Decisions:** [35 to
40](decisions.md). **Companions:** [session-engine.md](session-engine.md)
(tracks 1 and 2: `Next`, ended turns, `Agent` on `Start`) and
[session-input.md](session-input.md) (tracks 4 and 5: steering, the queue,
streams, compaction turns).

This document covers track 3, the session core; the CLI port is track 4. Terms
(session, session handle, conversation history, projected history) are in the
[glossary](ubiquitous-language.md).

## Layers

`Runner.Run(ctx, turn, command)` stays the public engine for servers that
schedule their own work, recovery workers, and tests. `session.Session` sits above
it and uses only the public engine API: `Run`, `Stream`, `Replay`, `Project`,
`Loader`, `StepReader`. It is `http.Get` over `Client.Do`: two levels of one
design. `Session` lives in package `session`, which imports `dashi` and never
the reverse; `compaction` implements its `Compactor` interface, so the session
imports no compactor.

## The type

```go
type Session struct {
    // ID names the session. Empty means a new session with a generated ID
    // (NewID), readable from ID after the first Start.
    ID string
    // New creates the session: the first Start makes turn 1, and a session
    // that already has turns fails with ErrExists.
    New bool
    // Runner supplies services. A nil Recorder becomes one in-memory
    // recorder that the handle keeps across calls.
    Runner Runner
    // Agent overrides fields of the carried agent for new turns.
    Agent Agent
    // Steering reaches running turns. Nil means the handle's own *Inbox.
    Steering Steering
    // Compactor, when set, runs a compaction turn before a new turn.
    Compactor Compactor
    // Exclusive promises that this handle is the only driver of the session
    // and that earlier drivers have exited. It proves nothing about effects
    // they started.
    Exclusive bool
    // unexported: mutex, cached turns and views, in-memory recorder, inbox,
    // queue, and the IDs of turns this handle is running.
}

func (s *Session) Run(ctx context.Context, c Command, opts ...RunOption) (Outcome, error)
func (s *Session) Stream(ctx context.Context, c Command, opts ...RunOption) iter.Seq2[Event, error]
func (s *Session) Send(ctx context.Context, text string, opts ...RunOption) (Outcome, error)
func (s *Session) Answer(ctx context.Context, answers []Resolve, opts ...RunOption) (Outcome, error)
func (s *Session) Steer(ctx context.Context, text string) (Queued, error)
func (s *Session) SteerItem(ctx context.Context, steer Steer) (Queued, error)
func (s *Session) Enqueue(ctx context.Context, text string) (Queued, error)
func (s *Session) EnqueueItem(ctx context.Context, item ContextItem) (Queued, error)
func (s *Session) RunQueue(ctx context.Context, opts ...RunOption) (Outcome, bool, error)
func (s *Session) Open(ctx context.Context) (State, error)
func (s *Session) History(ctx context.Context) ([]llm.Message, error)          // conversation history
func (s *Session) ProjectedHistory(ctx context.Context) ([]llm.Message, error) // what the next turn sends
func (s *Session) Turns(ctx context.Context) iter.Seq2[Turn, error]

// Say returns a Start holding one user text message.
func Say(text string) Start
```

**Routing.** `Run` mirrors the engine without the turn argument. A `Start` opens
the next turn after the idempotency check and the last-turn rules below. Any
other command (`Continue`, `Resolve`, `AddMessages`, `Cancel`) goes to the last turn.
The session fills `Start.History` and `Continue.History` with the projected
history of the turns before the target turn, the turn ID, and, on a `Start`, the
merged agent ([session-engine.md](session-engine.md#carry-forward)). A command
on a session with no turns returns `session.ErrNotFound`.

**`Send`** is exactly `Run(ctx, Say(text), opts...)`. It must stay a one-line
wrapper; drifting entry points made one peer's streaming path ignore its session
(maf#3848).

**`Answer`** runs `Resolve` for each answer, then one `Continue`. The user makes
one call; the record has separate steps, so decision 18 holds. It stops at the
first `Resolve` error. A `Resolve` without a command ID gets `settle:` and its
`CallID`, as `Pending`'s builders derive theirs from the call, and the
`Continue`'s command ID is `answer:` and the settles' IDs, so a retried `Answer`
is recognized. It takes `RunOption`s, as `Run` does,
and passes them to each run, so a leasing host gives `WithOwner` and a live UI
`WithObserver` the one way it gives them elsewhere.

**`RunQueue`** starts the next turn from queued input when the session is idle,
and reports false, with the last turn's outcome, when it ran nothing;
`delegate.RunUntilQuiet` needs it.

**Handle lock.** One mutex guards the cache, the inbox claims, the queue, and
the running set. `Run` and `Stream` hold it only while choosing the turn and
building the command, which includes closing the last turn, not while the turn
runs. A second `Start` on the same handle while a turn runs sees that turn as
live and is refused.

## Storage and turn IDs

The session keeps no record of its own. Turn *n* of session `S` has ID `S-n`,
the separator the CLI already uses. Session IDs may contain `-`, so the number
is parsed from the right. Titles, owners, and other metadata stay in the
application; the CLI's `session.json` is the model.

```go
// TurnID returns the ID of turn n (from 1) of a session.
func TurnID(sessionID string, n int) string

// Index is an optional Recorder capability for sessions.
type Index interface {
    // LastTurn returns the highest turn number recorded for the session,
    // or 0 when it has none.
    LastTurn(ctx context.Context, sessionID string) (int, error)
}
```

**Probing.** Without `session.Index`, the session finds the last turn by doubling
the distance *d* (1, 2, 4, …) past the last turn it knows exists (0 at first)
until a turn is missing, then binary-searching. A probe reads one step with
`StepsAfter(id, 0, 1)` on a `StepReader`, or calls `Load`, where
`ErrTurnNotFound` or an empty turn means missing; a step of another turn is an
error, and the search stops at 2^30. A 1,000-turn session takes about 20 reads
from a fresh handle, and a handle that already knows its last turn takes one. A
recorder that is neither cannot reopen a session; the handle then knows only
the turns it ran.

Probing needs turns numbered from 1 without gaps. The session never skips a
number: a `Start` always targets `last+1`. A host that deletes turns (later)
must implement `session.Index`. `filerecorder` implements it by which turn logs
exist, the same search over file names with no side index, so a damaged log
is found without being read and is then skipped as unreadable.

**Cache.** The handle caches loaded turns and their views. A turn with a
terminal stop never changes its projection ([ended
turns](session-engine.md#ended-turns-project-and-freeze)), so its cached
projection is final. The handle reloads the last turn before each command.

**Unreadable turns.** Building projected or conversation history skips a turn
that fails to load or replay, and adds a `turn_unreadable` warning naming it, as
the CLI does today. An unreadable *last* turn is an error, since its liveness is
unknown, except under `Exclusive`, where it has no worker: a new `Start` leaves
it out of the history with `turn_unreadable` and starts `S-(n+1)`. A keyed
`Start` and a queue turn still fail, since the idempotency check cannot rule
out that the turn holds their key.

## Concurrency and liveness

No session lease or lock. Three engine rules keep the record correct:

- A step commits only at the expected sequence of its turn, else
  `ErrRevisionConflict`; `WithOwner` fences a stale lease holder.
- Two workers starting turn *n* collide on `S-n`. One wins; the other reloads. A
  worker with a stale view collides the same way.
- A new turn starts only after the last one has ended, and ending is permanent:
  a later command on it gets `ErrTurnEnded`.

On `ErrRevisionConflict` or `ErrCommandConflict` from a `Start`, before its
`turn_started` step committed, the session reloads the last turn and applies
the idempotency check and the last-turn rules once more, then returns the error
if it still loses. A conflict on a later step means the turn started and
another worker took it over, so it is returned as is.

These rules do not say whether a turn is **live**, meaning a worker drives it
now. A turn with no stop recorded after its latest `Start` or `Continue` is
either running or crashed.

```go
type Liveness string // whether any worker may still act on a turn

const (
    LivenessUnknown Liveness = "unknown" // no proof, e.g. an expired lease
    LivenessLive    Liveness = "live"    // a worker holds an unexpired claim
    LivenessStopped Liveness = "stopped" // the host knows no worker still acts
)

// TurnLiveness is an optional Recorder capability for hosts that lease turns.
type TurnLiveness interface {
    Liveness(ctx context.Context, turnID string) (Liveness, error)
}
```

**Stopped is proof, not a timeout.** An expired claim shows only that a worker
stopped renewing it: the worker may still be inside a provider call or a tool,
and `WithOwner` fences its commits, not that effect. A host reports stopped
only when it knows no worker drives the turn (its process exited, or host
fencing guarantees it); an expired lease, an error, and a missing capability
are unknown. The session refines `NextRecover` to `NextRunning` when this handle
runs the turn or `Liveness` reports live.

**`Exclusive`** promises that no other process drives the session and that
earlier ones have exited, so no worker drives a turn this handle is not running.
The CLI sets it under its process lock; leasing servers implement
`TurnLiveness`.

**A stopped worker is not a stopped effect.** Liveness and `Exclusive` say only
whether a worker drives the turn; a tool's child process or a remote operation
can outlive its caller. So a last turn holding an **unsettled uncertain call**
(a started call, neither `ReadOnly` nor `Idempotent`, with no result, or a model
call that offered a hosted tool unsafe to repeat) is busy for a new message
whatever its liveness, even under `Exclusive`. The host releases it: `Resolve`
the call with the real result after checking the effect, or `Cancel{}` the turn.
A cancel records the host's acceptance of the risk, not a guarantee: the call
stays unsettled beside a `StopCanceled` step with the host's reason, projects as
"result unknown; the turn ended", and the model may repeat the effect.

**Taking over a lost worker.** A server recovers a turn whose lease expired
with `Continue{}` under `WithOwner`, never by closing it and starting another.
Resume repeats no effect: a started unsafe call becomes uncertain
(`tools_loop.go`, decision 11); safe calls may rerun; an unfinished model call
is repeated at a token cost only, unless it offered an unsafe hosted tool
(`runner.go`, `recoverModelIntent`). The old worker's late commits fail the
owner fence.

## What a new `Start` does

After the idempotency check, a `Start` reads the last turn's refined `Next()`.
The first matching row applies:

| Last turn | `Next()` | A new `Start` |
|---|---|---|
| None, or ended (completed, canceled, or a terminal limit) | `NextNone` | Starts |
| Cannot be read, under `Exclusive` | unknown | Skips it with `turn_unreadable`, starts |
| Cannot be read, without `Exclusive` | unknown | Returns the load error |
| Live, or running on this handle | `NextRunning` | Refuses, no `CallID` |
| Holds an unsettled uncertain call, and no worker is known to run it (stopped or `Exclusive` included) | any other | Refuses, with `CallID` |
| No stop recorded; stopped, or `Exclusive` | `NextRecover` | Closes, starts |
| No stop recorded; liveness unknown (an expired lease included) | `NextRecover` | Refuses |
| Waiting on approvals or results | `NextResolvePending` | Refuses |
| Interrupted, provider-pause limit, or answered and ready | `NextContinue` | Closes, starts |
| At a budget or model-call limit | `NextRaiseBudget`, `NextRaiseModelCalls` | Closes, starts |
| Refused, failed, or other limits | `NextFix` | Closes, starts |

A turn is **busy** when a `Start` refuses on it: live, possibly live, waiting on
the host, or holding an unsettled uncertain call. To add input to a running
turn, or hold it for the next one, callers use `Steer` and `Enqueue`. To replace
a running turn, they stop it (cancel its context or use `WithPause`), then send.

```go
var ErrTurnBusy = errors.New("dashi: session turn is busy")

type TurnBusyError struct {
    TurnID    string
    Next      Next
    Origin    StartOrigin // compaction: the session's own work
    Duplicate bool        // this Start is a retry of that turn
    CallID    string      // the unsettled uncertain call; empty while running
}

func (e *TurnBusyError) Unwrap() error { return ErrTurnBusy }
```

The message names the way out. For `NextRecover`: "dashi: turn S-7 may still be
running; if its worker is lost, take it over with Continue{} or discard it with
Cancel{}". For `NextResolvePending`: "answer its pending work or Cancel{} it". With
`CallID`: "call c-3 in turn S-7 may still be running; Resolve{} it after checking
its effect, or Cancel{} the turn to accept the risk". The CLI asks: "the last
command may still be running: mark it done, or discard it?"

**Closing** sends `Cancel{CommandID: "close:" + turnID, Reason: CloseReason}`
(`CloseReason` is "a new message moved on"); the fixed command ID makes two
closers idempotent, and the new turn's outcome warns `turn_closed`. The engine
refuses `Cancel` once structured output is recorded (`runner.go`), so the
session closes such a turn, whatever its `Next`, with `Continue{CommandID:
"close:" + turnID}`, which completes it without a model call
(`stopAfterAnswer`). At a model-call limit, the reserved final answer, that
`Continue` raises `ModelCalls` by one, or it would stop limited again. A
close that leaves the turn open refuses with `*TurnBusyError`. Steers the
closed turn never recorded move to the queue
([session-input.md](session-input.md)). Closing relies on track 1: ended turns
project and freeze.

**Why refuse, when most chat peers accept input while busy?** `Session.Run` is
also the server API, and a server must not guess whether the user meant steer,
queue, or replace. A chat UI calls `Steer` or `Enqueue`, as the CLI does. Why
close other stopped turns? The engine treats few stops as terminal (`state.go`,
`terminal`), so refusing on the rest would block a zero-value session after an
Esc, a refusal, or the default eight model calls.

## Idempotency

`Start.CommandID` is the session's deduplication key. The check runs **before**
the last-turn rules, because agent turns are long and client timeouts short: the
usual retry arrives while its turn still runs.

1. No key: the session generates one (`NewID`), so each call is a new message.
2. Scan from the newest turn started by a caller's `Start` through the last
   turn. Queue and compaction turns (`queue:` and `compact:` keys) never start
   the window, whatever their origin, so it covers those that ran after the
   caller's turn. The key marks them because a queue turn's origin can be
   `StartUser`; `Run` refuses a caller's `Start` with either prefix. A turn in the window that fails to load fails the check with
   its error, since a missed key would run the command again.
3. A turn matches when its recorded command ID equals the key. It is a duplicate
   when its input matches too: for a caller's `Start`, the recorded
   `StartFact.Input` ends with the retry's `Input` by JSON meaning (queued user
   items the session added come first); for a queue turn, the key itself is
   derived from the item IDs. History and agent are not compared. Same key,
   different input: `ErrCommandConflict`.
4. A duplicate whose turn has stopped returns the turn's current outcome, as the
   first call returned it: ended (`NextNone`), or stopped without ending
   (waiting on an answer, at a limit, needing a decision), since waiting would
   not change it.
5. A duplicate whose turn may still be running (`NextRunning`, `NextRecover`)
   returns `*TurnBusyError{Duplicate: true}` naming it. `Stream` instead follows
   a `NextRunning` turn with `Follow` until its next stop, when the recorder is
   a `StepReader` and a `Loader`; a `NextRecover` turn may have lost its worker,
   and following it could wait forever, so it refuses.
6. Two workers racing to `S-(n+1)` with the same key: with different histories,
   the engine's command digest covers `History` and the agent, so the loser gets
   `ErrCommandConflict`; with the same, the engine takes the loser as a retry
   and returns the winner's outcome. Either way the session reloads and applies
   step 3, so a true retry is recognized and a loser whose turn has no recorded
   stop gets step 5, never a success for a turn still running.

A host that needs a longer window deduplicates at admission. A later optional
`session.Index` method can find a turn by `CommandID`.

## Opening and creating

`Open` (or the first use of a handle with an ID) reads the last turn and returns
its state. It never runs anything:

```go
type State struct {
    Turns  int       // number of the last turn; 0 for a new session
    Last   *TurnView // nil when Turns is 0
    Next   Next      // refined with liveness
    Queued int       // items waiting in this handle's queue
}
```

An ID with no turns is `session.ErrNotFound`, unless `New` is set. With `New`, a
session that already has turns fails with `session.ErrExists`, except for an
idempotent retry of the first `Start`, which the check above finds first. `New`
stops applying once the handle starts turn 1 or finds turns without `New`, so
the handle that creates a session keeps using it. An empty ID is always new.
Creating is explicit, so a mistyped ID fails rather than start an empty
session, as Dive's `Store.Open` did.

With no `Runner.Recorder`, the handle creates one `MemoryRecorder`, as the
zero-value rule requires, and the first outcome carries a `memory_only` warning.

A `Session` refuses provider-side conversation state, as the OpenAI SDK does,
once roadmap item 2 adds such a setting to `llm.Settings`.

## Errors and warnings

Session warnings are not in any turn record, so the ones a turn run produces
(`turn_closed`, `turn_unreadable`, `queue_blocked`, `compaction_failed`,
`compaction_mismatch`, and `memory_only` for the in-memory recorder) go on a new
field, `Outcome.SessionWarnings []Warning`. `Steer`, `Enqueue`, and `EnqueueItem`
return theirs in `Queued.Warnings`.

| Name | Kind | When |
|---|---|---|
| `ErrTurnBusy`, `*TurnBusyError` | error | The last turn is live, possibly live, waits on the host, or holds an unsettled uncertain call; `Duplicate` for a retry of a turn that may still be running |
| `session.ErrNotFound` | error | An ID with no turns and no `New` |
| `session.ErrExists` | error | `New` on a session with turns |
| `ErrCommandConflict` | error (engine) | Same key, different input |
| `turn_closed` | warning | A new `Start` closed the last turn |
| `memory_only` | warning | In-memory recorder; or input queued under a durable recorder |
| `turn_unreadable` | warning | A turn was skipped while building history, or an unreadable last turn under `Exclusive` |
| `queue_blocked` | warning | A reservation cannot be settled, so the queue waits ([session-input.md](session-input.md)) |
| `compaction_failed`, `compaction_mismatch` | warning | See [session-input.md](session-input.md#compaction-turns) |
| `tool_not_offered` | recorded warning | See [session-engine.md](session-engine.md#tool-selection) |

## Conformance checks

`dashitest` gains checks for any recorder that implements `session.Index` (it
agrees with the turns committed, including after a race on `S-n`) and a probing
check (a session of 1, 2, and 3 turns is found with at most `2·log2(n)+2` reads,
plus one to load the last turn, through `StepReader` and through `Loader` alone,
and session IDs with dashes find their own turns beside IDs that share a
prefix). The read bound is a property of the search, so a root test checks it
at 64 and 1,000 turns on `MemoryRecorder`. Both are part of `dashitest.Run`, so
`filerecorder` and `MemoryRecorder` run both; `MemoryRecorder` passes by
probing, and `filerecorder` and a test store check the index. Backends that
skipped a shared suite have diverged elsewhere (ADK-Go #679, #1329).

A lost-worker check expires the old claim during a model call, then during a
non-idempotent tool call: a new message refuses, takeover never reruns the tool,
and the late commit is rejected. An effect check exits the CLI while its tool's
child process runs on: a new `Exclusive` handle's `Start` refuses, naming the
call; `Cancel{}` then `Start` succeeds, and the record shows the cancel.
