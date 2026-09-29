# Session engine changes

**Status:** Proposed, 2026-09-29. Companion to [sessions.md](sessions.md); PRD
[item 14](roadmap-research/14-session-continuity-prd.md). Covers track 1 (stop
meanings and ended turns) and track 2 (`Agent` on `Start`). Both change the
engine, and both are useful without sessions.

## `TurnView.Next()`

One method maps every stop to the host's next action, so hosts stop decoding
`Stop` fields. The project goal it serves: fix "errors with several meanings" at
the library layer. It is not one complete answer in every case: `TurnView`
returns `NextRunning` only in a hook's view (see below), and a
`NextRaiseBudget` turn may also need more model attempts, which the host reads
from `Stop`.

```go
type Next string

const (
    NextNone          Next = "none"           // completed, or ended for good
    NextRunning       Next = "running"        // no stop recorded; live (hook view, session)
    NextResume        Next = "resume"         // no stop recorded; not known to be live
    NextAnswer        Next = "answer"         // pending approvals or results
    NextContinue      Next = "continue"       // Continue can run now
    NextRaiseBudget   Next = "raise_budget"   // stopped at a cost or token cap
    NextRaiseAttempts Next = "raise_attempts" // stopped at the model-call limit
    NextResolve       Next = "resolve"        // fix a cause, then Continue, or close
)

func (v TurnView) Next() Next
```

