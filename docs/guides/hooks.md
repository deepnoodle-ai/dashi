# Hooks

A hook is a named function that Dashi calls at one point of a turn. This guide
covers the hook points, hiding tools, changing results, and vetoing replies;
[approving tool calls](approvals.md) builds on it, and
[running turns](running-turns.md) covers the basics.

## Hook points

Set hooks on `Runner.Hooks`; any number run at each point, in order:

| Point | Runs | Can | If it fails |
|---|---|---|---|
| `BeforeModel` | before each model request is prepared | change settings, add messages, hide tools, stop | skipped, with a warning |
| `BeforeSend` | on each prepared request, with its worst-case cost | deny, report a limit, record reservation terms | the turn stops failed |
| `BeforeTool` | on each client tool call with valid arguments | allow, deny, hold for approval, rewrite the arguments | the call is denied, with a warning; with `FailOpen`, no opinion |
| `AfterTool` | on each final result from a tool, or from the host through `Resolve` | replace what the model sees | the result is withheld from the model, with a warning |
| `BeforeFinish` | on each final reply: final text, or output submitted with `dashi_submit_output` | ask the model to continue, turn a submission down, accept an empty reply | the turn stops failed |

A zero decision means no opinion. Each hook's decision is saved in the turn
record with the step it leads to (`Step.HookResults`), and replay never calls
hooks.
A failed stop has the reason `dashi.StopReasonHookFailed`: fix the cause, then
`Continue` resumes where the turn stopped; for `BeforeFinish` it asks the hooks
again about the same reply, without another model call, unless you added
messages in between, which the model then sees. If the run's context ends while
a hook runs, the turn stops as it would for any ended context.

A tool hook that returns `dashi.Allow(reason)` leaves an audit record naming
it; one with no opinion leaves none. `ToolDecision`, `ModelDecision`, and
`ResultDecision` each carry a `Reason` and a `RulesVersion`, recorded in the
hook's `HookResult` with any answer other than no opinion: say why, and name
the policy or decision model version that decided. A `BeforeModel` decision
that changes nothing but carries a `Reason` or `RulesVersion` is recorded as
`VerdictNoChange`, so `HookActed` tells a retried call that the hook already
ran. Only a `BeforeTool`
denial's reason reaches the model. When a `BeforeTool` hook fails, the model
sees its name, so keep private details out of hook names. A hook that stops
the turn with a cost or token limit resumes like the turn's own caps, with a
`Continue` that raises the `Budget`; a limit the host owns, such as prepaid
credits, uses `dashi.LimitHost`. See [turn budgets](budgets.md) for raises
and host limits.

## Hiding tools

A `BeforeModel` hook's `Hide` removes tools from one call, such as a plan mode
that offers no write tools until the host approves the plan:

```go
plan := dashi.ModelHook{Name: "plan", Func: func(
	context.Context, dashi.ModelPlan) (dashi.ModelDecision, error) {
	if approved.Load() {
		return dashi.ModelDecision{}, nil
	}
	return dashi.ModelDecision{Hide: []string{"write_file", "bash"}}, nil
}}
```

Names must match exactly; a name no tool of this call has, including a tool a
dynamic toolset left out this time, is ignored with a `WarningHideIgnored`, and
`dashi_submit_output` cannot be hidden. A hide wins over a forced tool choice
that names a tool any hook hid: the choice is cleared, with a
`WarningToolChoiceCleared`. If no tools are left, the tool choice and
parallel-tools setting are cleared too. The hidden names are recorded in the
`ModelFact.Hidden` of the call's `model_call_started` step, and a call to a
hidden tool is not run and reads "This tool is not available now."

A hide shapes the offer only: a failing hook is skipped and offers the tools
again. To fail closed, handle errors inside the hook and return the hide, or
pair it with a `BeforeTool` deny. A hidden call never reaches `BeforeTool`, so
the deny acts only when a failed hook offered the tool again. Changing the
offered tools resets the whole cached prompt, not only the tools, so keep hides
stable. A package can export `Hooks()` and `Tools()`; join them with
`dashi.JoinHooks` and `tool.Join`, which fails on a duplicate tool name.

## Changing results

An `AfterTool` hook changes only what the model sees of a result: the next model
call, and the next turn's history through `dashi.TurnMessagesFull`. The record,
`Observer` and `Stream` events, and `ToolView.Outcome` keep the raw output, so
it is not storage redaction; keep a secret out of the record in the tool itself
([example](../../examples/redact/main.go)). `ToolView.Result()` shows what the
model saw. `Output.MapText` rewrites text blocks only; a strict redactor returns
an error, withholding the result, when it finds a block it cannot inspect, such
as a document or a resource. A failing result hook withholds that result,
telling the model the tool ran; set `FailOpen` on a hook that only improves
results to keep the result instead. `FailOpen` also works on `BeforeFinish`
and `BeforeTool`, where an error then counts as no opinion and still records a
warning. Use it on `BeforeTool` only for advice, such as a decision-model
check, never for a permission policy: a broken policy would let every call
through. Pass the same hooks to a `Resolve`
that reports a tool result, since it runs `AfterTool`.

