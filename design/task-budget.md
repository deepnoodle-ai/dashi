# Task budgets

**Status:** Accepted · **Date:** 2026-10-23

## Problem

`MaxTokens` caps each response, but the model cannot see it, so a long
reasoning pass can spend the whole cap and leave no answer. Anthropic's task
budget is a token budget the model sees for a whole agentic loop and paces
itself against. Dive added it (deepnoodle-ai/dive#345) by tracking message
pointers in memory, so a resumed turn lost track of its spend. Dashi ports the
behavior, not that code.

## Decisions

### A request setting named as the provider names it

```go
// llm.Settings
TaskBudget *TaskBudget `json:"task_budget,omitempty"`

type TaskBudget struct {
	Total     int  `json:"total"`
	Remaining *int `json:"remaining,omitempty"`
}
```

It is advisory and covers one Turn: every model call that answers one user
message. `MaxTokens` stays the hard limit on each response. It is unrelated to
`dashi.Budget`, which the runner enforces; neither is derived from the other.
`Input.Validate` rejects `Total <= 0` and `Remaining` outside `0..Total` as
programmer errors.

### Capability through the catalog

`FeatureTaskBudget` (`task_budget`) is claimed per model in
`catalog/anthropic.json`, sourced to Anthropic's task budget page: supported on
Opus 4.7, 4.8, 5, and 5.5, Sonnet 5.5, Haiku 5.5, Fable 5 and 5.1, and Mythos 5
and 5.1; unsupported on the 4.5 and 4.6 generations and Sonnet 5 (which answers
200 but is documented unsupported). Responses, Chat Completions, and Gemini
declare it unsupported in their API limits. `Fit` handles it like citations:
an unsupported budget is omitted with a `setting_omitted` warning, or an
Unsupported error under `CapabilityStrict`. An unknown model follows the
general rule: adjust sends it and records the feature as unverified; strict
refuses it.

The 20,000-token minimum is a new limit, `Capabilities.MinTaskBudget`, which
the Messages client states for the whole API, like `MinReasoningBudget`. Under
adjust, `Fit` raises a smaller `Total` to it with a `setting_clamped` warning
(`Remaining`, already within the old total, stays); strict fails.

### Wire format

`output_config.task_budget = {"type":"tokens","total":N,"remaining":M}` beside
`output_config.effort`, with `anthropic-beta: task-budgets-2026-03-13` joined to
any other beta value (the MCP client beta) in one comma-separated header. The
header follows the effective settings, so an omitted budget sends no beta.

### The Turn engine

Every ordinary call sends the agent's budget as is, and the provider counts the
spend from the history. Summary calls (`ModelPurposeCompaction`) drop it, as
they drop `ToolChoice`. Hooks may change it like any setting.

Active compaction removes messages, and their spend, from the request. After a
`context_compacted` step, the runner sends `Remaining` = the agent's
`Remaining` (or `Total`) − the tokens of the Turn's messages that are no longer
sent, floored at 0. It derives this from the record alone: the Turn's messages
without replacements (`TurnMessagesFull`) minus those still sent
(`TurnMessages`), compared by message reference. A replaced model reply counts
the output tokens its `model_call_finished` step recorded, which include
reasoning the message may not carry; any other replaced message (tool results,
added context) uses `llm.EstimatedMessageTokens`, the estimator budget checks
already use. A summary a later compaction replaces counts nothing, since the
messages it stood for were counted already. Because only recorded facts are
read, a recovered or replayed Turn computes the same value; the budget sent is
also recorded in `ModelFact.Effective`.

## Limits

- The estimate for tool results is about four bytes per text token and a
  fixed amount per media item, so `Remaining` is approximate.
- The summary text added in place of the replaced messages also enters the
  context; it is not charged against `Remaining`.
- Earlier turns' history counts against the budget on Anthropic's side, and
  Dashi does not adjust for it; size the budget from measured turns.

## Tests

Messages encoding (body, `remaining`, beta header, merge with the MCP beta),
omission on unsupported models and other APIs, strict errors, the below-minimum
clamp, unknown models, validation, and a Turn with two active compactions whose
recorded budgets match a replay of each prefix and of a recovered Turn. One
live call to Haiku 5.5 with a 20,000-token budget passed on 2026-10-23.
