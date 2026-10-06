# PRD: Hooks

**Status:** Approved (decision 31) · **Date:** 2026-09-28 · **Owner:** Curtis

## Problem

Dashi's one `Policy` interface has the right contract (read-only views in,
recorded decisions out) but the wrong packaging: "policy" suggests allow/deny
rules, only one is allowed, and every error ends the turn. Default allow
shipped in #46; this replaces `Policy` with hooks. PR 1a replaced `Policy`;
PR 1b added the submission path and vetoes. Later hook work is in
[hooks-after-tool.md](hooks-after-tool.md).

## Approach

A *hook* is a named function Dashi calls at one *hook point* of a turn. Any
number run at each point, in order, and their decisions combine by a fixed rule.
Each decision is saved with the step it leads to; `Turn.View` reads the record
and never calls hooks. A point that guards something *fails closed* (a hook error
blocks the action); one that shapes a request *fails open* (the failing hook is
skipped, with a warning). Every model call passes `BeforeModel` and
`BeforeSend`, and every eligible client call passes `BeforeTool`, including
after resume. A crash before a decision is committed may call a hook again, so
hooks stay idempotent.

## API

```go
type Hook[P, D any] struct {
    Name string // nonempty, unique within its hook point
    Func func(context.Context, P) (D, error)
}
// One defined type per point, each with Hooks() returning a Hooks value
// that holds just that hook.
type (
    ModelHook  Hook[ModelPlan, ModelDecision]
    SendHook   Hook[SendPlan, SendDecision]
    ToolHook   Hook[ToolPlan, ToolDecision]
    FinishHook Hook[FinishPlan, FinishDecision]
)
type Hooks struct {
    BeforeModel  []ModelHook
    BeforeSend   []SendHook
    BeforeTool   []ToolHook
    BeforeFinish []FinishHook
}

func JoinHooks(hs ...HookSet) Hooks // always allocates; HookSet has Hooks()
func WithHooks(Hooks) RunOption   // after Runner.Hooks, for this call only
// OnTool decodes a tool's arguments into T; the hook is named after the tool.
func OnTool[T any](tool string,
    f func(context.Context, ToolPlan, T) (ToolDecision, error)) ToolHook
func Allow(reason string) ToolDecision // recorded; combines as no opinion
func Deny(reason string) ToolDecision
// NeedsApproval stops the turn waiting; the host resolves it with
// Pending.Approve or Deny.
func NeedsApproval(reason string) ToolDecision
func Rewrite(args any) (ToolDecision, error)
// RequireApproval makes the named tools need approval, or every application
// tool when none are named. Hook name and RulesVersion:
// "require_approval:<sorted, comma-separated names>" ("require_approval:*"
// for all).
func RequireApproval(tools ...string) Hooks
```

A console prompt; a server uses `Hooks: dashi.RequireApproval("deploy")`:

```go
ask := dashi.OnTool("deploy",
    func(_ context.Context, _ dashi.ToolPlan, in deployArgs,
    ) (dashi.ToolDecision, error) {
        fmt.Printf("Deploy %s to %s? [y/N] ", in.Version, in.Environment)
        if yes(answers) {
            return dashi.Allow("operator approved"), nil
        }
        return dashi.Deny("The operator declined this deploy."), nil
    })
runner.Hooks = ask.Hooks()
```

**Plans and decisions.** A zero decision means *no opinion*.

- `ModelPlan{Input, Turn}` and `SendPlan{Request, Turn, WorstCaseCost, Remaining}`
  are unchanged; `ToolPlan{Call, Definition, Turn}`;
  `FinishPlan{Turn, ModelCallID, Reply, Output, OutputErr}`, where on a
  structured turn `Output` is the parsed value or `OutputErr` says why parsing
  failed.
- `ModelDecision{Settings, AddMessages, Stop}`;
  `SendDecision{Deny, Reason, Limit, Reservation}` (a `Limit` implies `Deny`);
  `ToolDecision{Kind, Reason, RulesVersion, Rewrite}`;
  `FinishDecision{AddMessages, AcceptEmpty, Reason}`.
- `ToolVerdict{Kind, Reason, Hook, RulesVersion}` is the combined result for a
  call, recorded as `ToolFact.Verdict` and shown as `ToolView.Verdict`. An
  allow takes the name and reason of the first hook that returned `Allow`;
  with none, `Hook` is empty and the reason is `no hook opinion`.

Removed: `Runner.Policy`, `Policy`, `AllowLocalPolicy`, `ModelAuthorization`,
`StopDecision`, and the approval's policy revision field.

## Hook points

Hooks run one at a time: `Runner.Hooks`, then `WithHooks`, each in list order.
Panics count as errors. When the run's context has ended, a hook's error gives
the normal context stop (interrupted, or canceled under `OnContextDone`) and
nothing is recorded for that hook.

