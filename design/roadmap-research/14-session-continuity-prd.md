# 14. Sessions — PRD

**Status:** Proposed, 2026-09-29; replaces the 2026-09-28 draft (an optional
history reference). **Roadmap:** [item 14](../remaining-work.md); reworks [item
15](../remaining-work.md). **Basis:**
[research](14-session-continuity-research.md), deep studies of
[pi](14-session-study-pi.md) and [Codex](14-session-study-codex.md), and
[decision 19](../decisions.md). **Tech spec:** [sessions.md](../sessions.md),
with [session-engine.md](../session-engine.md) and
[session-input.md](../session-input.md). **PRD PR:** #96. **Size:** L. **Risk:** public API, new
concepts.

## User problem

Dashi's only high-level API is its engine, `Runner.Run(ctx, turn, command)`. It
runs one turn well: explicit state, idempotent commands, resume on any worker.
Applications think in conversations, so every host rebuilds the same layer on
top, and gets parts of it wrong.

- **A first program needs four types for one sentence**, and threads history by
  hand for the second: `Turn{}`, `Start`, `llm.UserMessage`, `llm.Text`, then
  `dashi.Project` and `Start.History`.
- **The CLI rebuilt a session layer.** `cmd/dashi/session` keeps ordered turn
  IDs (`<session>-<n>`) and rebuilds history from the records (`HistoryBefore`).
  `cmd/dashi/engine.go` resends that history on every `Continue`, builds a new
  `Runner` per command to change the agent, passes `dashi.Agent{}` placeholders
  on `Continue` and `Cancel`, cancels any unfinished turn before a new prompt,
  and decodes stops by hand (`LimitStopped`, `CanGoOn`, `StopText`).
- **Delegation makes the host write a scheduler.** `examples/delegate` has a
  60-line loop: `waitingNotices`, `Inbox.Leftovers`, `FollowUp`,
  `FollowUpTurnID`, and history threading.
- **Approvals take N+1 runs.** Each answer is a `Run`, then a `Continue`; every
  host copies the `examples/durable_approval` loop.
- **Compaction has no home.** Package `compaction` asks the host to store its
  record "next to its history", in a conversation store Dashi does not have.

**Customers.** Three hosts, one job: hold a multi-turn conversation with
approvals, steering, resume after a crash, delegation follow-ups, and
compaction, without writing a session layer.

- **Durable server hosts** on Cloud Run. They build a handle per HTTP request,
  see retries with idempotency keys, and lose workers mid-turn.
- **The `dashi` CLI**, our first real host. One process owns a session under a
  file lock.
- **A future ACP adapter** (item 23). It needs prompts, replay with stable
  message IDs, and `queue | steer` input.

## Terms

- **Conversation history:** every message across a session's turns, including
  tool calls and results. It is complete; compaction never shortens it.
- **Session:** the conversation history plus the events around it: stops,
  approvals, steers, compaction summaries, spend, warnings, closed turns. A
  session ID names it. It is stored as its turns' records and has no record of
  its own.
- **Projected history:** the messages sent to the model for one call: the
  conversation history after compaction and projection. `Start.History` and
  `Continue.History` carry it. It replaces the term "model view".
- **Session handle:** the Go value `dashi.Session`, which drives one session in
  one process or request. It holds no durable state. The type is named after
  what it drives, as `*sql.DB` is; we avoid "thread" and "conversation" as type
  names.

**The handle rule.** Anything that must outlast a handle goes in the turn
records. `Session` fields are settings for the handle, not saved state. The one
known exception is the in-memory queue: queued input lives only in the handle
until the durable queue exists, so `Enqueue` returns a `memory_only` warning
when the recorder is durable.

## Goals and non-goals

**Goals.**

1. A first program is two lines past the client, and a second message needs no
   history code.
2. A server reopens a session by ID in each request, with idempotent sends and a
   typed refusal when a turn is busy.
3. Approvals, steering, queued input, delegation follow-ups, and compaction work
   through the session.
4. Hosts stop decoding stops: one value names the next action. Only a limit
   raise reads `Stop`, for how much to raise.
5. The CLI, `examples/delegate`, and `examples/compaction` port onto sessions
   and lose their glue code.

**Non-goals.** A session store or session-owned log; a session lease or lock;
guessing between steer, queue, and replace on `Start`; driving anything on open;
mixing a session with provider-side conversation state; session metadata such as
titles and owners (the application keeps it). Forks, rewind, child sessions,
deletion, the durable queue, and mid-turn compaction come later.

## User flows

