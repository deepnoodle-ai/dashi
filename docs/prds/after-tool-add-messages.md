# Add context from AfterTool hooks

Status: Draft\
PRD PR: TBD  Implementation PR: TBD  Updated: 2026-11-01

## Problem

A host often wants to tell the model something because of a tool call it
just made, without changing that call's result: "save what you learned to
memory" after a long web search, the guidance for a skill after the model
reads its file, or a warning that a command's output looks like an
injection attempt. `BeforeModel` and `BeforeFinish` hooks can add messages
(`AddMessages`); an `AfterTool` hook can only replace the result.

A host that ported Claude Code `PostToolUse` hooks had to build a side
channel: each `AfterTool` hook pushed items onto a mutex-guarded slice, and
a `BeforeModel` and a `BeforeFinish` hook drained it. The queue lives in
memory, so a crash or a turn that stops to wait for a host result loses
it, and nothing in the record ties an item to the call that caused it.
Every host that wants post-tool context repeats this work.

## Context

- **Claude Code `PostToolUse`.** The hook gets the tool name, input, and
  response, and may return `hookSpecificOutput.additionalContext`. Claude
  Code adds that text for the model as a `<system-reminder>` after the
  tool result; the result itself is unchanged. `decision: "block"` with a
  `reason` also only adds text, since the tool already ran.
- **Other frameworks.** Google ADK's `after_tool_callback` can only replace
  the tool's response. The OpenAI Agents SDK's `on_tool_end` only observes.
  LangChain middleware (`wrap_tool_call`) can return a command that adds
  messages to the graph state.
- **Dashi today.** A `BeforeModel` hook can read finished calls from
  `plan.Turn.Tools` and add items whose IDs come from the call IDs, so each
  item is added once. That works, but the hook must find "new since the
  last call" itself, it runs (and scans every call) before every model
  call, its rules are separate from the `AfterTool` hook that judged the
  result, and it fails open where a result check fails closed. A
  first-class field puts the item on the same record step as the result it
  is about, with no state in the host.
- **Placement already works.** Dashi renders all of a model message's tool
  results right after that message, and renders the items on any later
  step after them. A `tool_finished` step can already carry items: a
  turned-down `dashi_submit_output` call does (`rejectSubmission`). So
  items recorded on a call's `tool_finished` step follow every result of
  its batch.

## Users and job

**Primary:** a Go developer who builds an agent product on Dashi and adds
behavior around tools. Job: when a tool call finishes, add a reminder for
the model's next call, in one hook, with no queue of their own.
**Secondary:** a host that ports Claude Code style hook configuration and
wants `additionalContext` to map to one Dashi field.

## Concepts

No new concepts. `ResultDecision.AddMessages` is the `AddMessages` that
`ModelDecision` and `FinishDecision` already have: a list of
`ContextItem`s that the model sees on its next call.

## Use cases

### UC-1: Nudge the model to save memory after a research tool

```go
nudge := dashi.ResultHook{Name: "memory-nudge", FailOpen: true, Func: func(
	_ context.Context, p dashi.ResultPlan) (dashi.ResultDecision, error) {
	if p.Call.Name != "web_search" || p.Outcome.State != tool.StateSucceeded {
		return dashi.ResultDecision{}, nil
	}
	return dashi.ResultDecision{AddMessages: []dashi.ContextItem{{
		ID:     "memory-nudge", // a fixed ID: at most once per turn
		Origin: dashi.ContextRuntime,
		Message: llm.ReminderMessage(llm.Reminder{
			Name:    "memory",
			Tier:    llm.ReminderContextual,
			Content: "If this search taught you something durable, save it with remember.",
		}),
	}}}, nil
}}
runner.Hooks = nudge.Hooks()
```

- [ ] The next model request has the reminder after the tool results of
      the batch, and the `web_search` result is unchanged.
- [ ] The item is on the call's `tool_finished` step (`Step.Items()`), and
      `Step.HookResults` has `{after_tool, memory-nudge, add_messages}`.
- [ ] A second `web_search` in the same turn adds nothing more: the turn
      already records the ID.
- [ ] `TurnMessagesFull` of the ended turn includes the item after the
      results, so the next turn's history keeps it.

### UC-2: A batch of parallel calls, some resolved later

The model calls `read_file` and `deploy` together. `deploy` returns
`waiting`; the turn stops. Later the host resolves it with `Resolve`.

- [ ] Items added for `read_file` are recorded in the run; items added for
      `deploy` are recorded by the `Resolve` command.
- [ ] No model call happens while `deploy` is unresolved. On `Continue`,
      the request has the assistant message, both results, then the items,
      in the order their `tool_finished` steps were recorded.
- [ ] A provider never receives an item between a tool call and its
      result (a `turnMessages` test over the recorded steps).

### UC-3: Warn and redact in one hook

A hook replaces a result that holds a secret and adds an item telling the
model that the output was redacted.

