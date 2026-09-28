# PRD: Hooks

**Status:** Approach, revised after design review (decision 31) ·
**Date:** 2026-09-28 · **Owner:** Curtis

## Problem

Dashi's extension point is one `Policy` interface (`BeforeModel`, `BeforeSend`,
`AuthorizeTool`, `OnStop`). Its contract is right: read-only views in, typed
decisions out, recorded. Its packaging is not. "Policy" elsewhere means
allow/deny rules, and developers look for "hooks". There is only one policy, so
budget rules, approvals and reminders are merged by hand. Every error ends the
turn. Nothing runs after a tool, and hooks cannot keep state. The default-allow
half of decision 31 already shipped (#46); this change removes `Policy`.

## Approach

A *hook* is a named function that Dashi calls at one *hook point* of a turn.
Any number of hooks run at each point, in order, and their answers combine by a
fixed rule. Each hook's answer is saved in the turn record with the step it
leads to, and replay reads that record instead of calling hooks again. A hook
point that guards something *fails closed* (a hook error blocks the action);
one that only shapes a request *fails open* (a hook error skips that hook, with
a warning). `Observer` stays separate and read-only; previews are lossy,
committed steps are not (see live-delivery.md).

## API

```go
type Hook[P, D any] struct {
    Name     string // nonempty, unique within its hook point
    Func     func(context.Context, P) (D, error)
    FailOpen bool   // AfterTool only (PR 2)
}
func (h Hook[P, D]) Hooks() Hooks // a Hooks value holding just h

type (
    ModelHook  = Hook[ModelPlan, ModelDecision]
    SendHook   = Hook[SendPlan, SendDecision]
    ToolHook   = Hook[ToolPlan, ToolDecision]
    FinishHook = Hook[FinishPlan, FinishDecision]
)
type Hooks struct {
    BeforeModel  []ModelHook
    BeforeSend   []SendHook
    BeforeTool   []ToolHook
    BeforeFinish []FinishHook
}

func JoinHooks(hs ...Hooks) Hooks // always allocates
func WithHooks(Hooks) RunOption   // runs after Runner.Hooks, this call only
// OnTool decodes a tool's arguments into T. The hook is named after the
// tool; set Name for a second hook on the same tool.
func OnTool[T any](tool string,
    f func(context.Context, ToolPlan, T) (ToolDecision, error)) ToolHook
func Allow(reason string) ToolDecision // recorded; combines as no opinion
func Deny(reason string) ToolDecision
// Defer makes the call wait for approval: the turn stops waiting, and the
// host answers with Pending.Approve or Pending.Deny.
func Defer(reason string) ToolDecision
func Rewrite(args any) (ToolDecision, error)
// RequireApproval defers the named tools, or every application tool when
// none are named. Its hook is named "require_approval:<sorted names>".
func RequireApproval(tools ...string) Hooks
```

The blocking approval (`examples/approval`):

```go
ask := dashi.OnTool("deploy",
    func(_ context.Context, _ dashi.ToolPlan, in deployArgs) (dashi.ToolDecision, error) {
        fmt.Printf("Deploy %s to %s? [y/N] ", in.Version, in.Environment)
        if yes(answers) {
            return dashi.Allow("operator approved"), nil
        }
        return dashi.Deny("The operator declined this deploy."), nil
    })
runner := dashi.Runner{Client: client, Tools: tool.List{deploy}, Hooks: ask.Hooks()}
```

The server form (`examples/durable_approval`): `Hooks:
dashi.RequireApproval("deploy")`.

**Plans and decisions.** A zero decision means *no opinion*.

- `ModelPlan{Input, Turn}` and `SendPlan{Request, Turn, AtRisk, Remaining}`
  are unchanged. `ToolPlan{Call, Definition, Turn}`.
  `FinishPlan{Turn, AttemptID, Answer, Output, OutputErr}`: on a structured
  turn, `Output` is the parsed value or `OutputErr` says why parsing failed.
- `ModelDecision{Settings, Deliver, Stop}`.
- `SendDecision{Deny, Reason, Limit, Admission}` replaces
  `ModelAuthorization`, whose zero value denied. A `Limit` implies `Deny`.
- `ToolDecision{Kind, Reason, Revision, Rewrite}`: a hook's answer.
- `FinishDecision{Deliver, AcceptEmpty, Reason}`. A nonempty `Deliver` asks
  the model again with those items. `AcceptEmpty` accepts an empty answer.
  `Reason` is shown to the model when a submission is not accepted.
- `ToolVerdict{Kind, Reason, Hook, Revision}`: the combined result for a call,
  recorded as `ToolFact.Verdict` and shown as `ToolView.Verdict`. For allow, it
  takes the name and reason of the first hook that returned `Allow`; if none
  did, `Hook` is empty and the reason is `no hook opinion`.

Removed: `Runner.Policy`, `Policy`, `AllowLocalPolicy`, `ModelAuthorization`,
`StopDecision`, `ApprovalFact.PolicyRevision`.

## Hook points

Hooks run one at a time: `Runner.Hooks`, then `WithHooks`, each in list order.

| Point | Combining rule | Hook error or invalid result |
|---|---|---|
| `BeforeModel` | Each hook sees `Input.Settings` as changed by earlier hooks; non-nil `Settings` replaces the struct; `Deliver` appends; the first `Stop` ends the chain, and no deliveries are committed | Fails open: `WarningHookFailed`, and that hook's answer is dropped |
| `BeforeSend` | The first `Deny` stops the turn: `StopRefused`, or `StopLimited` with `Limit`. Each `Admission` is kept under its hook's name | `StopFailed`, reason `hook_failed` |
| `BeforeTool` | The first deny ends the chain; otherwise defer beats allow. A `Rewrite` is schema-checked and restarts the chain | The call is denied; the model sees "Denied: check `<name>` failed."; `WarningHookFailed` |
| `BeforeFinish` | Deliveries from all hooks append; `AcceptEmpty` if any hook says so | `StopFailed`, reason `hook_failed` |

- **Ended context.** When the run's context has ended, a hook's error or panic
  gives the normal context stop (interrupted, or canceled under
  `OnContextDone`), at every point. Nothing is recorded for that hook.
- **Panics** are recovered as hook errors.
- **`hook_failed`** sits beside `tool_fatal`: fix the cause, then `Continue`.
  A failed stop does not count toward `Interruptions`, so hosts that resume
  interrupted turns automatically do not loop on it. After a `BeforeFinish`
  failure, `Continue` runs the finish point again on the recorded answer,
  without another model call. A `hook_failed` stop is never terminal, so the
  record still accepts this retry.
- **Validation.** `Run` rejects, before recording anything: an empty or
  duplicate name within a point, a nil `Func`, and `FailOpen` outside
  `AfterTool`. Docs say to prefix names in reusable packages.
- **Stops.** A `BeforeModel` hook may return `refused`, `limited` (with a
  valid `Limit`) or `failed`; any other kind is an invalid result. The runner
  sets `Stop.Terminal` (live delivery), never the hook.
- **Admission** from all hooks is limited to 16 KiB in total; the hook that
  exceeds it is an invalid result.
- **Rewrites.** A rewrite equal to the current arguments by JSON meaning
  (decision 27) is no opinion. Rewrites must converge: after 3 restarts the
  call is denied with reason `rewrite limit` and `WarningHookFailed`, and the
  turn continues. `Call.RequestedArgs` keeps the model's arguments.
- **Tools.** Arguments are schema-checked before `BeforeTool` (already true).
  Invalid, unknown and `dashi_submit_output` calls skip hooks. `OnTool`
  decode failures are hook errors.
- **Approvals.** The call ID and the argument digest bind an approval to the
  exact call. Dashi does not re-run hooks at `Settle`. A host whose rules
  changed can compare `ToolView.Verdict.Revision` with its current rules
  before calling `Approve`.
- **Delivered items** from hooks pass the same `validateContext` check as any
  delivered context before they are recorded.
- **Configuration.** Like tools and the client, the host supplies the same
  hooks on every command for a turn. `WithHooks` is for extras such as a CLI
  prompt.

## The finish point (PR 1b)

A *candidate* is a model answer that would end the turn: final text, or a
schema-valid `dashi_submit_output` call. Both paths go through one function,
in this order: steering's check (design/steering.md), then `BeforeFinish`,
then the empty-answer and schema checks, unless a hook delivered items.

- **Text path.** Deliveries are recorded in a `StepLoopContinued`, which
  already means the answer was not accepted. No new record field is needed.
- **Submission path.** The finish point runs after the submission's
  `tool_started` step. If a hook delivers, the submission settles as failed
  with the output "Not accepted: `<Reason>`", and the items are recorded before
  the next model call. Steering uses the same function with its own text. The
  submission never succeeded, so replay needs no new rule.
- **No further call.** Deliveries are dropped with a warning unless
  `nextCallPossible` holds; `AcceptEmpty` still applies.

`nextCallPossible(state)` (in `state.go`, next to the `Stop.Terminal` rule) is
true when one more model call still leaves the reserved final-answer call
available, the budget is not spent, the deadline has not passed, no pause is
requested, and the context is live. With the defaults (8 attempts, reserve on),
call 8 is the reserved final answer:

| Candidate from call | Next call | Possible? |
|---|---|---|
| 1 to 6 | 2 to 7, a normal call | Yes |
| 7 | 8, the reserved final answer | No |
| 8 | none | No |

With `NoReservedFinalAnswer`, a candidate from calls 1 to 7 may continue.

## Recording

- `Step.HookResults []HookResult{Point, Name, Verdict, Pass, Revision, Reason,
  Error}` holds every non-zero or failed answer, attached to the next
  committed step, whatever its kind. `Pass` counts rewrite restarts, so an
  audit shows who rewrote what. `Verdict` values: `allow`, `deny`, `defer`,
  `rewrite` (tool); `deny`, `limit`, `admit` (send); `change`, `stop`
  (model); `deliver`, `accept_empty` (finish); `failed` (any).
- Each step that carries a command ID records `ActiveHooks`, the configured
  names per point. `WarningHooksChanged` is recorded when they differ from
  the previous command. A tool hook leaves a positive record with `Allow`.
- `ModelFact.Admission` becomes `map[string]json.RawMessage` keyed by hook
  name. Recorders reserve each entry atomically.
- Warnings may ride on any step kind; the runtime buffers them until the next
  commit. New codes: `WarningHookFailed`, `WarningHooksChanged`.
- `TurnView.Context(origin, name)` returns the live named item, so a
  `BeforeModel` injector can skip an item already current.

## Guarantees

- **Recorded.** Every answer is saved with the step it leads to. After a crash
  before that commit, recovery may call a hook again, so hooks stay idempotent
  (`BeforeModel` runs on every loop pass).
- **Complete.** Every model call passes `BeforeModel` and `BeforeSend`,
  including the reserved final answer. Every eligible client call passes
  `BeforeTool`, streamed or not, and after resume.
- **Replay** reads recorded answers and never calls hooks.
- **Authorized on final values.** Hooks authorize the arguments that run.

## Later: PR 2

`AfterTool` (hooks run on each tool or host result, may replace what the model
sees in `ToolFact.Presented`, fail closed unless `FailOpen`), with
`ToolResultPlan`/`ToolResultDecision`; `ModelDecision.Hide` (remove tools
from one call); hook state saved per name and read with
`TurnView.HookState`; `ToolDecision.Stop`; `tool.Join`; recipe examples
(context injector, result redactor, test-until-green, plan mode). Revisit
cross-point name sharing when state lands.

## Implementation plan

- **PR 1a** replaces `Policy`: types, validation, `WithHooks`, `OnTool`, the
  helpers, `BeforeModel`, `BeforeSend`, `BeforeTool`, `ToolVerdict`,
  `HookResults`, `ActiveHooks`, `TurnView.Context`, failure and context
  rules, and `BeforeFinish` on the text path only, where `OnStop` runs today.
  Steering seams 1 (model-path order), 5 (warnings on any step), 6 (one
  `validateContext`) and 7 (a point before each call's `BeforeTool` chain).
- **PR 1b** adds the finish point on both paths, the shared submission
  rejection, `nextCallPossible`, and the finish retry after `hook_failed`:
  steering seams 2, 3 and 4.
- **Files.** The `Policy` parts of `policy.go` move to a new `hooks.go`;
  `Event` and `Observer` stay put until live delivery's first PR moves them
  to `observer.go`, and this branch rebases onto it. `runner.go`,
  `tools_loop.go` (error strings lose "policy"), `turn.go`, `state.go`,
  `outcome.go`; `internal/providertest/live_agent.go`; provider doc comments
  that say "needs no Policy"; `examples/approval`, `durable_approval`,
  `tools`; `docs/guides/running-turns.md`, `tools.md`, `docs/agent-turns.md`;
  `design/agent-loop.md`, `ubiquitous-language.md`.
- **Tests.** Every combining rule and failure mode per point, panics, and the
  ended-context rule; a normalizing rewrite hook; the rewrite limit;
  validation; `OnTool` decode failures; `JoinHooks` and concurrent
  `WithHooks` without aliasing; `RequireApproval` naming and no-argument
  form; `ActiveHooks` changes; approvals after the revision check is removed.
  A hook-call-order suite in the root package covers streamed and collected
  calls, crash resume, deferred approval, and the reserved final answer.
  About 20 test policies port to hooks. Guardrail: turns without hooks record
  the same steps as today, apart from the verdict shape.

## Risks

| Risk | Mitigation |
| --- | --- |
| Default allow runs something unexpected | The app bound every tool; `RequireApproval()` gates all |
| A fail-open `BeforeModel` hook skips a check | Checks that must hold go in `BeforeSend` or `BeforeTool`; documented |
| Two packages pick the same hook name | Rejected within a point; prefixing documented |
| A hook that always fails | `hook_failed` needs an explicit `Continue`; the warning names the hook |
