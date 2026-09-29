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
schedule their own work, recovery workers, and tests. `dashi.Session` sits above
it and uses only the public engine API: `Run`, `Stream`, `Replay`, `Project`,
`Loader`, `StepReader`. It is `http.Get` over `Client.Do`: two levels of one
design. `Session` lives in package `dashi`; `compaction` imports `dashi`, so the
session takes a `Compactor` interface.

## The type

```go
type Session struct {
    // ID names the session. Empty means a new session with a generated ID
    // (NewID), readable from ID after the first Start.
    ID string
    // New creates the session: the first Start makes turn 1, and a session
    // that already has turns fails with ErrSessionExists.
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
    // and that earlier drivers have exited, so a turn it is not running is
    // stopped.
    Exclusive bool
    // unexported: mutex, cached turns and views, in-memory recorder, inbox,
    // queue, and the IDs of turns this handle is running.
}

func (s *Session) Run(ctx context.Context, c Command, opts ...RunOption) (Outcome, error)
func (s *Session) Stream(ctx context.Context, c Command, opts ...RunOption) iter.Seq2[Event, error]
func (s *Session) Send(ctx context.Context, text string, opts ...RunOption) (Outcome, error)
func (s *Session) Answer(ctx context.Context, answers ...Settle) (Outcome, error)
func (s *Session) Steer(ctx context.Context, text string) (Queued, error)
func (s *Session) Enqueue(ctx context.Context, text string) (Queued, error)
func (s *Session) EnqueueItem(ctx context.Context, item ContextItem) (Queued, error)
func (s *Session) RunQueue(ctx context.Context, opts ...RunOption) (Outcome, bool, error)
func (s *Session) Open(ctx context.Context) (SessionState, error)
func (s *Session) History(ctx context.Context) ([]llm.Message, error)          // conversation history
func (s *Session) ProjectedHistory(ctx context.Context) ([]llm.Message, error) // what the next turn sends
func (s *Session) Turns(ctx context.Context) iter.Seq2[Turn, error]

// Say returns a Start holding one user text message.
func Say(text string) Start
```