**First program.** An empty `Session` works: no recorder means an in-memory one,
and the first outcome warns `memory_only`.

```go
s := &dashi.Session{Runner: dashi.Runner{Client: client}}
out, err := s.Send(ctx, "What's the weather in Tokyo?")
out, err = s.Send(ctx, "And in Fahrenheit?")
```

**Server handler.** A fresh handle per request finds the same session in the
records. A retry with the same key gets the first result, never a second turn.

```go
func (h *Handler) Message(w http.ResponseWriter, r *http.Request) {
    s := &dashi.Session{ID: r.PathValue("session"), New: r.URL.Query().Has("new"),
        Runner: h.runner, Steering: h.steering}
    start := dashi.Say(readMessage(r))
    start.CommandID = r.Header.Get("Idempotency-Key")
    out, err := s.Run(r.Context(), start, dashi.WithOwner(h.workerID))
    var busy *dashi.TurnBusyError
    switch {
    case errors.As(err, &busy):
        writeJSON(w, http.StatusConflict, busy) // retry, or steer via h.steering
    case err != nil:
        writeError(w, err)
    default:
        writeJSON(w, http.StatusOK, render(out))
    }
}
```

The v1 server contract: the queue lives in one handle and dies with the request
(`Enqueue` returns `memory_only`); a turn another worker runs is steered through
the host's steering source; stopping it from another process is later. A
recovery worker takes over a turn whose lease expired. It never closes that turn
and starts another, since the old worker may still be acting:

```go
st, err := s.Open(ctx)
if err == nil && st.Next == dashi.NextResume { // no live claim
    // Takeover: a started non-idempotent call becomes uncertain, never rerun.
    out, err = s.Run(ctx, dashi.Continue{}, dashi.WithOwner(workerID))
}
```

**CLI.** One handle per open session, the only driver under the CLI's lock;
`/model` changes one field.

```go
s := &dashi.Session{ID: id, Runner: runner, Exclusive: true,
    Agent: dashi.Agent{Model: model}}
st, err := s.Open(ctx) // reports; never runs anything
if st.Next == dashi.NextResume {
    // banner: "turn 7 was interrupted; Enter resumes it"
    out, err = s.Run(ctx, dashi.Continue{}) // Enter; /discard runs Cancel{}
}
// A new prompt after a crash mid-command refuses with busy.CallID set:
// "the last command may still be running: mark it done, or discard it?"
// Mark done runs Settle{} for the call; discard runs Cancel{}.
// Typed while a turn runs:
s.Steer(ctx, text)
// A new prompt closes a stopped turn; the outcome warns turn_closed:
out, err := s.Send(ctx, prompt)
// Enter after NextRaiseAttempts:
out, err = s.Run(ctx, dashi.Continue{ModelAttempts: 2 * limit})
```

**Approvals.** One call records every answer, then continues.

```go
if out.Next() == dashi.NextAnswer {
    out, err = s.Answer(ctx, []dashi.Settle{out.Pending[0].Approve()})
}
```

**Delegation.** A finished task's notice is queued on the session; the queue
starts the follow-up turn.

```go
tasks := delegate.NewTasks(delegate.Config{OnTask: func(e delegate.TaskEvent) {
    if e.Kind == delegate.TaskEnded {
        s.EnqueueItem(ctx, e.Task.Notice().Steer().Item)
    }
}})
out, err := s.Send(ctx, "Who owns billing, and who is on call for search?")
// Wait for the tasks, running the queue as notices arrive:
out, err = delegate.RunUntilQuiet(ctx, s, tasks)
```

**Compaction.** Set `Compactor: compaction.New(compaction.Policy{Model: info})`;
the session runs a compaction turn before a turn when the projected history is
too long. A transcript UI walks `s.Turns` and shows a marker for each turn whose
`TurnView.Origin` is `StartCompaction`.

## Requirements

- **FR-1** `Session` is built only on the public engine API and lives in package
  `dashi`. `Send` is exactly `Run(ctx, dashi.Say(text))`.
- **FR-2** Turn records are the only truth. Turn *n* of session `S` is `S-n`.
  The last turn comes from an optional `SessionIndex`, or from probing.
- **FR-3** No lock. Recorder conditional appends, `S-n` collisions, and
  permanently ended turns are the guarantee. An optional `TurnLiveness` reports
  live, stopped, or unknown; an expired lease is unknown, since only proof that
  no worker drives the turn counts as stopped. `Exclusive` promises that the
  handle is the only driver and that earlier processes have exited. Neither
  proves that a started effect stopped. A lost worker's turn is taken over with
  `Continue{}` under `WithOwner`, which never reruns a started non-idempotent
  call.