- [ ] The model sees the replaced result and the item. The record has one
      `HookResult` with `VerdictReplace`.
- [ ] `Outcome.Data` is dropped because the result changed, as today. A
      hook that only adds items keeps `Outcome.Data`.

### UC-4: Failures

- [ ] A later fail-closed hook fails: the result is withheld, no item from
      the chain is recorded, and a `WarningMessagesDropped` names the call.
- [ ] A `FailOpen` hook fails: its own items are dropped; earlier and
      later hooks' items are kept.
- [ ] An invalid item (for example a system role from `ContextUser`) is an
      invalid answer: the hook counts as failed, with `FailOpen` deciding
      the rest, as for an invalid `Replace`.
- [ ] The run context ends during the chain: when the result is withheld,
      no item is recorded; when it is kept (the rest fail open), earlier
      hooks' items are kept.
- [ ] Replaying the turn never calls the hook. A crash before the step is
      committed records neither the result nor the items; the rerun hook
      gives the same IDs.

## Requirements

- R-1: `ResultDecision` gains `AddMessages []ContextItem` with JSON key
  `add_messages`. Its doc comment states placement, IDs, and failure.
- R-2: The items go on the `ContextFact` of the call's `tool_finished`
  step, committed with the result, for executor results and `Resolve`.
- R-3: An item without an ID gets
  `<turn>:tool:<call ID>:hook:<hook name>:<n>`, n counting from 1 within
  the hook's list, mirroring `hookItemID` for `BeforeModel`.
- R-4: Items go through `skipRecordedItems`: an ID the turn already
  records is skipped, and a different item under it warns, as for
  `BeforeFinish`.
- R-5: Item rules are `ContextItem.Validate`, the same as `BeforeModel`.
- R-6: Turn validation and replay accept items on any `tool_finished`
  step, not only a turned-down submission's. Steers stay limited to
  submissions.
- R-7: `docs/guides/hooks.md`, the `Hooks` combining rules, and
  `CHANGELOG.md` describe the field.

## Not in scope

- Adding items for calls with no `AfterTool` run: denied, invalid,
  unknown, or submitted calls. A `BeforeTool` denial already sends the
  model its reason.
- A size limit on added items. `Limits.ToolResultBytes` covers the result
  only; `BeforeModel` items have no limit either.
- Showing earlier hooks' items to later hooks. Hooks at one point stay
  independent; IDs remove duplicates.
- Items after a turn ends. An ended turn refuses tool results, so no
  `AfterTool` runs.

## Decisions

1. **Placement is a rendering rule, not a timing rule.** Items ride on the
   `tool_finished` step, and Dashi renders steps' items after the batch's
   results, which it groups with the model message. No request is built
   while any call of the batch is unresolved, so a partial batch or a later
   `Resolve` cannot break the pairing. Rejected: holding items until the
   batch ends, which needs new state across commands.
2. **The `tool_finished` step records the items.** One commit holds the
   result, the hook results, and the items, so a crash loses or doubles
   nothing and replay reads them. Rejected: a separate `messages_added`
   step, which opens a crash window between two commits.
3. **IDs from turn, call, hook, and position.** The record shows which
   hook and call added an item, and a rerun after a crash gives the same
   ID. A host's own ID (UC-1) still wins and dedupes across calls.
4. **Withholding drops all items of the chain.** An item can describe a
   result the model no longer sees, so keeping it could leak what the
   guard withheld. This matches `BeforeModel`, where the first `Stop` drops
   added items. A `FailOpen` failure drops only that hook's items.
5. **Replace and AddMessages may combine; one `HookResult` per hook.** The
   verdict is `VerdictReplace` when the result changed, else
   `VerdictAddMessages` (reused, its comment now naming AfterTool). The
   items themselves are on the step, so nothing is lost.
6. **The same item rules as `BeforeModel`.** User role for untrusted
   origins; operator may also use system or developer; content blocks and
   reminders only. One rule for every hook that adds items.
7. **`Outcome.Data` stays when only items are added.** Data drops only when
   the model-visible result changes (`sameForModel`); items are not part
   of the result.
8. **`ResultPlan` is unchanged.** The hook already sees the call, the
   outcome, the current result, and the turn; nothing new is needed.
9. **Invalid items fail the hook.** Every hook point treats an invalid
   answer as an error. The guide recommends `FailOpen` for hooks that only
   add reminders, so a broken reminder never withholds a result.

## Success

The host's queue-and-drain workaround is deleted and its port maps
`additionalContext` to `AddMessages` in one hook. Requests never put an
item between a tool call and its result. Existing `AfterTool` behavior,
replay, and records without items are unchanged.

## Risks and open questions

- Items render in completion order, not call order, for parallel calls.
  This is deterministic from the record and matches what the model saw
  happen. Not blocking.
- A reminder hook without `FailOpen` withholds results on any error.
  Mitigated by the guide and the example. Not blocking.