**Routing.** `Run` mirrors the engine without the turn argument. A `Start` opens
the next turn after the idempotency check and the last-turn rules below. Any
other command (`Continue`, `Settle`, `Deliver`, `Cancel`) goes to the last turn.
The session fills `Start.History` and `Continue.History` with the projected
history of the turns before the target turn, the turn ID, and, on a `Start`, the
merged agent ([session-engine.md](session-engine.md#carry-forward)). A command
on a session with no turns returns `ErrSessionNotFound`.

**`Send`** is exactly `Run(ctx, Say(text), opts...)`. It must stay a one-line
wrapper; drifting entry points made one peer's streaming path ignore its session
(maf#3848).

**`Answer`** runs `Settle` for each answer, then one `Continue`. The user makes
one call; the record has separate steps, so decision 18 holds. It stops at the
first `Settle` error.

**`RunQueue`** starts the next turn from queued input when the session is idle,
and reports false when nothing was queued. No earlier draft had a public way to
run the queue without new input; `delegate.RunUntilQuiet` needs one.

**Handle lock.** One mutex guards the cache, the inbox claims, the queue, and
the running set. `Run` and `Stream` hold it only while choosing the turn and
building the command, not while the turn runs. A second `Start` on the same
handle while a turn runs sees that turn as live and is refused.

## Storage and turn IDs

The session keeps no record of its own. Turn *n* of session `S` has ID `S-n`,
the separator the CLI already uses. Session IDs may contain `-`, so the number
is parsed from the right. Titles, owners, and other metadata stay in the
application; the CLI's `session.json` is the model.

```go
// TurnID returns the ID of turn n (from 1) of a session.
func TurnID(sessionID string, n int) string

// SessionIndex is an optional Recorder capability for sessions.
type SessionIndex interface {
    // LastTurn returns the highest turn number recorded for the session,
    // or 0 when it has none.
    LastTurn(ctx context.Context, sessionID string) (int, error)
}
```

**Probing.** Without `SessionIndex`, the session finds the last turn by doubling
*n* (1, 2, 4, …) until a turn is missing, then binary-searching between the last
present and the first missing number. A probe reads one step: `StepsAfter(id, 0,
1)` when the recorder is a `StepReader`, and a turn exists when that returns a
step. A recorder that is only a `Loader` is probed with `Load`, and
`ErrTurnNotFound` or an empty turn means missing. A 1,000-turn session takes
about 20 reads. A recorder that is neither cannot reopen a session; the handle
then knows only the turns it ran.

Probing needs turns numbered from 1 without gaps. The session never skips a
number: a `Start` always targets `last+1`. A host that deletes turns (later)
must implement `SessionIndex`. `filerecorder` stores each turn at a hash of its
ID, cannot list turns, and probes; it needs no side index, which would be a
second record to keep in step.

**Cache.** The handle caches loaded turns and their views. A turn with a
terminal stop never changes its projection ([ended
turns](session-engine.md#ended-turns-project-and-freeze)), so its cached
projection is final. The handle reloads the last turn before each command.

**Unreadable turns.** Building projected or conversation history skips a turn
that fails to load or replay, and adds a `turn_unreadable` warning naming it, as
the CLI does today. An unreadable *last* turn is an error, since the session
cannot apply the rules below without it.

## Concurrency and liveness

No session lease or lock. Three engine rules keep the record correct:

- A step commits only at the expected sequence of its turn, else
  `ErrRevisionConflict`; `WithOwner` fences a stale lease holder.
- Two workers starting turn *n* collide on `S-n`. One wins; the other reloads. A
  worker with a stale view collides the same way.
- A new turn starts only after the last one has ended, and ending is permanent:
  a later command on it gets `ErrTurnEnded`.

On `ErrRevisionConflict` from a `Start`, the session reloads the last turn and
applies the idempotency check and the last-turn rules once more, then returns
the error if it still loses.

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
only when the worker is known quiescent (its process exited, or host fencing
guarantees no effect in flight); an expired lease, an error, and a missing
capability are unknown. The session refines `NextResume` to `NextRunning` when
this handle runs the turn or `Liveness` reports live.

**`Exclusive`** promises that no other process drives the session and that
earlier ones have exited, so a turn this handle is not running is stopped. The
CLI sets it under the process lock `cmd/dashi/session` holds today, a host
promise rather than a recorder capability. It covers workers, not what they
spawned: a tool's child process can outlive a crashed CLI, and the closed turn
projects that call as "result unknown". Leasing servers implement
`TurnLiveness`.

**Taking over a lost worker.** A server recovers a turn whose lease expired
with `Continue{}` under `WithOwner`, never by closing it and starting another.
Resume repeats no effect: a started call that is neither `ReadOnly` nor
`Idempotent` becomes uncertain (`tools_loop.go`) and is never rerun (decision
11); safe calls may rerun; an unfinished model call is settled and repeated at
a token cost only, unless it offered an unsafe provider-hosted tool, which
makes it uncertain too (`runner.go`, `recoverModelIntent`). The old worker's
in-flight effect may still complete, and its commits fail the owner fence, but
no second copy starts, and an uncertain call blocks new turns until the host
settles or cancels it.

## What a new `Start` does

After the idempotency check, a `Start` reads the last turn's refined `Next()`:

| Last turn | `Next()` | A new `Start` |
|---|---|---|
| None, or ended (completed, canceled, or a terminal limit) | `NextNone` | Starts |
| No stop recorded; live | `NextRunning` | Refuses |
| No stop recorded; stopped, or `Exclusive` | `NextResume` | Closes, starts |
| No stop recorded; liveness unknown (an expired lease included) | `NextResume` | Refuses |
| Waiting on approvals or results | `NextAnswer` | Refuses |
| Uncertain, not yet settled | `NextResolve` | Refuses, unless stopped or `Exclusive` |
| Interrupted, provider-pause limit, or answered and ready | `NextContinue` | Closes, starts |
| At a budget or model-call limit | `NextRaiseBudget`, `NextRaiseAttempts` | Closes, starts |
| Refused, failed, or other limits | `NextResolve` | Closes, starts |

A turn is **busy** when a `Start` refuses on it: live, possibly live, waiting on
the host, or holding an uncertain effect that another worker may still be
causing. Answer or settle it, or cancel it, first. To add input to a running
turn, or hold it for the next one, callers use `Steer` and `Enqueue`. To replace
a running turn, they stop it (cancel its context or use `WithPause`), then send.

```go
var ErrTurnBusy = errors.New("dashi: session turn is busy")

type TurnBusyError struct {
    TurnID    string
    Next      Next
    Origin    StartOrigin // compaction: the session's own work
    Duplicate bool        // this Start is a retry of that turn
}

func (e *TurnBusyError) Unwrap() error { return ErrTurnBusy }
```

The message names the way out. For `NextResume`: "dashi: turn S-7 may still be
running; if its worker is lost, take it over with Continue{} or discard it with
Cancel{}". For `NextAnswer`: "answer its pending work or Cancel{} it". For an
uncertain turn: "settle its uncertain call with Settle{} or Cancel{} it".

**Closing** sends `Cancel{CommandID: "close:" + turnID, Reason: CloseReason}`,
where `CloseReason` is "a new message moved on". The fixed command ID makes two
closers idempotent. The outcome of the new turn carries a `turn_closed` warning.
One case needs care: the engine refuses `Cancel` on a turn whose structured
output is recorded (`runner.go`, "turn cannot cancel"). A stopped turn holding
output maps to `NextContinue`. Whatever its `Next`, the session closes a turn
holding output with `Continue{CommandID: "close:" + turnID}`, which completes it
without a model call (`runner.go`, `stopAfterAnswer`). Steers the closed turn
never recorded move to the queue ([session-input.md](session-input.md)).
Closing relies on track 1: ended turns project and freeze.

**Why refuse, when most chat peers accept input while busy?** `Session.Run` is
also the server API, and a server must not guess whether the user meant steer,
queue, or replace. A chat UI gets accepting behavior by calling `Steer` or
`Enqueue`, as the CLI does: typed text during a turn is a steer. Why close other
stopped turns? The engine counts only completed, canceled, and three limit stops
as terminal (`state.go`, `terminal`), so refusing on the rest would block a
zero-value session after an Esc, a refusal, or the default eight model calls.

## Idempotency

`Start.CommandID` is the session's deduplication key. The check runs **before**
the last-turn rules, because agent turns are long and client timeouts short: the
usual retry arrives while its turn still runs.

1. No key: the session generates one (`NewID`), so each call is a new message.
2. Scan from the newest turn started by a caller's `Start` through the last
   turn. Queue and compaction turns (`queue:` and `compact:` keys) never start
   the window, whatever their origin, so it covers those that ran after the
   caller's turn.
3. A turn matches when its recorded command ID equals the key. It is a duplicate
   when its input matches too: for a caller's `Start`, the recorded
   `StartFact.Input` ends with the retry's `Input` by JSON meaning (queued user
   items the session added come first); for a queue turn, the key itself is
   derived from the item IDs. History and agent are not compared. Same key,
   different input: `ErrCommandConflict`.
4. A duplicate whose turn has ended (`NextNone`) returns its outcome.
5. A duplicate whose turn has not ended returns `*TurnBusyError{Duplicate:
   true}` naming it. `Stream` instead follows that turn with `Follow` when the
   recorder is a `StepReader`.
6. Two workers racing to `S-(n+1)` with the same key and different histories:
   the engine's command digest covers `History` and the agent, so the loser gets
   `ErrCommandConflict`. The session reloads and applies step 3, so a true retry
   is still recognized.

A host that needs a longer window deduplicates at admission. A later optional
`SessionIndex` method can find a turn by `CommandID`.

## Opening and creating

`Open` (or the first use of a handle with an ID) reads the last turn and returns
its state. It never runs anything:

```go
type SessionState struct {
    Turns  int       // number of the last turn; 0 for a new session
    Last   *TurnView // nil when Turns is 0
    Next   Next      // refined with liveness
    Queued int       // items waiting in this handle's queue
}
```

An ID with no turns is `ErrSessionNotFound`, unless `New` is set. With `New`, a
session that already has turns fails with `ErrSessionExists`, except for an
idempotent retry of the first `Start`, which the check above finds first. An
empty ID is always new. Creating is explicit so that a mistyped ID fails instead
of silently starting an empty session, as Dive's `Store.Open` did.

With no `Runner.Recorder`, the handle creates one `MemoryRecorder`, as the
zero-value rule requires, and the first outcome carries a `memory_only` warning.

A `Session` refuses provider-side conversation state, as the OpenAI SDK does.
Dashi has no such setting today (`llm.Settings`); the rule binds when roadmap
item 2 adds one.

## Errors and warnings

Session warnings are not in any turn record, so the ones a turn run produces
(`turn_closed`, `turn_unreadable`, `compaction_failed`, `compaction_mismatch`,
and `memory_only` for the in-memory recorder) go on a new field,
`Outcome.SessionWarnings []Warning`. `Steer`, `Enqueue`, and `EnqueueItem`
return theirs in `Queued.Warnings`.

| Name | Kind | When |
|---|---|---|
| `ErrTurnBusy`, `*TurnBusyError` | error | The last turn is live, possibly live, waits on the host, or holds an unsettled uncertain call; `Duplicate` for a retry of a turn that has not ended |
| `ErrSessionNotFound` | error | An ID with no turns and no `New` |
| `ErrSessionExists` | error | `New` on a session with turns |
| `ErrCommandConflict` | error (engine) | Same key, different input |
| `turn_closed` | warning | A new `Start` closed the last turn |
| `memory_only` | warning | In-memory recorder; or input queued under a durable recorder |
| `turn_unreadable` | warning | A turn was skipped while building history |
| `compaction_failed`, `compaction_mismatch` | warning | See [session-input.md](session-input.md#compaction-turns) |
| `tool_not_offered` | recorded warning | See [session-engine.md](session-engine.md#tool-selection) |

## Conformance checks

`dashitest` gains checks for any recorder that implements `SessionIndex` (it
agrees with the turns committed, including after a race on `S-n`) and a probing
check (a session of 1, 2, 3, 64, and 1,000 turns is found with at most
`2·log2(n)+2` reads). `filerecorder` and `MemoryRecorder` run both. Backends
that skipped a shared suite have diverged elsewhere (ADK-Go #679, #1329).

A lost-worker check expires the old claim during a model call, then during a
non-idempotent tool call: a new message refuses, takeover never reruns the tool,
the late commit is rejected, and no new turn starts while the call is unsettled.
