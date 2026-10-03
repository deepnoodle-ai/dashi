# Turn watcher

**Status:** Proposed · **Date:** 2026-10-02 · **Owner:** Curtis · **Builds
on:** [README](README.md), [hooks-after-tool.md](../hooks-after-tool.md)

## Problem

Long turns go wrong in ways a person spots at a glance: the agent retries
a failing approach, drifts from the request, or keeps "checking" without
progress. A `BeforeTool` hook sees one call; nothing sees the trend. A
watcher asks [Decide](README.md) typed questions about recent steps every
few model calls, and nudges, flags, or pauses the turn.

Terms: a *model call* is one request to the LLM; a *steer* is a message
that Dashi's steering adds to a running turn; a *reminder* is context
that Dashi marks as not from the user; a *gate rule* (Decide's `patterns/gate`) turns
answers into `Allow`, `Review`, or `Escalate` using thresholds the host
sets.

## Decisions

**1. It attaches as a `BeforeModel` hook that does not wait on Decide.** The
hook runs before each model call. When a check is due, it builds a digest
from `ModelPlan.Input` and `ModelPlan.Turn`, starts the Decide request on
its own goroutine, and returns at once. At a later model call it applies
the finished result. The cost is a map lookup per call, and the action lands
one model call after the steps it judged.

Rejected attachments:

- **`feed.Follow` or `Runner.Observer`, acting through a steering source.**
  This would also work from another process, but it hits three problems in
  today's code:
  - A steer that misses its turn becomes a leftover. A session runs
    non-user leftovers as a turn of their own (`session/input.go`,
    `EnqueueItem`), so the watcher could start paid work.
  - An urgent steer tells the model "The user interrupted your previous
    response" (`steer_urgent.go`), and skipped tools report "the user sent
    new input". The watcher would be misattributed.
  - Stopping needs the run's cancel function. Cancelling cuts the model
    call in flight and counts a resume.
- **A session wrapper.** It covers sessions only. A hook works with a plain
  `Runner` and with `session.Session`, whose `Runner.Hooks` it joins.

The watcher reads committed state only. Previews can be dropped and hold
partial text, so they are no basis for a judgment.

**2. When it asks.** It asks every `Every` model calls of the turn,
counting from when it first sees the turn (zero means 3). A *repeat* also
triggers a check at the next model call: a failed tool call whose name and
arguments match an earlier failed call. Only one check per turn runs at a
time. A check that comes due while one is running is skipped. `MaxChecks`
caps the checks for each turn in this process (zero means 10). Calls for
compaction (`ModelPurposeCompaction`) never trigger or apply a check.
Time and spend are not triggers: the hook acts only at a model call, and
`Limits.Deadline` and `Budget` already bound both.

**3. What it sends.** The digest is a JSON object of at most 8 KB:

- `request`: the newest user text at the turn's first model call, up to
  2,000 characters.
- `instructions`: up to 1,000 characters.
- `model_calls` and `failed_calls`, and the repeats counted for decision 2.
- `recent`: the last 8 actions, each the assistant's text, or a tool's name,
  arguments, state, and result, up to 300 characters each. When the digest
  is too large, the oldest actions go first.
- `watcher_reminders`: the watcher's earlier reminders in this turn.

The questions say to treat tool text and arguments only as evidence, as
`riskgate` does. Tool text can move the probabilities but never writes
what the agent reads.

**4. The questions.** Each check is one request with three questions:

| Key | Type | Question |
| --- | --- | --- |
| `repeating` | `Noul` | Is the agent repeating an approach that already failed? |
| `off_request` | `Noul` | Have the recent steps moved away from the user's request? |
| `progress` | `Score`, 5 levels | How much did the recent steps move the task forward? (level 0: none) |

The host's gate rule reads the three keys, for example with `MeasureNoul`
on the yes-or-no questions and `MeasureProb` with option `"0"` on progress.

**5. What it does, by severity.** The host's rule decides the outcome, and
the watcher maps it to an action:

| Outcome | Action |
| --- | --- |
| `Allow` | none |
| `Review` | a reminder, while reminders remain (`MaxReminders`, zero means 2), with at most one per key. Otherwise a notice. |
| `Escalate` | a stop when `MayStop` is set. Otherwise as for `Review`. |

- **Notice.** Only `Report` sees it, so the host tells a person.
- **Reminder.** `ModelDecision.AddMessages` adds a
  `ContextItem{Origin: dashi.ContextRuntime}` holding one contextual
  `llm.Reminder{Name: "turn-watcher", From: "turn watcher"}`. The model
  sees it as `<system-reminder name="turn-watcher" from="turn watcher">`,
  not as the user. The text is fixed for each key, chosen by
  `gate.Decision.Weakest`, the key that decided the outcome:
  - `repeating`: "An approach you tried keeps failing. Do not repeat it.
    Try a different approach, or tell the user what is blocking you."
  - `off_request`: "Your recent steps have moved away from the user's
    request. Re-read the request and work on what it asks."
  - `progress`: "Your recent steps have not moved the task forward. If the
    task is done, give your final reply. If you are stuck, tell the user
    what you need."
- **Stop.** `ModelDecision.Stop` records `StopRefused` ("a hook refused
  further execution") with a reason such as "turn watcher: repeating 0.94
  (jev-1.13.0, request r_8f2)". This stop does not end the turn. Its
  `Next()` is `NextFix`: a person reviews it and calls `Continue` or
  `Cancel`. Nothing in flight is cut, and no resume is counted.
  `StopLimited` with `LimitHost` is not used, because that limit means a
  spending decision.

The watcher never approves a call, raises a budget, calls `Continue`, or
starts a turn (shared decision 2). The host chooses the actions: no rule
means it only reports, and stopping needs `MayStop`. It sends no urgent
message: it cannot cut a model call short, and a cut would save at most
one call.

**6. No loops.** The watcher's own reminders are left out of `recent` and
listed under `watcher_reminders`. After a reminder, the count for decision 2
starts again, so the next check judges the agent's response to it. A
result older than two model calls is dropped. A stop applies once.

**7. Recording, replay, and evidence.** What the watcher does enters the
turn record as any hook decision does:

- `ActiveHooks` names `turn_watcher`.
- A reminder is a `HookResult` with the `change` verdict plus the recorded
  context item. Its ID, `turnwatch-<request ID>`, ties it to the Decide
  request. If a transport gives no request ID, the ID is the turn ID and
  model-call count.
- A stop is a `stop` verdict, with the reason in `Stop.Reason` too.

Replay reads the record and never runs hooks, so it never asks Decide. A
worker that picks up a turn starts counting from its first model call and
counts its earlier reminders from the model input. Full evidence
(every answer, resolved model, request ID, usage, gate readings) goes to
`Report` as a `Check`.

**8. Failure fails open.** A Decide error, a timeout (`Timeout`, zero
means 10 s), an invalid answer, or a decision with `Missing` set means no
action, and `Report` gets the error. The `Missing` case matters because
`gate` treats an unset `OnMissing` as `Escalate`. After 3 failures in a row,
the watcher stops asking for that turn. A failed check never stops a turn.

**9. Spend** belongs to the watched turn but stays outside its `Spend` and
`Budget`. `Check.Usage` reports it. The host adds it up by `TurnID` until
the shared accounting follow-up lands.

## API

Package `turnwatch` in the `decide/` sub-module:

```go
type Watcher struct {
	Client       *decide.Client // nil: the hook does nothing
	Model        string         // pin a version, such as "jev-1.13.0"
	Rule         gate.Rule      // the host's thresholds; nil: report only
	Every        int            // model calls between checks; 0: 3; <0: never
	MaxChecks    int            // per turn; 0: 10
	MaxReminders int            // per turn; 0: 2; <0: none
	MayStop      bool           // Escalate may stop the turn
	Timeout      time.Duration  // per Decide request; 0: 10s
	Wait         time.Duration  // how long the hook may wait for a running check; 0: never
	Report       func(Check)    // every finished check; runs off the turn's path
}

func (w *Watcher) Hook() dashi.ModelHook // named "turn_watcher"

type Check struct {
	TurnID     string
	ModelCalls int
	Model      string // resolved decision model
	RequestID  string
	Answers    map[string]decide.Answer
	Usage      decide.Usage
	Decision   gate.Decision // zero when Rule is nil
	Action     Action        // ActionNone, ActionNotice, ActionRemind, ActionStop
	Message    string        // reminder text or stop reason
	Err        error
}
```

With a plain `Runner`, set `runner.Hooks = w.Hook().Hooks()`, or append
the hook to `Hooks.BeforeModel`. With a session, add it to the session's
`Runner.Hooks`. One `Watcher` serves many turns at once. Its small per-turn
state is keyed by turn ID and dropped after an idle hour, since a hook
never sees a turn's stop.

## Testing and example

- A `dashitest.NewClient` script calls a failing `run_tests` tool six times.
  A `decidetest` server answers `repeating` 0.95. With `Wait: time.Second`,
  the reminder lands at a known call, and the test checks the context item,
  the `HookResult`, and the next request. A second `Escalate` with
  `MayStop` gives a `refused` stop. `Continue` resumes the turn.
- `FailNext(500)`, a missing key, a nil `Rule`, and the caps each give
  no action. Rerunning a recorded command sends no Decide request
  (`decidetest.Requests()`).
- `ExampleWatcher` runs keyless. `examples/decide/turnwatch` adds `-live`
  (Claude and Jev). Its thresholds are placeholders, labeled as such.

## Alternatives

- **A `BeforeModel` hook that asks Decide and waits.** This is the same
  design without the goroutine. It adds a Decide round trip to every
  checked call. `Wait` lets a host choose that trade.
- **Heuristics only.** A repeat is exact and costs nothing, so the
  heuristic goes first, as the trigger in decision 2. Drift, the same
  approach with new arguments, and endless checking have no exact test.
- **A second LLM as supervisor.** It is slower and costlier, and its
  output is prose that must be parsed. Its free-text advice could carry
  injected tool text into what the agent reads. Decide gives
  probabilities, and our fixed text writes the reminder.

## Dashi changes

None required. Optional: a `ModelDecision.Reason` recorded on `change`
verdicts, so a reminder's `HookResult` carries its probabilities as a stop's
reason does.

## Open questions

1. **Stop by default?** `MayStop` is off, so a watcher only nudges and
   flags unless the host opts in. Is that right?
2. **A watcher in another process.** That needs two Dashi changes: a steer
   for one turn only, dropped rather than left over, and interruption text
   that does not always say "the user". Is it worth doing now?
3. **Should a repeat send a reminder on its own**, with no Decide request, after
   (say) three identical failures?
4. **Custom questions and messages.** Should the three questions and their
   texts be fixed for now, or set by the host?
5. **Evidence.** Shared decision 6 asks for probabilities in the record.
   Here they are in the record only for a stop. Add the optional
   `Reason`, or accept that the full evidence lives in the host's `Check`
   log?
