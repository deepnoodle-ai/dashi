# PRD: Hooks

**Status:** Approach for implementation (decision 31) · **Date:** 2026-09-28 ·
**Owner:** Curtis

## Problem

Dashi's extension point is one `Policy` interface (`BeforeModel`, `BeforeSend`,
`AuthorizeTool`, `OnStop`) plus the watch-only `Observer`. Its contract is
right: read-only views in, typed decisions out, recorded. Its packaging is not.
"Policy" elsewhere means allow/deny rules, and developers look for "hooks".
There is only one policy, so budget rules, approvals and reminders are merged by
hand. Every error ends the turn. Nothing runs after a tool, no hook can hide
tools, and hooks cannot keep state. Most hook bugs elsewhere come from mutable,
wrapped state or hooks that skip some paths.

The default-allow half of decision 31 already shipped (#46): a nil `Policy`
allows bound tools with revision `default-v1`. This change removes `Policy`
itself.

## Approach

Pi's named hooks, fitted to Dashi: one input and one result type per hook
point; any number of hooks per point in an immutable `Hooks` value; a pure,
documented combining rule; gatekeeping points fail closed and shaping points
fail open; every hook result recorded with the step it enables and never
re-run on replay. Arguments are re-checked by every hook after a rewrite. No
middleware, request-only transforms, raw payload edits, or hooks for
provider-hosted tools.

## API

Before: implement four methods, usually by embedding `AllowLocalPolicy`.

```go
type askPolicy struct{ dashi.AllowLocalPolicy; answers *bufio.Reader }
func (p askPolicy) AuthorizeTool(ctx context.Context, call tool.Call,
    view dashi.TurnView) (dashi.ToolDecision, error) { /* parse call.Args */ }
runner.Policy = askPolicy{answers: in}
```

After:

```go
type Hook[P, D any] struct {
    Name     string // nonempty, unique within its hook point
    Func     func(context.Context, P) (D, error)
    FailOpen bool   // AfterTool and BeforeFinish only
}
type (
    ModelHook  = Hook[ModelPlan, ModelDecision]
    SendHook   = Hook[SendPlan, SendDecision]
    ToolHook   = Hook[ToolPlan, ToolDecision]
    ResultHook = Hook[ToolResultView, ToolResultDecision] // PR 2
    FinishHook = Hook[FinishPlan, FinishDecision]
)
type Hooks struct {
    BeforeModel  []ModelHook
    BeforeSend   []SendHook
    BeforeTool   []ToolHook
    AfterTool    []ResultHook // PR 2
    BeforeFinish []FinishHook
}

func JoinHooks(hs ...Hooks) Hooks // always allocates
func WithHooks(Hooks) RunOption   // runs after Runner.Hooks, this call only
func OnTool[T any](name, tool string,
    f func(context.Context, ToolPlan, T) (ToolDecision, error)) ToolHook
func Allow(reason string) ToolDecision  // recorded; combines as no opinion
func Deny(reason string) ToolDecision
func Defer(reason string) ToolDecision
func Rewrite(args any) (ToolDecision, error)
func RequireApproval(tools ...string) ToolHook
```

The blocking CLI approval (`examples/approval`):

```go
ask := dashi.OnTool("ask", "deploy",
    func(_ context.Context, _ dashi.ToolPlan, in deployArgs) (dashi.ToolDecision, error) {
        fmt.Printf("Deploy %s to %s? [y/N] ", in.Version, in.Environment)
        if yes(answers) {
            return dashi.Allow("operator approved"), nil
        }
        return dashi.Deny("The operator declined this deploy."), nil
    })
runner.Hooks = dashi.Hooks{BeforeTool: []dashi.ToolHook{ask}}
```

The server form (`examples/durable_approval`) changes one line:
`Hooks: dashi.Hooks{BeforeTool: []dashi.ToolHook{dashi.RequireApproval("deploy")}}`.

**Plans and decisions.** A zero decision is *no opinion*.

- `ModelPlan{Input, Turn}` and `SendPlan{Request, Turn, AtRisk, Remaining}`:
  unchanged.
- `ModelDecision{Settings, Deliver, Stop, Hide, State}`.
- `SendDecision{Deny, Reason, Limit, Admission, State}`: replaces
  `ModelAuthorization`, whose zero value denied. A `Limit` implies `Deny`.
- `ToolPlan{Call, Definition, Turn}`.
- `ToolDecision{Kind, Reason, Revision, Rewrite, Stop, State}`: a hook's
  answer, never recorded as a whole.
- `ToolVerdict{Kind, Reason, Hook, Revision}`: the combined result, recorded in
  `ToolFact.Decision` and shown as `ToolView.Decision`.
- `FinishPlan{Turn, AttemptID, CallID, Answer, Output}`: the candidate. On a
  structured turn, `Output` is already schema-valid.
- `FinishDecision{Continue, Deliver, AcceptEmpty, State}`: replaces
  `StopDecision`. A nonempty `Deliver` implies `Continue`.
- `ToolResultView{Call, Definition, Outcome, Presented, Turn}` and
  `ToolResultDecision{Replace, Failed, Stop, State}`: PR 2.

Removed: `Runner.Policy`, `Policy`, `AllowLocalPolicy`, `defaultPolicy`,
`ModelAuthorization`, `StopDecision`.

## Hook points

Hooks at a point run one at a time in registration order (`Runner.Hooks`,
then `WithHooks`). Panics are recovered as errors. Hooks honor the run's
context.

| Hook | Combining rule | On error or invalid result |
|---|---|---|
| `BeforeModel` | Each hook sees `Input.Settings` as changed by earlier hooks; non-nil `Settings` replaces the struct; `Deliver` appends; `Hide` unions; the first `Stop` ends the chain. Deliveries commit as one `StepDelivered` | `WarningHookFailed`; that hook is skipped |
| `BeforeSend` | The first `Deny` is `StopRefused`, or `StopLimited` with `Limit`. Each hook's `Admission` is kept under its name | `StopInterrupted`, reason `hook_failed`; resumable, not counted in `Interruptions` |
| `BeforeTool` | The first deny ends the chain; otherwise defer beats allow. A `Rewrite` ignores `Kind`, is schema-validated, and restarts the chain (at most 3). All no opinion allows, reason `no hook opinion` | The call is denied. The model sees "Denied: check `<name>` failed."; the error text stays in `Step.Hooks` |
| `AfterTool` (PR 2) | `Replace` and `Failed` chain; a `Stop` applies after the batch settles | "Result withheld: hook `<name>` failed"; with `FailOpen`, keep the output and warn |
| `BeforeFinish` | `Continue` and `AcceptEmpty` if any hook says so; `Deliver` appends | As `BeforeSend`; with `FailOpen`, a warning and no opinion |

- **Validation.** `Run` rejects, before recording anything: an empty or
  duplicate name within one point, a nil `Func`, and `FailOpen` outside
  `AfterTool`/`BeforeFinish`. The same name at different points is one bundle
  and shares state (PR 2). Nothing detects unrelated packages picking the same
  name; the docs say to prefix names.
- **Tools.** Arguments are schema-checked before `BeforeTool` (already true
  today); invalid, unknown, hidden and `dashi_submit_output` calls skip hooks.
  A `Defer` no longer requires a revision. A decode failure in `OnTool` is a
  hook error, so the call is denied.
- **Finish.** `BeforeFinish` replaces `OnStop` and runs at every candidate final
  answer: the final-text path (after the structured-output parse) and the
  successful-submission path (before `StepOutputRecorded`). `Continue`
  records a `StepLoopContinued` whose new `Supersedes` field names the
  candidate (attempt ID or submission call ID); replay then accepts later
  model calls and picks the latest unsuperseded submission. When no further
  model call is possible (reserved final answer, attempts, budget, deadline),
  `Continue` is ignored with a warning; `AcceptEmpty` still applies. After a
  `hook_failed` stop, `Continue` re-runs `BeforeFinish` on the recorded
  candidate without another model call.
- **Approvals.** `ApprovalFact.PolicyRevision` becomes an optional
  `DecisionRevision`, checked by `Settle` only when set; the argument digest
  stays mandatory. `Pending.Approve` copies it from `ToolView.Decision`.
  Dashi does not re-run hooks at `Settle`. `RequireApproval` is a `BeforeTool`
  hook named `require_approval` that defers the named tools with revision
  `require-approval:<sorted names>`.
- **Configuration.** Like tools and the client, the host supplies the same
  `Runner.Hooks` on every command for a turn. `WithHooks` is for extras such as
  a CLI prompt, never a required redactor. Dashi never appends into caller
  slices.
- **PR 2 items.** `AfterTool` runs once per outcome whose content came from a
  tool or the host (including `Settle`, inside that command), on the goroutine
  that drains results. Changes go to `ToolFact.Presented{Output, IsError}`,
  used by projection; `Outcome` stays authoritative for state and cost.
  `ModelDecision.Hide` removes application tools from one call (never
  `dashi_submit_output`); a call to a hidden tool settles as "tool
  unavailable". `State json.RawMessage` (at most 16 KiB) is saved in
  `Step.Hooks`; `TurnView.HookState(name)` returns the latest value, with a
  call-local overlay so `BeforeSend` sees the same pass's `BeforeModel` state.
  `TurnView.Context(origin, name)` returns the live named item.

**Packaging.** A reusable package exports `Hooks() dashi.Hooks` and
`Tools() tool.Toolset`; the app joins them with `JoinHooks` and a new
`tool.Join` (PR 2). No "extension" concept is needed. The guide shows a
five-line hook that defers any tool not annotated read-only.

## Recording and replay

- `Step.Hooks []HookResult{Point, Name, Verdict, Revision, Reason, Error,
  State}` holds the non-zero and failed results. Results ride on the next
  committed step: `StepDelivered` or `StepModelIntent` for `BeforeModel` and
  `BeforeSend`, `StepToolDecision` for `BeforeTool`, `StepLoopContinued`,
  `StepOutputRecorded` or `StepStopped` for `BeforeFinish`. Warnings use the
  same rule. A turn with no hooks records no hook results.
- `ToolFact.Decision` becomes `ToolVerdict`. `Hook` names the deciding hook;
  `Rewrite` leaves the record (`Call.RequestedArgs` keeps the original).
- `ModelFact.Admission` becomes `map[string]json.RawMessage` keyed by hook
  name, 16 KiB total. Recorders reserve each entry atomically.
- New: `WarningHookFailed`, the stop reason `hook_failed` (a constant), and
  `Step.Supersedes`. Replay does not count a `Continue` after a `hook_failed`
  stop toward `Interruptions`.
- Removed: revision `default-v1`. Old records need no reading (no users).

## Guarantees

- **Recorded.** Every result is saved with the step it enables. After a crash
  before that commit, recovery may call a hook again, so hooks stay
  idempotent (`BeforeModel` runs on every loop pass).
- **Complete.** Every model call passes `BeforeModel` and `BeforeSend`,
  including the reserved final answer. Every eligible client call passes
  `BeforeTool`, streamed or not, and after resume.
- **Pure combining.** Replay reads recorded results and never calls hooks.
- **Authorized on final values.** Hooks authorize the arguments that will run.

## Implementation plan

**Split.** PR 1 (this branch) replaces `Policy`: the `Hooks` type, combiners,
validation, `WithHooks`, `OnTool` and helpers, `BeforeModel` (settings,
deliver, stop), `BeforeSend` with named admission, `BeforeTool`,
`BeforeFinish` on both paths with `Supersedes`, `Step.Hooks`, failure modes,
`DecisionRevision`, and the examples and guides. PR 2: `AfterTool` and
`Presented`, `Hide`, hook state, `TurnView.Context`, and
`ToolDecision.Stop`. PR 1 must include `BeforeFinish`, since removing `OnStop`
needs it and steering depends on it.

**Files.** `policy.go` becomes `hooks.go`; `runner.go` (model, send, classify,
the submission path, `WithHooks`); `tools_loop.go` (the tool chain); `turn.go`,
`state.go` (formats, replay rules); `outcome.go` (`RequireApproval`,
`Pending`); `internal/providertest/live_agent.go`; provider doc comments
that say "needs no Policy"; `examples/approval`, `durable_approval`, `tools`;
`docs/guides/running-turns.md`, `tools.md`, `docs/agent-turns.md`;
`design/agent-loop.md`, `ubiquitous-language.md`, `decisions.md` if the
decision text changes.

**Tests.** Every combining rule and failure mode per point, including panics;
the rewrite restart and limit; validation errors; `OnTool` decode failures;
`JoinHooks` and concurrent `WithHooks` without aliasing; approvals with and
without `DecisionRevision`; `BeforeFinish` on text and submission paths,
`Supersedes`, the ignored-`Continue` case, and `hook_failed` resume without a
model call or an interruption count; a hook-call-order suite over streamed and
collected calls, crash resume, deferred approval, and the reserved final
answer. It lives in the root package's tests, not `dashitest`, because it
tests the runner, not a host's recorder. The ~20 test policies port to
hooks. Guardrail: turns with no hooks record the same steps as today, apart
from the verdict shape. PR 2 adds short examples of a context injector, a
result redactor, test-until-green, and plan-mode tool hiding, each as short
as Pi's.

**What steering needs.** Steering (design/steering.md) builds on PR 1:

- `model()` has one place, before the `BeforeModel` chain, where steering
  commits its `StepDelivered`; the plan's input and view are built after it,
  so hooks see steers in `Input.Messages` (FR-6).
- A single candidate-finish function serves both final-answer paths. Steering
  inserts its check before the `BeforeFinish` chain and reuses
  `StepLoopContinued.Supersedes` and the replay rule for superseded
  submissions (FR-7).
- A shared `nextCallPossible` check serves ignored `Continue` and steering
  FR-9.
- Pending warnings ride on the next committed step, as steering FR-12 needs.

**Changes from the reviewed draft.**

- `JoinHooks` no longer detects one name in two values: it cannot tell one
  bundle's hooks at two points from two packages' hooks.
- A failed `BeforeTool` hook's error text is not shown to the model, since it
  may hold internal details.
- `ToolVerdict` splits the recorded result from the hook's answer.
- `Allow(reason)` is added for recorded approvals.
- `Deliver` implies `Continue`.
- The call-order suite is internal (see Tests).
- Schema-before-hooks already shipped.

## Risks

| Risk | Mitigation |
| --- | --- |
| Default allow runs something unexpected | The app bound every tool; `RequireApproval`; the guide's read-only recipe |
| A fail-open `BeforeModel` hook skips a check | Checks that must hold go in `BeforeSend` or `BeforeTool`; documented |
| Two packages pick the same hook name | Rejected within a point; documented prefixing across points |
| A flaky `AfterTool` withholds results (PR 2) | The warning names the hook; `FailOpen` for cosmetic hooks |
