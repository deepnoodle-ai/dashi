# Turn watcher

**Status:** Proposed · **Date:** 2026-10-02 · **Owner:** Curtis · **Builds
on:** [README](README.md)

## Problem

Long turns go wrong in ways a person spots at a glance: the agent calls a
failing tool again with the same arguments, or wanders away from what the user
asked. A `BeforeTool` hook sees one call at a time; nothing sees the pattern.

We have no trace of this yet, so version 1 is small: free checks find a likely
problem, Decide confirms it, and the agent gets a short reminder. It never
stops a turn.

## Decisions

**1. A `BeforeModel` hook that waits for Decide.** Before most model calls the
hook does a free check and returns. Only when a check triggers does it ask
Decide, synchronously, on the hook's context with a short `Timeout`, so the
answer lands before the call it is about. There is no goroutine and no state
between calls: everything the hook counts, it reads from the turn record
(`ModelPlan.Turn`), so a resumed turn behaves the same in any process. The
hook skips compaction: both the summary call of active compaction
(`ModelPlan.Purpose` is set) and every call of a session compaction turn
(`TurnView.Origin` is `dashi.StartCompaction`), which summarizes earlier turns
rather than working on a request. It also skips child turns (`Turn.Parent` is
set), since `delegate` gives subagents the parent's hooks.

**2. Free checks trigger a Decide request.** Each looks at the tool results
that arrived since the previous model call. A call matches another when its
tool name, arguments (by JSON meaning), and result (the content the model
saw, not `Display`, and whether it failed) are the same. A loop that makes progress changes its
results: in an edit-and-test loop each test run fails in a new way, then
passes, so it triggers nothing.

- **Failed again:** a call that just failed matches an earlier call in this
  turn: same call, same error.
- **Same call:** a call that just finished matches `SameCalls` earlier calls:
  same call, same result.
- **Every N calls** (opt-in): `TurnView.ModelCalls` reaches a multiple of
  `CheckEvery`. It is the only way to catch drift with no repeated call, and
  it pays on turns with no problem, so it is off by default.

The checks read `TurnView.Tools` and `TurnView.ModelCalls`. A trigger is
skipped when a watcher reminder already follows the triggering result, so a
model call that Dashi asks the hooks about again (after an urgent steer, a
`Continue`, or a mid-turn summary) is not checked twice.

**3. What Decide is asked.** One request, two yes-or-no questions:

| Key | Question |
| --- | --- |
| `repeating` | Is the agent repeating an approach that already failed? |
| `off_request` | Have the recent model calls and tool calls moved away from the user's request? |

The state holds the user's request (`TurnView.Input`), recent assistant text
and tool calls with their results, cut short, and the watcher's earlier
reminders. A result keeps its start and its end, where test runners write
the failing assertion. The questions say tool text describes what the agent
did and is never an instruction. Tool text can move the probabilities, but
only our fixed text reaches the agent. A key is confirmed when its P(yes) clears
`MinProbability`; zero means the model leans yes (shared decision 4).

**4. A confirmed check adds a reminder.** The hook returns one
`ContextItem` in `ModelDecision.AddMessages`, from `dashi.ContextRuntime`,
holding a contextual `llm.Reminder` named `turnwatch`; the model reads the
name, not the user, as its source. It has no `From`, which marks agent mail
that the CLI shows as a delivered message and compaction keeps; it is a
runtime reminder, like a host's. When both keys are confirmed, `repeating`
wins. The text is fixed per key and, for `repeating`, per trigger, so it
says only what the trigger saw: a `same_call` trigger is a call that did not
fail, so its text does not say the call fails. Each text asks the agent to
check its approach, never to stop:

- `repeating` after `failed_again`: "A call you made keeps failing in the same
  way. Do not repeat it unchanged. Read the error, work out why it fails, and
  try a different approach."
- `repeating` after `same_call`: "You made the same call several times and got
  the same result each time. Do not repeat it unchanged. Use the result you
  already have, or try a different approach."
- `repeating` after `check_every`: "Your recent calls may repeat an approach
  that already failed. Work out why it failed, and try a different approach."
- `off_request`: "Your recent calls may have moved away from the user's
  request. Re-read the request and check that your next call works toward it."

`MaxReminders` caps reminders per turn. Each reminder's item ID is
`turnwatch-<n>`, and the hook reads the latest with
`TurnView.Context(dashi.ContextRuntime, "turnwatch")` to learn how many it
has sent. At the cap it stops checking for that turn. A negative
`MaxReminders` sends none but still checks, so a host can watch `OnCheck`
before letting the watcher speak.