`Next` reads the **recorded** stop. For a record with no stop step after the
latest `Start` or `Continue`, `Replay` and `Outcome` fill in a projected stop
(uncertain, interrupted, waiting, or empty; `state.go`, `projectedStop`) and
mark it `Stop.Projected`; `Next` then returns `NextResume`. The mark is an
exported field, so a view sent as JSON gives the same `Next`. A hook's view
during a run has an empty stop that is not projected, and gives `NextRunning`.
Otherwise `TurnView` cannot know liveness; the session refines `NextResume`
([sessions.md](sessions.md#concurrency-and-liveness)). `NextResume` means
`Continue` resumes the turn when `ReadyToContinue`; otherwise `Continue` gives
`ErrNotReady`, and the host settles `Pending` first or cancels.

Every `StopKind`, `LimitKind`, and stop reason in the code maps to one value:

| Recorded stop | Condition or reason | `Next` |
|---|---|---|
| none: the turn has not started (`Revision` 0) | | `NextNone` |
| none after the latest `Start`/`Continue` (`Stop.Projected`) | | `NextResume` |
| none, in a hook's view during a run | | `NextRunning` |
| any non-terminal stop | structured output recorded and `ReadyToContinue` | `NextContinue` (finishes without a model call) |
| `completed`, `canceled` | | `NextNone` |
| `limited` | `Terminal`: `deadline`, `interruptions`, or `cost` with unpriced spend | `NextNone` |
| `limited` | `model_attempts`, also with structured output recorded (the reserved final answer) | `NextRaiseAttempts` |
| `limited` | `cost`, `output_tokens`, `total_tokens`, cap spent (not `ReadyToContinue`) | `NextRaiseBudget` (raise attempts as well when none are left; decision 34) |
| `limited` | `cost` with `ReadyToContinue`: the next call's cost risk is unbounded (no price, no finite `max_tokens`, hosted tools), so a raise would not help | `NextResolve` |
| `limited` | `provider_pauses`, `ReadyToContinue` (otherwise `NextResolve`) | `NextContinue` |
| `limited` | `host` (a `BeforeSend` hook's spending denial) | `NextResolve` |
| `limited` | reason `output` or `context` (one call hit its output or context window; no `Limit` detail) | `NextResolve` |
| `waiting` | `ReadyToContinue` (every answer recorded) | `NextContinue` |
| `waiting` | otherwise: deferred approval, external tool result, `provider approval pending`, `tool result or approval pending` | `NextAnswer` |
| `interrupted` | `ReadyToContinue`: `pause requested`, a context cause, `worker stopped before result` | `NextContinue` |
| `interrupted` | not ready (interruptions used up, pending host work) | `NextResolve` |
| `refused` | a model refusal (the provider's raw reason) or a hook `Deny` | `NextResolve` |
| `failed` | `tool_fatal`, `tool_outcome_invalid`, `hook_failed`, `output_rejected` | `NextResolve` |
| `failed` | `content_no_resolver`, `content_unavailable`, `content_too_large`, `content_size_mismatch`, `content_digest_mismatch` | `NextResolve` |
| `failed` | free-text failures: provider rejection, invalid or `incomplete_model_result`, `empty_response`, `tool binding unavailable`, `tool binding changed or unavailable`, `hosted tool cannot execute locally`, `prepared request changed destination`, `prepared request changed original model input`, `untrusted stop with client calls`, `untrusted stop with hosted approval`, native output schema unsupported, replay errors | `NextResolve` |
| `uncertain` | `ReadyToContinue` (the host settled it) | `NextContinue` |
| `uncertain` | otherwise: `model attempt needs reconciliation`, `tool outcome needs reconciliation`, `provider-hosted tool may have run`, uncertain dispatch | `NextResolve` |

`NextContinue` is never returned for a refusal, so a host is never told to ask
the model again after it refused. A test builds each row; a new reason with no
row fails the test. Track 3 may map an `interrupted` turn that is not ready but
has `Pending` items to `NextAnswer`.

**CLI.** `LimitStopped` becomes `Next() == NextRaiseAttempts`, and `Open`
becomes `Next() != NextNone`. `CanGoOn` accepts `NextContinue`,
`NextRaiseAttempts`, and `NextResume` with `ReadyToContinue` (the CLI holds the
session lock, so a turn with no recorded stop is not live). It also accepts a
failed turn that is ready (`NextResolve`, `ReadyToContinue`, and `Stop.Kind ==
StopFailed`, never a refusal): the library cannot tell a rejected request from
a rate limit, but the user can, so the CLI shows "Press Enter to retry".

**Resume after a crash.** When `Continue` resumes a turn with no recorded stop
(`NextResume`), the runner adds a runtime reminder for the next model call: the
previous worker stopped mid-turn, so check the current state before repeating
any action. Dashi already reruns only read-only or idempotent tools and marks
the rest uncertain (decision 11); the reminder covers the model's side, as
Codex's restart turn does. Like the steering reminder, it is projected from the
record, not recorded, and a later dispatched call drops it. Crashes that repeat
before a call reaches the provider still give that call one reminder.

## Ended turns project and freeze

A turn has **ended** when its stop is terminal (`ErrTurnEnded` guards it). A
session *closes* a stopped turn by canceling it, which ends it. Today a terminal
stop settles only calls that never started (`state.go`, the `StepStopped` case).
Calls in state started, waiting, or uncertain stay pending, and so does a model
intent. `Project` then fails ("client tool call is unresolved", "model intent
needs reconciliation"; `project.go`). And `acceptsAfterEnd` still accepts a
`Settle` for started work after the terminal stop (`state.go`), which would
change an old turn's projection after later turns were built on it. With the
skip-unreadable rule, an ended turn would vanish from the projected history,
including the user's prompt.

Two fixes in track 1:

- **Project unsettled calls as errors.** In an ended turn, each tool call still
  started, waiting, or uncertain projects as an error result: "result unknown;
  the turn ended". A pending or unresolved model attempt projects nothing,
  since no message was recorded. Hosted approvals are already cleared at a
  terminal stop.
- **Freeze at the terminal stop.** `Project` reads an ended turn's steps only up
  to its terminal stop. Later results are still recorded, for audit and
  accounting (`Spend` counts them), but never projected, and `TurnView.Answer`
  stays fixed there too.

The fix avoids LangGraph's opposite mistake of patching dangling calls with fake
successes (deepagents#5547).

## `Agent` moves to `Start`

The agent is fixed per turn: recorded at `Start`, and `Continue` uses the
recorded one. On `Runner` it forces the CLI to build a runner per command, and
to pass `dashi.Agent{}` placeholders. Placement rule: recorded values go on the
command, process wiring in `RunOption`s, services on `Runner`.

```go
type Runner struct {
    Client          llm.Client
    Tools           tool.Toolset
    Recorder        Recorder
    Hooks           Hooks
    Observer        Observer
    OnContextDone   ContextDone
    ContentResolver ContentResolver
    ContentLimits   ContentLimits
}

type Start struct {
    CommandID    string
    Agent        Agent // zero fields take catalog defaults
    Origin       StartOrigin
    Budget       *Budget
    OutputSchema json.RawMessage
    History      []llm.Message
    Input        []llm.Message
    Parent       *CallRef
}
```

The engine fills zero `Start.Agent` fields from the client and the catalog
(`defaults.go`, `agentFor`). Commands on an existing turn always use the
recorded agent, so `ErrAgentChanged` applies to a retried `Start`, whose digest
covers the agent, and to a caller's snapshot whose recorded agent differs from
a `Loader`'s record. The `Limits.ToolResultBytes` warning at `Start` reads
`Start.Agent`.

## Carry-forward

In a session, the agent for a new turn is merged **field by field**:
`Start.Agent`, then `Session.Agent`, then the agent recorded by the last
**user-origin** turn, then the engine's defaults. A zero field falls through to
the next source. The merge recurses into `Settings` and `Limits`; a nil pointer,
empty string or slice, zero number, or false falls through. To turn a setting
off, use its negative or `No…` form (zero-value rule). Compaction and scheduled
turns never pass on their agent: carrying starts from the last user-origin turn.

| Field | Carries forward? |
|---|---|
| `Instructions`, `Model`, `Settings`, `OutputSchema`, `Tools`, `NoTools`, `Limits` (except `Deadline`), `ID` | Yes |
| `Limits.Deadline` | Never: an absolute time would stop the next turn before its first call |
| `Provider`, `API` | Only while they match the runner's client; otherwise they, and `Model`, fall to the client's defaults, so a host that changes clients does not fail every turn |
| `Revision` | Only when set by the host; an `auto:` revision is dropped and recomputed, since the merged agent may differ |
| `Start.Budget`, `Start.OutputSchema`, `Start.Input` | Never: one turn only |

Codex documents the same per-setting rule (`output_schema` is turn-only there).
Replacing the whole agent would drop instructions and tools whenever someone
sets a model.

## Tool selection

Implementations stay on `Runner.Tools`, a `tool.Toolset` resolved before each
model call, so MCP toolsets can change. Selection moves to the agent:

```go
type Agent struct {
    // ...existing fields
    // Tools names the tools this agent may call. Empty means every tool
    // the runner offers.
    Tools []string `json:"tools,omitempty"`
    // NoTools offers no tools; a compaction turn sets it.
    NoTools bool `json:"no_tools,omitempty"`
}
```

Before each call the runner filters the resolved toolset by name, then applies
`BeforeModel` hiding. Nothing new is recorded at start: the selection is part of
the recorded agent, and it enters the automatic revision. A named tool the
toolset does not offer records a `tool_not_offered` warning once per turn, on
the next committed step. `NoTools` wins over `Tools`. Only selected tools are
validated, and a call to a tool that was not selected settles as an unknown
tool without running. The final-answer reserve and `dashi_submit_output` are
unaffected.

## `CurrentTurn` and `delegate`

`delegate` reads the parent agent through `CurrentRunner(ctx).Agent`
(`delegate/tasks.go`, the child start), which no longer exists.

```go
// CurrentTurn returns the view of the turn whose tool call ctx serves,
// including its recorded agent.
func CurrentTurn(ctx context.Context) (TurnView, bool)
```

The view is taken as the call starts, with an empty `Stop` as in a hook's view;
a `Toolset` sees the turn as of the model call it offers tools for.
`CurrentRunner` stays for services. `delegate` builds a child's `Start.Agent`
from `Subagent.Agent`, taking provider, API, and model from `CurrentTurn` when
unset (decision 22 of item 13). `Subagent.Tools` becomes `Subagent.Agent.Tools`;
`Subagent.ReadOnly` stays, since it filters by annotation. A child draws only
from the tools the parent agent may use: the parent turn's selection applies
before the child's. `Recover` and resume no longer set `runner.Agent`:
`Continue` uses the recorded agent. `FollowUp` returns a `Start` with no agent;
the host sets it.

## Record changes

`Agent` gains `tools` and `no_tools`. Track 2 adds `TurnView.Origin`, the
recorded `Start.Origin`: carry-forward, the idempotency window, and
`TurnBusyError.Origin` (track 3), and queue turns (track 4) all read it. Track 5
adds the `compaction` `StartOrigin` and a `StartFact.Compaction` fact
([session-input.md](session-input.md#compaction-turns)). Dashi has no users, so
there is no migration.
