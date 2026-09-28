# PRD: Hooks

**Status:** Draft, revised after four reviews · **Date:** 2026-09-28 ·
**Owner:** Curtis

## Problem

Kernel's extension point is a single `Policy` interface with four methods
(`BeforeModel`, `BeforeSend`, `AuthorizeTool`, `OnStop`), plus a watch-only
`Observer`. Its contract is right: each method gets a read-only view and returns
a typed decision that Kernel records. Its packaging is not.

- **The name misleads.** Elsewhere "policy" means allow/deny/ask rules (Codex,
  Gemini's policy engine, OpenHands). Developers look for "hooks" (Claude Code,
  Pi, Gemini CLI, Codex, Strands, Pydantic AI).
- **Only one policy,** so budget rules, approvals and reminders must be merged
  by hand.
- **Every error ends the turn,** including invalid rewrite JSON and a flaky
  context hook.
- **Missing capabilities.** Nothing runs after a tool, no hook can hide tools
  for a call (Pi's plan mode), and hooks cannot keep state of their own.
- **A nil `Policy` denies all tools,** breaking the zero-value rule.

Most hook bugs elsewhere come from mutable, wrapped state or from hooks that
skip some paths (claude-code #83353, ADK #1515, Pi #9773, Dive DIVE-11).

## Approach: Pi's hooks, fitted to Kernel

We adopt from Pi's harness:

- named hooks with one input and one result type each;
- passive events kept separate (`Observer` stays lossy and read-only);
- a documented combining rule and durability per hook;
- gatekeeping hooks fail closed, shaping hooks fail open;
- hook-owned state (Pi's `appendEntry`).

We adapt five things:

1. **One durability class.** A hook's result is saved with the step it enables.
   There is no request-only transform (Pi's `transform_context`), because the
   record must rebuild the exact model request.
2. **An immutable `Hooks` value instead of `on()`/unsubscribe.** One `Runner`
   serves many turns, and Pi's process-wide registration caused cross-session
   conflicts (D#1546).
3. **Recorded names.** Each step records which hooks decided and why.
4. **Stricter rewrites.** Every hook re-checks rewritten arguments; in Pi a
   later rewrite can bypass earlier checks.
5. **`AfterTool` and `BeforeFinish` fail closed by default,** because they are
   the redaction and guardrail points. Pi's `after_response` is dropped;
   final-text guardrails use `BeforeFinish`.

## Goals

1. One concept, hooks, for every point where app code decides or shapes a turn.
2. Any number of hooks per point, combined by a pure, documented rule.
3. Every hook result, including hook state, is recorded with its step and names
   its hook.
4. A hook error, panic or invalid result never leaves a turn unrecoverable.
5. Every model call and every eligible client tool call passes through the
   hooks, proven by a conformance test.
6. Everyday hooks are as short as Pi's; an empty `Hooks` works.

## Non-goals

- Middleware wrapping `next`; request-only transforms; raw payload edits.
  Headers and metadata belong at the model client, not in hooks.
- Hooks for provider-hosted tools, which run at the provider. Answering hosted
  approval requests from `BeforeTool` is future work.
- Deferred to later designs: a once-per-run hook (`TurnView.Context` covers the
  common case), dropping old tool outputs from projection (with compaction),
  replacing the final answer, and `BeforeCompact`.
- Redacting stored records. `AfterTool` controls what the model sees.

## API

```go
type Hook[P, D any] struct {
    Name     string
    Func     func(context.Context, P) (D, error)
    FailOpen bool // AfterTool, BeforeFinish: on error, proceed and warn
}

type (
    ModelHook  = Hook[ModelPlan, ModelDecision]
    SendHook   = Hook[SendPlan, SendDecision]
    ToolHook   = Hook[ToolPlan, ToolDecision]
    ResultHook = Hook[ToolResultView, ToolResultDecision]
    FinishHook = Hook[FinishPlan, FinishDecision]
)

type Hooks struct {
    BeforeModel  []ModelHook
    BeforeSend   []SendHook
    BeforeTool   []ToolHook
    AfterTool    []ResultHook
    BeforeFinish []FinishHook
}

// JoinHooks always allocates new slices. A name collision between two
// values is kept in the result and reported by Run.
func JoinHooks(hs ...Hooks) Hooks

// OnTool decodes a named tool's validated arguments into T. A decode
// failure is a hook error, so the call is denied.
func OnTool[T any](name, tool string,
    f func(context.Context, ToolPlan, T) (ToolDecision, error)) ToolHook
func Deny(reason string) ToolDecision
func Defer(reason string) ToolDecision
func Rewrite(args any) (ToolDecision, error)
```

A permission gate:

```go
type bashArgs struct{ Command string }

gate := kernel.OnTool("gate", "bash",
    func(_ context.Context, _ kernel.ToolPlan, in bashArgs) (kernel.ToolDecision, error) {
        if dangerous.MatchString(in.Command) {
            return kernel.Defer("dangerous command"), nil
        }
        return kernel.ToolDecision{}, nil
    })
runner.Hooks = kernel.Hooks{BeforeTool: []kernel.ToolHook{gate}}
```

- FR-1: `Runner.Hooks` replaces `Runner.Policy`. Like tools and the client, it
  is configuration the host must supply unchanged on every command for a turn
  (`Run` with `Start`, `Continue`, `Settle`), because `AfterTool` and resume
  depend on it. `WithHooks(Hooks)` adds hooks for one command, after the
  runner's, for extras such as a CLI prompt, never a required redactor.
  Kernel never appends into caller slices. Names are nonempty and unique
  within each hook point. A name may repeat across points within one `Hooks` value, which shares its state (FR-5), but
  `JoinHooks` and `WithHooks` reject a name that appears in two different
  values, so unrelated hooks cannot collide. Violations make `Run` return an
  error, as does `FailOpen` outside `AfterTool` and `BeforeFinish`.
- FR-2: Plans carry the turn ID, a `TurnView`, and the attempt or call ID.
  - `ToolPlan{Call, Definition}`.
  - `ToolResultView{Call, Definition, Outcome}`, where `Outcome` includes
    earlier hooks' changes.
  - `FinishPlan{Result}`.
- FR-3: Decisions:
  - `ModelDecision{Settings, Deliver, Hide, Stop, State}`.
  - `SendDecision{Deny, Reason, Limit, Admission, State}` (the zero value
    allows).
  - `ToolDecision{Kind, Reason, Revision, Rewrite, Stop, State}`.
  - `ToolResultDecision{Replace *tool.Output, Failed *bool, Stop, State}`.
  - `FinishDecision{Continue, Deliver, AcceptEmpty, State}`.
- FR-4: Each step records `Step.Hooks []HookResult{Point, Name, Verdict,
  Revision, Reason, Error, State}` as provenance; only `ToolDecision` sets
  `Revision`. The combined result stays a typed fact: `ToolFact.Decision`
  records the effective allow, defer or deny after rewrites, and replay reads
  only that. An explicit allow is recorded but combines as no
  opinion. With no opinion at all, the reason is `no hook opinion`.
- FR-5: **Hook state.** `State json.RawMessage` (at most 16 KiB) is saved with
  the step. `TurnView.HookState(name)` returns the latest value for that name,
  shared across hook points so one bundle keeps one state. A hook sees state
  returned earlier in the same pass (for example, `BeforeSend` sees the same
  call's `BeforeModel` state) through a call-local overlay on the `TurnView`,
  whichever step saves it; an unacknowledged step discards it.
  `TurnView.Context(origin, name) (ContextItem, bool)` returns the live named
  item, so an injector can tell whether its snapshot is current.
- FR-6: **Tool visibility.** `ModelDecision.Hide []string` removes application
  tools from that call. Hidden names from all hooks combine. They are recorded
  in `Step.Hooks` and in the intent's tool definitions. `kernel_submit_output`
  cannot be hidden. A call to a hidden tool settles as not run ("tool
  unavailable") without calling hooks.
- FR-7: `ModelFact.Admission` becomes `map[string]json.RawMessage` keyed by hook
  name. Recorders reserve each entry atomically; hooks must not reserve on their
  own.

## Hook points

A zero result means *no opinion*. Hooks run one at a time in registration order.
`AfterTool` runs on the goroutine that drains tool results, even with
`ToolConcurrency > 1`.

| Hook | Combining rule | On error or invalid result |
|---|---|---|
| `BeforeModel` | Each hook sees prior settings and builds on `p.Input.Settings` (non-nil replaces the struct); `Deliver` appends; `Hide` unions; the first `Stop` ends the chain. Deliveries commit as one `StepDelivered` | Warning; that hook is skipped |
| `BeforeSend` | Runs until a denial; the first denial is `StopRefused` (or `StopLimited` with `Limit`) | `StopInterrupted` with the stable reason `hook_failed` (the hook is named in `Step.Hooks`); resumable, and does not count against `Interruptions` |
| `BeforeTool` | The first deny ends the chain; otherwise defer beats allow. A `Rewrite` ignores its `Kind`, is validated, and every hook is asked again (at most 3 rewrites). All-no-opinion allows | Recorded deny; the reason reaches the model and the turn continues |
| `AfterTool` | Each hook sees the presented output so far; `Replace` and `Failed` chain; a `Stop` applies after the batch settles | "Result withheld: hook `<name>` failed" and a warning; with `FailOpen`, keep the output and warn |
| `BeforeFinish` | `Continue` and `AcceptEmpty` are true if any hook says so; `Deliver` appends | The answer is not accepted: `StopInterrupted`, as for `BeforeSend`; with `FailOpen`, a warning and no opinion |

- FR-8: **Validate, then hook.** Arguments are checked against the tool schema
  before `BeforeTool`. Invalid arguments go back to the model as a tool error
  without calling hooks, so hooks always see valid arguments.
  `kernel_submit_output` is exempt from hooks.
- FR-9: `AfterTool` runs once for every outcome whose content came from a tool
  or the host (executed, failed, or supplied through `Settle`). It does not run
  for denied or not-run outcomes. For `Settle`, it runs inside the command
  before the commit. `Outcome` stays the authoritative record of execution
  state and cost. Changes go to a separate `ToolFact.Presented{Output,
  IsError}`, which projection uses when set; `ToolView.Presented()` returns
  what the model sees. A redactor never changes whether a tool ran or its
  cost. `AfterTool` cannot deliver context, which would
  split a model message's tool calls from their results.
- FR-10: `BeforeFinish` replaces `OnStop` and runs at every candidate final
  answer, on both the text and `kernel_submit_output` paths. It runs after the
  steering check (design/steering.md FR-7). A `Continue` supersedes a candidate,
  including a submission, under the same rules as a steer (steering FR-7 and
  FR-9). When no model attempt would remain, `Continue` is ignored with a
  warning; `AcceptEmpty` still applies. After a `hook_failed` stop, `Continue`
  runs `BeforeFinish` again on the recorded candidate, including a successful
  submission, without another model call or attempt.
- FR-11: Approval binding is simplified:
  - A `BeforeTool` hook's `Revision` is audit data in `Step.Hooks`.
  - `ApprovalFact.PolicyRevision` becomes an optional `DecisionRevision`, which
    `Settle` checks only when it is set. The approval stays bound to the
    argument digest, as today.
  - Kernel does not re-run hooks at `Settle`.
- FR-12: Panics are recovered as hook errors. Hooks honor the run's context. A
  CLI may block inside a `BeforeTool` hook on a prompt; a server uses `Defer`
  and `Settle`. The guide shows both.

## Guarantees

- **Recorded.** Every result and state is saved in `Step.Hooks` with the step it
  enables. If a crash happens before that commit, recovery may call the hook
  again. `BeforeModel` runs on every loop pass, so hooks use `HookState` or
  `Context` to stay idempotent.
- **Complete.** Every model call passes through `BeforeModel` and `BeforeSend`,
  including the reserved final answer and future summary or child calls. Every
  client call with valid arguments to a visible application tool passes through
  `BeforeTool`, and `AfterTool` runs per FR-9, whether streamed or not, and
  after resume. Invalid, hidden, and `kernel_submit_output` calls are exempt. A
  `kerneltest` conformance suite checks the order of hook calls on each path.
- **Pure combining.** Replay reads recorded results and never calls hooks.
- **Authorized on final values.** Hooks always authorize the arguments that will
  run.

## Packaging and defaults

A reusable package exports a type with `Hooks() kernel.Hooks` and
`Tools() tool.Toolset`. The app joins them with `JoinHooks` and a new
`tool.Join(...tool.Toolset)`. The two share state through their closures, or
durably through `HookState`. No new "extension" concept is needed.

**Tools work by default.** With no hook opinion, local and external tools are
allowed: binding a tool enables it. This is a deliberate decision, matching Pi
and the zero-value rule. The guide includes a five-line hook that defers any
tool not annotated read-only.

## Internal alignment

- Remove `Policy`, `AllowLocalPolicy` and `denyToolPolicy`. Each call site
  (`runner.go:991, 1081, 1374`; `tools_loop.go:206`) calls one combiner that
  returns the decision and the `HookResult` list.
- `tools_loop.go:199-243` moves schema validation before the hooks. The rewrite
  loop becomes "restart on rewrite", and its `StopFailed` exits become hook
  errors.
- Replay does not count `hook_failed` stops toward `Interruptions`, and
  allows a `BeforeFinish` retry on the recorded candidate.
- Tool definitions are filtered by `Hide` before preparation. The submit-tool
  path gains its `BeforeFinish` call.
- Update the docs, `design/agent-loop.md`, `design/steering.md`, and
  `design/ubiquitous-language.md`. Record the decision in `design/decisions.md`.

## Success criteria

- Ports of Pi's permission gate, context injector, result redactor,
  test-until-green, and plan-mode tool restriction each fit in as few lines as
  Pi's, or fewer, and live in the examples.
- The conformance suite covers each of these paths:
  - streamed and collected calls;
  - resume after a crash;
  - deferred approval and `Settle`;
  - the reserved final answer, with and without `NoReservedFinalAnswer`;
  - both structured-output paths;
  - hidden tools.
- Tests cover every combining rule, every failure mode, rewrite restarts, and
  concurrent `WithHooks` use without aliasing.
- Guardrail: turns with no hooks make the same calls as today, except that tools
  are no longer denied by default.

## Risks

| Risk | Mitigation |
| --- | --- |
| Default allow runs something unexpected | The app bound every tool; release notes; the guide's approval recipe |
| A flaky `AfterTool` hook withholds results | Warning names the hook; `FailOpen` for cosmetic hooks |
| A fail-open `BeforeModel` hook skips a check | Checks that must hold go in `BeforeSend` or `BeforeTool`; documented |
| Hook state grows the record | 16 KiB cap; only saved when a hook returns it |
