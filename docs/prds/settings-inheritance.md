# Keep omitted settings omitted in new session turns

Status: Draft\
PRD PR: pending  Implementation PR: pending  Updated: 2026-09-30

## Problem and users

An application resolves generation settings before starting a new Session turn. It needs that complete result to replace older controls. Today, `Session.agentFor` fills nil and zero fields from `Session.Agent` and the last user turn. Omitting temperature or reasoning therefore revives an earlier value. Resetting MaxTokens to its default revives an earlier explicit limit. This blocks the CLI settings work and forces hosts to work around library inheritance.

## Context and concept

Session merges `Start.Agent`, `Session.Agent`, and the last user-origin Agent in that order. Runner then resolves its ordinary defaults. Partial Settings inheritance is useful and remains the default. Commands on an existing turn use its recorded Agent.

**`Agent.NoInheritedSettings`** makes that layer's Settings complete. Settings omitted or zero at that layer do not inherit from lower-priority layers. Higher-priority fields still override it. The boundary applies to Settings only. Instructions, model, tools, output schema, and limits retain their current rules.

## Use cases and acceptance

### UC-1: Replace older settings with a complete set

A host resolves its current configuration and sets it on Session.Agent:

```go
s.Agent = dashi.Agent{
    NoInheritedSettings: true,
    Settings: llm.Settings{MaxTokens: 2_000},
}
out, err := s.Send(ctx, "Continue with these settings.")
// out.Agent.Settings has no inherited temperature or reasoning.
```

- [ ] After a user turn records temperature, reasoning, and a token limit, a marked empty Settings set leaves Temperature and Reasoning nil and MaxTokens zero.
- [ ] Provider preparation applies the existing MaxTokens default and model output limit. It does not restore the earlier token limit.
- [ ] A marked nonempty set preserves supplied fields and clears every omitted Settings field, including booleans and nested PromptCache fields.
- [ ] Explicit pointer zero and false values retain their existing meanings. Validation, provider adjustments, and budget enforcement remain unchanged.
- [ ] The rule survives JSON record storage and reopening with a fresh Session handle.

### UC-2: Combine layers deliberately

A developer marks `Start.Agent` to replace both Session and historical Settings. Alternatively, they mark `Session.Agent` and use a partial Start override.

- [ ] Marked Start.Settings blocks both lower layers, even when the set is empty.
- [ ] A partial Start inherits missing fields from marked Session.Settings. Any remaining nil or zero stays nil or zero.
- [ ] When both layers are marked, Start.Settings wins as a complete set.
- [ ] With no boundary, existing per-field merging remains unchanged.
- [ ] An empty Session handle can carry the last user turn's complete Settings and marker. A later partial Start overrides supplied fields under the same priority rules.
- [ ] The effective marker appears in TurnView.Agent and Outcome.Agent. False remains the zero value and is omitted from Agent JSON.

### UC-3: Queue, compact, and resume

A host changes Session.Agent for future work while a recorded turn or queued input exists.

- [ ] Queue turns use the current Session boundary. User queue turns can carry their Agent forward. Scheduled, operator, and compaction turns remain excluded as carry sources.
- [ ] Automatic and manual compaction retain the Compactor's settings. Only the existing model-identity fallback reads Session's merged Agent.
- [ ] The ordinary turn after compaction uses the requested boundary and the replacement history.
- [ ] Continue, Settle, Deliver, and Cancel use the already recorded Agent, regardless of later Session changes.
- [ ] Earlier Start facts, model intents, prepared requests, and revisions remain unchanged. Existing retry and agent-change checks still apply.

## Requirements and decisions

- Add `NoInheritedSettings` beside Agent.Settings and document its layer behavior. Commit a runnable Example and update the sessions guide.
- Apply the boundary to the whole Settings struct. Reject individual clearing flags or optional wrappers, which expand the llm API and cannot cover future settings uniformly.
- Keep the opt-in false by default. Reject unconditional struct replacement, which breaks callers supplying partial Settings.
- Preserve the marker as part of the serialized Agent under existing identity rules. Reject transient handle state, which loses intent across reopening.
- Keep the boundary specific to Settings. Reject a generic merge-policy API, which introduces unrelated choices for tools, limits, and instructions.

## Scope, risks, and success

Exclude CLI commands, flags, settings-file formats, provider default changes, and record migration. Complete Settings requires the host to resolve its desired configuration first. Developers who want partial inheritance leave the flag false.

Tests cover layer combinations, nil/zero values, fresh handles, queue dispatch, compaction, and recorded continuation. Reviewers run the committed Example. Legacy records decode with false, and existing partial-inheritance and recovery tests pass. Named PRD review precedes the lightweight technical spec and implementation. No product question blocks this draft.