- **FR-4** A `Start` refuses with `*TurnBusyError` only when the last turn is
  busy: live, possibly live (liveness unknown, an expired lease included),
  waiting on the host, or holding an unsettled uncertain call, whatever its
  liveness and even under `Exclusive`. It closes any other stopped turn with a
  warning. The error names the recovery calls (`Continue{}`, `Settle{}`,
  `Cancel{}`) and, for an uncertain call, its `CallID`. `Cancel{}` on such a
  turn records that the host accepts the risk; it does not stop the effect.
- **FR-5** An ended turn always projects: unsettled calls become "result
  unknown" errors, and results that arrive later never change it.
- **FR-6** A `Start` with a known command ID returns the first result, is
  refused as a duplicate while that turn has not ended, or conflicts when its
  input differs. This check runs before FR-4.
- **FR-7** `Steer` reaches the running turn; `Enqueue` holds input for the next
  turn. The queue starts the next turn when the session goes idle. Missed steers
  move to the queue.
- **FR-8** Compaction is its own turn, recording the replacement history. Every
  reader honors it, whatever its configuration.
- **FR-9** `Agent` moves from `Runner` to `Start`. A session builds each new
  turn's agent field by field from `Start.Agent`, `Session.Agent`, and the last
  user turn. Tool selection moves with it.
- **FR-10** `TurnView.Next()` maps every stop to one of eight actions; the
  session alone refines `NextResume` to `NextRunning`.
- **FR-11** Opening never drives. An unknown ID fails unless `New`; `New` on an
  existing session fails.
- **FR-12** `Stream` spans every turn it runs; `Turns` gives the transcript.

## Implementation tracks and acceptance

Five stacked tracks. Each is usable alone and tested by porting real code.

**Track 1: stop meanings and ended turns (M).** [Spec](../session-engine.md).
- A table test covers every `StopKind`, `LimitKind`, and stop reason in the code
  and its `Next` value; a new reason without a row fails the test.
- A canceled turn with a started, waiting, and uncertain call projects each as
  an error result; a late `Settle` after the cancel leaves `Project` byte-equal.
- A resumed crashed turn carries the check-before-repeating reminder.
- `cmd/dashi`'s `LimitStopped`, `CanGoOn`, and `Open` become one-liners over
  `Next`.

**Track 2: `Agent` on `Start` (L).** [Spec](../session-engine.md).
- `Runner` has no `Agent`; `Continue`, `Settle`, and `Cancel` need no
  placeholder. The CLI uses one `Runner`.
- `Agent.Tools` filters the resolved toolset by name before each call; `NoTools`
  offers none; a missing name warns once; the selection changes the automatic
  revision.
- `delegate` uses `CurrentTurn`, and its tests pass with `Subagent.Tools` merged
  into `Agent.Tools`.
- `TurnView.Origin` reports the recorded origin; carry-forward reads it.

**Track 3: session core (L).** [Spec](../sessions.md).
- The first-program flow runs keyless in a new `examples/chat` on a scripted
  `dashitest` client (new), with a tool call and a denied call.
- Tests cover every row of the last-turn table, with and without `Exclusive`
  and `TurnLiveness`; a busy error's message names its recovery calls.
- The old worker's claim expires during a model call, and during a
  non-idempotent tool call: a new message refuses; takeover with `Continue{}`
  does not rerun the tool call; no new turn overlaps the unsettled call.
- An unsettled uncertain call refuses a new message even when the turn is
  stopped or `Exclusive` is set; the error carries its `CallID`.
- Idempotency tests: a retry while the turn runs, after it ends, after queue and
  compaction turns ran after it, with different input, and two racing workers.
- `dashitest` checks `SessionIndex` and probing; `filerecorder` passes by
  probing.

**Track 4: steering, the in-memory queue, and the CLI (M).**
[Spec](../session-input.md).
- Dispatch-on-idle, missed-steer, and `memory_only` tests; `Enqueue` returns the
  warning.
- A queue turn's `Start` fails validation, is rejected by the recorder, and has
  an unknown commit outcome: the first two restore the items in order; the
  third loads the turn and consumes them only when its start committed. Then a
  rejected model step after `turn_started`, and `ErrRecordInvalid` after it:
  enqueue another item, and the original is not delivered twice. The same cases
  for user items put ahead of a caller's `Start`.
- The CLI exits while its tool's child process keeps running: a new `Exclusive`
  handle's `Start` refuses, naming the call; `Cancel{}` then `Start` succeeds,
  and the record shows the cancel step.