The result limit (`Limits.ToolResultBytes`) runs after the `AfterTool` chain.
A hook sees the full text, and the limit shortens what the hooks leave. A hook
cannot bring back text that the limit removed. `ToolView.Shortened()` counts
bytes after the hooks ran. See [tools](tools.md#large-results).

## Hook sets

Supply the same `Runner.Hooks` on every command for a turn: each command runs
only the hooks it is given, and a change is recorded as a warning.
`dashi.WithHooks` adds hooks for one call, for extras such as a terminal
prompt. Hook names must be unique within a point; reusable packages should
prefix theirs. `dashi.JoinHooks` combines hook sets: anything with a `Hooks()`
method, including a single hook and a `Hooks` value, as in
`dashi.JoinHooks(onCall, guard, runner.Hooks)`. For a hook that holds every
tool not marked read-only for approval, see
[holding calls for later approval](approvals.md#holding-calls-for-later-approval).
For advisory model review, use a separately recorded turn and required host
approval. See [using a model's advice](approvals.md#using-a-models-advice).

## Decision-model checks

Package `decide/toolcheck` gives two ready-made checks that ask a decision
model, through [Decide](https://github.com/deepnoodle-ai/decide), for a
second opinion. Both zero values work, with `TYPESAFE_API_KEY` from the
environment:

```go
runner.Hooks = dashi.JoinHooks(
	toolcheck.CommandRisk{},     // holds a likely disaster for approval
	toolcheck.PromptInjection{}, // warns the model about a likely attack
	policy,
)
```

- `CommandRisk` reads the `command` argument of `bash` (`ShellTools` names
  other shells) and holds a command that could likely cause severe harm,
  such as `rm -rf ~` or a force push to `main`, as a `PendingApproval` named
  `toolcheck-command-risk`. It checks every shell call, because a hook does
  not see other hooks' answers, so add it where no other hook asks about
  each command, such as an agent that runs commands without approval.
- `PromptInjection` reads tool results and puts a warning before one that
  likely tries to take over the agent or hides text from a person. The
  record keeps the raw output. Of a very long result it reads the start
  and end (8 parts of 60 KiB), never skips it, and tells the model to
  treat the whole result as untrusted.

They are advice, not a sandbox: neither ever allows a call. Both fail open
on an outage: with no key, or when Decide is down, the call runs and the
result reaches the model unchanged, with a `hook_failed` warning. When the
command check fails in a way the input can cause (a timeout, a rate limit, a
command too long to check, a rejected request, or an invalid answer), its
hook holds the call for approval instead, with a `Reason` that says why. After an outage (no key,
a failed login, a server error, or a lost connection) a check pauses for a
minute; a timeout, a rate limit, or a rejected input never pauses it, and
the content check never pauses the command check. A host
that asks the user in its own `BeforeTool` hook calls `CommandRisk.Check`
there and shows one prompt; `toolcheck.IsOutage` tells an outage from a
failure the input could cause.

`PromptInjection` does not see everything outside text can reach the model
through. It does not check:

- error results, such as a page that returns 404 with an attack in its body;
- provider-run tools, such as a provider's web search or fetch, which never
  reach `AfterTool`;
- a two-step fetch: `curl -o page.html URL` returns little text, and a later
  `read_file page.html` comes from a closed-world tool;
- shell commands that do not run `gh`, `curl`, `wget`, or `http`, such as
  `cat page.html` on a downloaded file, `python -c 'requests…'`, or
  `git clone`;
- the workspace file tools reading text from outside, such as the README of
  a cloned repository;
- a result whose check timed out or hit the rate limit, which reaches the
  model unchanged with a `hook_failed` warning.

The checked commands and results go to the decision model's provider with
your Decide key. `PromptInjection` reads every result except those of tools whose
open-world hint is false; the workspace file tools and `ask_user` set it.
To keep a trusted tool's results local, set it on that tool:

```go
t.Definition.Annotations.OpenWorld = new(false)
```

## Adding messages and vetoing replies

`BeforeModel` runs before every model call. `dashi.SeedContext(item)` adds a
named item only when its latest visible value differs from the configured
value, or the slot is absent from `p.Input.Messages`. Origin and Name identify
the slot. Empty Origin means user context. Item IDs do not affect the
comparison. This rule holds across `Session` turns and reopened handles.
Changed values append after earlier messages to preserve the prompt prefix.
Restoring A after B appends A again, even if an older A remains visible.

Building the turn's messages keeps each named item's identity in
`llm.Message.Context`. This metadata adds no authority or model-visible
content. Matching ordinary user text cannot suppress a seed. Retained
compaction messages keep their identity, so the item is not added again.
Dropped items return before the next request. Summary text does not count as
retained context. Old compaction messages without identity remain in place and
receive one new identified copy. `SeedContext` requires Name and clears
NextCallOnly.

For the user's request, read `p.Turn.Input`: the turn's start input, then
user items that `AddMessages` or a steer added later, but not items hooks
added. A tool reads the same view with `dashi.CurrentTurn(ctx)`.
For context limited to the current turn, inspect `p.Turn.Context(origin, name)`.
To act once per turn in any hook, ask `p.Turn.HookActed(point, name)`. It reads
recorded hook decisions and holds when another process resumes the turn.
`BeforeModel` may run more than once for one reply, and `BeforeSend` may be
asked again for the same reply, when an [urgent steer](steering.md) sends the
run back before the request is sent. Add a value that must reflect each call
as a named `Snapshot` item: the model sees it after the earlier copies, which
stay in place so the [prompt cache](prompt-caching.md) still matches. Every
replaced copy also stays in the session history, so keep snapshot items small
and change them rarely. Reserve spending in the recorder, with the call's
`model_call_started` step, not in the hook.

`BeforeFinish` vetoes a reply with a `Reason`, which the model sees as "Not
accepted: …" before it replies again, or nudges with `AddMessages` items
alone. A nudge never spends the
[reserved final-reply call](recovering-turns.md#turn-states-and-limits): when
no normal model call is possible (on the reserved call or the one before it,
past the deadline, or when the next call would exceed a cost cap), it is
dropped with a warning and the reply stands. A veto may use the reserved call.
When no call fits at all, or the reserved reply is vetoed too, the turn stops
failed with `dashi.StopReasonOutputRejected` and no reply; `Continue` after
raising the limit lets the model try again. A vetoed reply never shows in
`out.Reply`.