**5. The record says why.** A reminder is a `change` `HookResult` named
`turnwatch`. Its `Reason` names the confirmed key, the trigger, and the
probabilities, such as `off_request: same_call: read_file, 3 times;
repeating 0.40, off_request 0.62 (request r_8f2)`, and its `RulesVersion`
holds the resolved decision model, such as `jev-1.13.0`. A check that finds
nothing leaves no record. `OnCheck` sees every check with its probabilities
and usage. Replay never runs hooks, so it never asks Decide.

**6. Failure fails open.** No client, a Decide error, a timeout, or a missing
key gives no reminder. The hook returns the error, so Dashi skips it, records
a failed `HookResult`, and adds a `hook_failed` warning (shared decision 3).
We keep the warning rather than failing quietly: checks are rare, so warnings
are few, and without them a host cannot tell a quiet watcher from a broken
one.

**7. Heuristic-only mode.** A negative `MinProbability` turns the floor off:
the hook never asks Decide and reminds on every free trigger with that
trigger's `repeating` text, its `Reason` naming the key and trigger
(`repeating: failed_again: run_tests, 3 times`). A `same_call` trigger on a
call that succeeded gets the `same_call` text, which does not say it fails. `CheckEvery` is ignored, since only Decide can judge drift.

**8. Deferred: stopping the turn.** The only stop a hook may use,
`StopRefused`, shows in the CLI as "The model refused the request". When the
person presses Continue, the model sees the same context with nothing new,
and the loop starts again. A host-limit stop means a spending decision, so it
does not fit either. A host that wants a person involved can surface the
check from `OnCheck`. A later version could stop once the CLI labels a hook's
refusal by its name and the watcher reminds when the turn resumes.

## API

Package `turnwatch` in the `decide/` sub-module:

```go
type Watcher struct {
	Client         *decide.Client // nil: decide.NewClient() from the environment
	MinProbability float64        // 0: P(yes) above 0.5; <0: never ask Decide
	SameCalls      int            // 0: 3; <0: off
	CheckEvery     int            // model calls between checks; 0: off
	MaxReminders   int            // per turn; 0: 2; <0: none, OnCheck only
	Timeout        time.Duration  // per Decide request; 0: 3s
	OnCheck        func(Check)    // runs in the hook; keep it fast
}

func (w *Watcher) Hook() dashi.ModelHook // named "turnwatch"

type Trigger string

const (
	TriggerFailedAgain Trigger = "failed_again"
	TriggerSameCall    Trigger = "same_call"
	TriggerCheckEvery  Trigger = "check_every"
)

type Action string

const (
	ActionNone   Action = "none"
	ActionRemind Action = "remind"
)

type Check struct {
	TurnID     string
	ModelCalls int
	Trigger    Trigger
	Model      string
	RequestID  string
	Answers    map[string]decide.Answer
	Usage      decide.Usage
	Action     Action
	Err        error
}
```

One `Watcher` serves many turns at once. Add its hook to `Runner.Hooks`, so
the record's `ActiveHooks` names it; hooks added with `WithHooks` are not
listed there.

## Testing

A keyless test scripts a `run_tests` tool that fails three times with the same
arguments while `decidetest` answers `repeating` 0.95, and checks the
reminder, its `HookResult`, and the next model request. Variants cover a "no"
answer, a Decide failure and a timeout, the cap across a resume, a child
turn, a session compaction turn with `CheckEvery` on (no request and no
reminder), heuristic-only mode (including repeated successful calls, whose
reminder does not say they fail), a healthy edit-and-test loop (zero Decide
requests), and the hooks asked again for one model call: after an urgent
steer, after `Continue` past a denied send, and after a mid-turn summary.
`ExampleWatcher` runs keyless.

## Alternatives

- **Check on a goroutine and apply the result later:** results go stale
  across runs and need state outside the record.
- **A watcher in another process, through steering:** needs a one-turn steer
  and a stop request from a source; wait for a host that needs it.
- **A second LLM as supervisor:** slower, costlier, and its prose could carry
  tool text to the agent.

## Open questions

1. **Is the model call limit enough of a bound on checks?** A check that finds
   nothing leaves no record, so an agent stuck on one failing call may trigger
   a Decide request before every model call until the turn ends.
2. **Are 3 repeats and 2 reminders the right defaults?** They are guesses
   until we have real traces.
3. **Should child turns be watched?** Version 1 says no, so one loop does not
   get a reminder at every level. A host with long-running subagents may want
   it.