- `examples/delegate` uses `RunUntilQuiet`; `FollowUpTurnID` is gone.
- A cross-turn `Stream` yields each turn's events, tagged with its `TurnID`.
- `cmd/dashi` runs on `Session` with `Exclusive`; its `session` package keeps
  only metadata (title, directory, diffs, lock).

**Track 5: compaction turns (L).** [Spec](../session-input.md).
- A compacted session reopened by a handle with no `Compactor` builds byte-equal
  projected history; a digest mismatch warns and changes nothing.
- A failed summary cancels the compaction turn, keeps the previous projected
  history, and warns; no compaction runs right after a failed or canceled turn.
- `examples/compaction` runs on `Session`; roadmap item 15 is updated.

## Alternatives considered

- **Helpers only.** `Say`, `HistoryBefore`, `Next`, and `RunUntilQuiet`, with
  no session. Rejected: every host still writes the glue, and each helper is
  designed without the concept that joins them.
- **Other entry points.** `dashi.NewChat`, an in-memory chat helper, was
  rejected because the first real host would replace it; the session is durable
  from the start. `runner.Ask(text)` was rejected as a second verb: `dashi.Say`
  keeps one `Run` entry point and keeps `Start` visible.
- **A session value, state in and out (Approach 2).** Like `Turn`, the caller
  stores it. Rejected: two values to save and keep consistent with the turn
  records, a value that grows in memory mode, and a second method family on
  `Runner`, the sibling-API problem this work removes.
- **Sessions in the engine (Approach 3).** The recorder stores sessions and
  commands are addressed by session ID. Rejected: it reverses decision 19, makes
  every recorder implement sessions, and is the hardest change to undo. Approach
  1's IDs and records are what it would store, so it stays open.
- **The draft's `HistoryRef`.** An opaque history reference and resolver.
  Rejected: it moves the problem into a host resolver and gives no session
  behavior (busy rules, queue, compaction, stop meanings).
- **An `OnBusy` policy on `Start`.** Rejected: once stopped turns close on their
  own, nothing needs it, and a server must not guess what the user meant.
  `Steer` and `Enqueue` are explicit calls.
- **Compaction as a cut.** Smaller, but recomputed on every open, so a deploy
  that changes projection breaks the fingerprint of a stopped turn; it cannot
  hold non-contiguous kept messages or opaque provider items.
- **Carrying the whole agent forward.** Setting `/model` would drop instructions
  and tools.

## Risks

| Risk | Mitigation |
| --- | --- |
| Closing stopped turns by default discards one a host might continue, such as one at its model-call limit | `turn_closed` warning; add a `KeepStopped` setting if a host needs it |
| Chat UIs find refusal awkward | UIs call `Steer` or `Enqueue`; if every host enqueues on refusal, add an opt-in accept policy |
| A fresh handle loads turns back to the newest compaction turn on every send | Measure on the CLI's longest sessions |
| Without `TurnLiveness` or `Exclusive`, a crashed turn blocks new messages until the host resumes or cancels it | The error names the turn and its recovery calls; the CLI sets `Exclusive` under its lock |
| A worker whose lease expired may still finish an in-flight effect | Expired is unknown, never stopped; takeover never starts a second copy, and the uncertain call waits for the host to settle it |
| A tool's child process outlives a crashed CLI despite `Exclusive` | The unsettled call keeps the turn busy until the user marks it done (`Settle`) or discards it (`Cancel{}`, a recorded acceptance of the risk) |
| Compaction turns appear in transcripts | `TurnView.Origin`; check against ACP replay |
| Queued input is lost between requests | `memory_only` warning; the durable queue is the first later note |

## Later

Each gets its own note. **Durable queue** first, with the requirements in the
spec. **Mid-turn compaction** in the engine, before the CLI relies on sessions
for long turns. **Fork** (a new session whose first start records the parent and
the cut; stopped turns copied as closed). **Rewind** (a recorded marker with an
expected-last-turn check; conversation only). **Child sessions** (a parent link,
resumable by task ID). **Deletion and retention** (an optional deleter; turns
before the newest compaction turn; the host's explicit choice). **Replacing a
running turn across processes** (a durable stop request). **An optional
`SessionIndex` lookup by `CommandID`**, for a longer dedup window. **The ACP
adapter** (item 23). **An opt-in accept policy** if chat hosts all enqueue on
refusal. When a caller needs them: **`KeepStopped`** (refuse rather than close),
**`Withdraw`** (take back a queued item), and **`FollowSession`** with a
`SessionCursor` (follow a session across turns from another process).