| Point | Combining rule | Hook error or invalid result |
|---|---|---|
| `BeforeModel` | Each hook sees settings as changed by earlier ones; non-nil `Settings` replaces them; `AddMessages` appends; the first `Stop` ends the chain and no added messages commit. A `Stop` may be `refused`, `failed`, or `limited` with a host, cost or token `Limit` | Fails open: `WarningHookFailed`; that decision is dropped |
| `BeforeSend` | The first `Deny` stops: `StopRefused`, or `StopLimited` with `Limit`. `Reservation` is kept per hook name, 16 KiB in total | `StopFailed`, reason `hook_failed` |
| `BeforeTool` | The first deny ends the chain; otherwise needs-approval beats allow. A `Rewrite` is schema-checked and restarts the chain | The call is denied ("Denied: check `<name>` failed."); `WarningHookFailed`. With `FailOpen`, no opinion and a `WarningHookFailed`, for advice only |
| `BeforeFinish` | `AddMessages` items append; `AcceptEmpty` if any hook says so; the first `Reason` is kept | `StopFailed`, reason `hook_failed` |

- **`hook_failed`** sits beside `tool_fatal`: fix the cause, then `Continue`.
  It does not count toward `Resumes`, so auto-resuming hosts do not loop.
  After a `BeforeFinish` failure, `Continue` runs the finish point again on the
  recorded reply, without another model call, unless messages were added
  since: then the model is asked again. The runner sets
  `Stop.Ended`; hooks never do.
- **Rewrites.** A rewrite equal to the current arguments by JSON meaning is no
  opinion. After 3 restarts the call is denied ("Not run: a check kept changing
  the arguments.", with `WarningHookFailed`) and the turn continues.
  `Call.RequestedArgs` keeps the model's arguments.
- **Tools.** Arguments are schema-checked before `BeforeTool`. Invalid, unknown
  and `dashi_submit_output` calls skip hooks. An `OnTool` decode failure is a
  hook error.
- **Approvals.** The call ID and argument hash bind an approval; hooks do not
  re-run at `Resolve`.
- **Validation.** `Run` rejects an empty or duplicate name within a point and a
  nil `Func`, before recording. Reusable packages prefix their names. The host
  supplies the same `Runner.Hooks` on every command. Hook-added
  messages pass `validateContext` like any added item.

## The finish point

A *candidate* is a model reply that would end the turn: final text, or a
schema-valid `dashi_submit_output` call. One function handles both, in order:
steering's check, the output parse, `BeforeFinish`, then the empty-reply and
schema checks, unless a hook rejected the reply. A `Reason` is a *veto*
(the reply is unacceptable); `AddMessages` alone is a *nudge*. The model sees
"Not accepted: <reason>" on either path.

- **Text path.** The reason and the items are recorded in a
  `StepLoopContinued`, whose position says which reply it replaced.
- **Submission path.** A submission skips `BeforeTool` (the runner records
  its allow), so it goes requested, allowed, started. The finish point runs
  after `tool_started` and the schema check. `rejectSubmission(call, reason,
  source, items)` finishes it as `tool.StateFailed` with that output,
  `ToolFact.Reason` naming the source, and the items in the same step's
  `Context`, so a crash cannot separate them.
- **No further call.** A nudge never spends the reserved call: without a
  normal call it is dropped with `WarningMessagesDropped` and the reply
  stands. A veto may use the reserved call; when no call fits at all, or the
  reserved reply is vetoed, it is still recorded and the turn stops
  `StopFailed` with `output_rejected` and no output. A vetoed reply never
  shows in `Reply` until a new one arrives (`ContextFact.Vetoed`).
- **Recorded items.** Items whose ID the turn already records are dropped
  before deciding, so a nudge with a fixed ID nudges only once per turn.
- **After `hook_failed`.** Messages added since turn the kept reply
  down at `Continue` when a further call fits; otherwise it is dropped with a
  warning and the hooks run again on the kept reply.

`nextCallFits(s, extraInputTokens)` is true when a further non-final call
can happen: it leaves the reserved final-reply call available, the deadline
and context allow it, and every `Budget` cap holds for its worst case (the
latest real request, plus twice what it would add, priced like
`checkSendBudget`; a call in flight counts at its worst case and as used).
Steering's `nextCallPossible` also refuses on a pause; the finish point does
not, since the run pauses before the next call anyway. With the defaults, a
candidate from call 7 cannot continue, since call 8 is the reserved final
reply.

## Recording

- `Step.HookResults []HookResult{Point, Name, Verdict, Pass, RulesVersion,
  Reason, Error}` holds every non-zero or failed decision, on the next committed step of
  any kind. `Pass` counts rewrite restarts. `Verdict`: `allow`, `deny`,
  `needs_approval`, `rewrite` (tool); `deny`, `limit`, `reserve` (send); `change`,
  `stop` (model); `add_messages`, `reject`, `accept_empty` (finish); `failed` (any).
  `Turn.View` checks each against its point.
- The first step of each `Start` and `Continue`, the commands that run hooks,
  records `ActiveHooks`, the `Runner.Hooks` names per point; `WithHooks` extras
  are left out. `WarningHooksChanged` notes a change from the previous one.
- `ModelFact.Reservation` becomes a map by hook name.
- Warnings, buffered by the runtime, ride on the next step of any kind.
  `TurnView.Context(origin, name)` returns a live named item.
