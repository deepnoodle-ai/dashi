# Add context from AfterTool hooks

Status: Approved\
PRD PR: #362  Implementation PR: #366  Updated: 2026-11-01

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

- **Claude Code `PostToolUse`** ([hooks](https://code.claude.com/docs/en/hooks)).
  `hookSpecificOutput.additionalContext` becomes a system reminder "next
  to the tool result", read on the next model request. Text over 10,000
  characters goes to a file, and the model gets the path and a preview.
  `updatedToolOutput` replaces the result. A separate `PostToolBatch`
  event runs after a full batch of parallel calls, before the next model
  call. Each tool call can add its own item.
- **OpenAI Agents SDK** ([guardrails](https://openai.github.io/openai-agents-python/guardrails/)).
  Tool output guardrails "can replace the output or raise a tripwire";
  they do not add a message.
- **LangChain** ([custom middleware](https://docs.langchain.com/oss/python/langchain/middleware/custom)).
  `wrap_tool_call` returns a `ToolMessage` or a `Command`, which can
  update state, including messages.
- **Dashi today.** A `BeforeModel` hook can read finished calls from
  `plan.Turn.Tools` and add items whose IDs come from the call IDs, so each
  item is added once. But it must find "new since the last call" itself,
  scans every call before every model call, and is split from the
  `AfterTool` hook that judged the result. A first-class field puts the
  item on the result's own record step, with no state in the host.
- **Placement already works.** Dashi renders all of a model message's tool
  results right after that message, and renders the items on any later
  step after them. A `tool_finished` step can already carry items: a
  turned-down `dashi_submit_output` call does (`rejectSubmission`). So
  items recorded on a call's `tool_finished` step follow every result of
  its batch. Unlike Claude Code, an item is not next to its own result.

## Users and job

**Primary:** a Go developer who builds an agent product on Dashi and adds
behavior around tools. Job: when a tool call finishes, add a reminder for
the model's next call, in one hook, with no queue of their own.
**Secondary:** a host that ports Claude Code style hook configuration and
wants `additionalContext` to map to one Dashi field.

## Concepts

No new concepts. `ResultDecision.AddMessages` is the `AddMessages` that
`ModelDecision` and `FinishDecision` already have: a list of
`ContextItem`s that the model sees on its next call, after every result of
the model message's calls.

**Recommended shape.** A post-tool item is a contextual reminder
(`llm.ReminderMessage` with `ReminderContextual`) with `Origin:
ContextRuntime`, whose text names the tool or call it is about. A Claude
Code port maps `additionalContext` to exactly this: the text can echo tool
output, so it must not carry operator authority, and it is not the
person's words, so it must not be `ContextUser`.

## Use cases

### UC-1: Nudge the model to save memory after a research tool

```go
nudge := dashi.ResultHook{Name: "memory-nudge", Func: func(
	_ context.Context, p dashi.ResultPlan) (dashi.ResultDecision, error) {
	if p.Call.Name != "web_search" || p.Outcome.State != tool.StateSucceeded {
		return dashi.ResultDecision{}, nil
	}
	return dashi.ResultDecision{AddMessages: []dashi.ContextItem{{
		ID:     "memory-nudge", // a fixed ID: at most once per turn
		Origin: dashi.ContextRuntime,
		Message: llm.ReminderMessage(llm.Reminder{
			Name: "memory",
			Tier: llm.ReminderContextual,
			Content: "The web_search call returned new facts. If any are " +
				"durable, save them with remember.",
		}),
	}}}, nil
}}
runner.Hooks = nudge.Hooks()
```

- [ ] The next model request has the reminder after all tool results of
      the batch, and the `web_search` result is unchanged.
- [ ] The item is on the call's `tool_finished` step (`Step.Items()`), and
      `Step.HookResults` has `{after_tool, memory-nudge, add_messages}`.
- [ ] A second `web_search` in the same turn adds no item, because the
      turn already records the ID, and its step has no `add_messages`
      `HookResult` for the hook.
- [ ] `TurnMessagesFull` of the ended turn includes the item after the
      results, so the next turn's history keeps it.
- [ ] An item with no `Origin` and a user message is recorded as
      `ContextUser`, as for `BeforeModel`; the guide shows the recommended
      shape first and says why.

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
model that the `read_file` output was redacted.

- [ ] The model sees the replaced result and the item. The record has one
      `HookResult` for the hook, with `VerdictReplace`.
- [ ] `Outcome.Data` is dropped because the result changed, as today. A
      hook that only adds items keeps `Outcome.Data`.

### UC-4: Failures

- [ ] Two hooks add an item with the same ID: the first hook's item is
      recorded with `add_messages`; the second hook records no
      `add_messages`.
- [ ] An invalid item (for example a system role from `ContextUser`) is
      dropped with a `WarningMessagesDropped` naming the hook and call. The
      result, the hook's `Replace`, and its valid items stand. An invalid
      `Replace` still fails the hook, as today.
- [ ] A later fail-closed hook fails: the result is withheld, no item from
      the chain is recorded, and one `WarningMessagesDropped` names the
      call. Earlier hooks keep their `replace` and `no_change` results;
      none has `add_messages`.
- [ ] A `FailOpen` hook fails: its own items are dropped; earlier and
      later hooks' items are kept.
- [ ] The run context ends during the chain: when the result is withheld,
      no item is recorded; when it is kept (the rest fail open), earlier
      hooks' items are kept.
- [ ] Replay refuses a record whose denied call, or call skipped for an
      urgent steer, has a `tool_finished` step that carries items.
- [ ] Replaying the turn never calls the hook. A crash before the step is
      committed records neither the result nor the items; the rerun hook
      gives the same IDs.

## Requirements

- R-1: `ResultDecision` gains `AddMessages []ContextItem` with JSON key
  `add_messages`. Its doc comment states placement ("after every result
  of the model message's calls, in the order the calls finished"), the
  recommended shape, naming the tool in the text, IDs, and failure.
- R-2: The items go on the `ContextFact` of the call's `tool_finished`
  step, committed with the result, for executor results and `Resolve`.
- R-3: An item without an ID gets
  `<turn>:tool:<call ID>:hook:<hook name>:<n>`, n counting from 1 within
  the hook's list, mirroring `hookItemID` for `BeforeModel`.
- R-4: `skipRecordedItems` runs once over the whole chain's items, in
  hook order, before any verdict is recorded, so the record matches the
  items: a hook gets `add_messages` only when at
  least one of its items is recorded and it did not replace the result.
  A hook whose items were all dropped records nothing for them, or
  `no_change` when it gave a `Reason` or `RulesVersion`.
- R-5: Item rules are `ContextItem.Validate`, the same as `BeforeModel`.
- R-6: Replay accepts non-steer items on a `tool_finished` step only when
  it has an `Outcome`, the call is not `dashi_submit_output`, and the
  call's status before the step is one an executor or `Resolve` result
  finishes from: `started`, `waiting`, or `result_unknown`. This mirrors
  the status check for a turned-down submission (`turn_replay_apply.go`)
  and reads no `Reason` text. Denied, hidden, invalid, and steer-skipped
  calls finish from `requested`, `allowed`, or `denied`, so their steps
  refuse items. Steers stay limited to turned-down submissions.
- R-7: Update the docs: `docs/guides/hooks.md` (table row, a section with
  the recommended shape and the fixed-ID pattern for one item per turn or
  batch), the `Hooks` combining rules, the
  `WarningMessagesDropped` comment (today it names only `BeforeFinish`;
  it also covers duplicate IDs and now `AfterTool`), the
  `VerdictAddMessages` comment (now `BeforeFinish` and `AfterTool`;
  `BeforeModel` records `change`), the `Step.Items()` comment (it should
  name `AfterTool` results among the steps that carry items), the hook
  table in `design/hooks.md`,
  `ResultDecision` in `design/hooks-after-tool.md`, and `CHANGELOG.md`.

## Not in scope

- Adding items for calls with no `AfterTool` run: denied, invalid,
  unknown, or submitted calls. A `BeforeTool` denial already sends the
  model its reason.
- A size or count limit on added items, unlike Claude Code's 10,000
  characters. A hook may add one item per tool call, as Claude Code does,
  and items stay in the turn's messages. A host that wants at most one
  item per turn or per batch uses a fixed ID (UC-1); one that wants the
  size cap truncates in its hook. No host has reported a flood.
- Showing earlier hooks' items to later hooks. Hooks at one point stay
  independent; IDs remove duplicates.
- A per-batch hook like `PostToolBatch`. A `BeforeModel` hook already runs
  once after each batch.
- Items after a turn ends. An ended turn refuses tool results, so no
  `AfterTool` runs.

## Decisions

1. **Placement is a rendering rule, not a timing rule.** Items ride on the
   `tool_finished` step, and Dashi renders steps' items after the batch's
   results, which it groups with the model message. No request is built
   while any call of the batch is unresolved, so a partial batch or a later
   `Resolve` cannot break the pairing. Rejected: holding items until the
   batch ends, which needs new state across commands.
2. **Authors name the tool in the text; Dashi does not.** The host knows
   the right wording, and a prefix Dashi added would change the content
   that an item's ID dedupes on. The docs and every example name the call.
3. **The `tool_finished` step records the items.** One commit holds the
   result, the hook results, and the items, so a crash loses or doubles
   nothing and replay reads them. Rejected: a separate `messages_added`
   step, which opens a crash window between two commits.
4. **IDs from turn, call, hook, and position.** The record shows which
   hook and call added an item, and a rerun after a crash gives the same
   ID. A host's own ID (UC-1) still wins and dedupes across calls.
5. **Withholding drops all items of the chain.** An item can describe a
   result the model no longer sees, so keeping it could leak what the
   guard withheld. This matches `BeforeModel`, where the first `Stop` drops
   added items. Earlier `replace` and `no_change` results stay, since those
   hooks did run; the warning says the items were dropped. A `FailOpen`
   failure drops only that hook's items.
6. **Replace and AddMessages may combine; one `HookResult` per hook.** The
   verdict is `VerdictReplace` when the result changed, else
   `VerdictAddMessages`. Generated IDs name the hook, so the items still
   attribute to it; a host's own ID loses that, which is acceptable.
7. **The same item rules as `BeforeModel`, with a different penalty.**
   `ContextItem.Validate` decides what is valid at every point: user role
   for untrusted origins; operator may also use system or developer;
   content blocks and reminders only. What happens to an invalid item
   differs. `BeforeModel` and `BeforeFinish` count it as a hook failure.
   At `BeforeModel` that only skips the hook, since it fails open. At
   `AfterTool`, a failure withholds a result the model needs, and an
   invalid item cannot leak anything, so `AfterTool` drops the item
   instead (Decision 10).
8. **`Outcome.Data` stays when only items are added.** Data drops only when
   the model-visible result changes (`sameForModel`); items are not part
   of the result.
9. **`ResultPlan` is unchanged.** The hook already sees the call, the
   outcome, the current result, and the turn; nothing new is needed.
10. **Invalid items are dropped, not a hook failure.** Fail-closed exists
    to stop leaks, and an invalid item cannot leak because the model never
    sees it; Dashi warns and adjusts. `WarningMessagesDropped` fits: its
    meaning is "items were not added". `FailOpen` stays optional for
    reminder hooks; it matters only when the hook returns an error.

## Success

The host's queue-and-drain workaround is deleted and its port maps
`additionalContext` to `AddMessages` in one hook. Requests never put an
item between a tool call and its result, and the record's `add_messages`
results always match the recorded items. Existing `AfterTool` behavior,
replay, and records without items are unchanged.
