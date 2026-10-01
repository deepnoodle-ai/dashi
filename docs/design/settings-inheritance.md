# Complete settings during Session inheritance

Status: Accepted\
Date: 2026-09-30\
Workflow: Spec, then implementation after independent PRD review.

## Context

The [approved PRD](../prds/settings-inheritance.md) lets a host preserve omitted generation controls in new Session turns. `session_agent.go` currently fills every zero Settings field from lower-priority Agents. Changing the default would break partial callers. The implementation adds one explicit Settings boundary while preserving existing Agent merging and recorded continuation.

## Proposal

Add this field beside Agent.Settings in `turn.go`:

```go
// NoInheritedSettings stops Settings from inheriting lower-priority
// Agent values in a Session. Higher-priority partial settings may
// override this complete set. It does not affect other Agent fields.
NoInheritedSettings bool `json:"no_inherited_settings,omitempty"`
```

In `mergeAgent(a, b)`, where a has higher priority, shield Settings before the existing recursive fill. When a.NoInheritedSettings is true, assign a.Settings to the local b.Settings. The recursive fill can then inherit no different Settings values. Retain the existing Tools/NoTools selection rule and reflection merge for all other fields.

When only b is marked, merge a's partial settings over b's complete set as usual. The existing boolean fill copies b's true marker into the result. That result blocks Settings from the next, older layer. When both are marked, a's entire Settings wins. False selects ordinary merging at that layer. It does not cancel a boundary at a lower layer.

| Start marker | Session marker | Effective Settings |
|---|---|---|
| true | either | Start.Settings only |
| false | true | Start nonzero fields, then Session.Settings, with no historical fallback |
| false | false | Existing per-field fallback to the last user Agent, including its marker |

The boundary covers every Settings field, including nested structs and future additions. Do not enumerate generation controls. Merge local Agent copies and never write into a supplied or recorded Settings value.

Keep `Session.agentFor`'s source order and last-user selection. Queue dispatch already uses this helper with an empty Start.Agent. Preserve `buildCompaction`'s isolation: it inherits only Provider/API/Model when needed, never ordinary Settings or their marker. Runner commands for recorded turns continue using their recorded Agent.

## Records and compatibility

Retain the effective marker in StartFact.Agent, TurnView.Agent, and RunResult.Agent through existing recording and replay. It participates in existing command hashes, Agent equality, and derived versions. False is omitted from JSON, so legacy Agents retain their serialized shape and derived identity. Old records decode without migration.

Keep provider preparation unchanged. Nil pointers mean omitted controls. MaxTokens zero receives the existing finite provider-preparation default. Validation, budgets, preparation adjustments, retry identity, and recovery retain their current contracts.

## Alternatives and tradeoffs

- Replace every Settings struct wholesale. Rejected because existing partial callers would lose inherited controls.
- Add a clearing value for each field. Rejected because it changes the llm API and adds maintenance for every new setting.
- Store the choice on Session alone. Rejected because Start cannot override it and new handles lose the recorded choice.

A boolean adds an Agent selection rule. Hosts using it must first resolve their complete desired Settings. The small API keeps that cost local and leaves partial inheritance available by default.

## Verification and rollout

Add keyless tests for marked Start, marked Session, both, neither, and a carried marker after JSON storage and reopening. Observe Agent.Settings and model inputs for nil pointers, explicit zero/false pointers, MaxTokens zero, Citations false, and nested PromptCache clearing. Include a provider-preparation check that a cleared MaxTokens receives its ordinary default.

Exercise queue dispatch, compaction followed by an ordinary turn, and continuation after Session settings change. Compare old facts and derived Agent versions before and after the change. Keep legacy Agent identity coverage. Update the sessions guide and relevant API comments. Commit an Example showing older controls cleared by complete Session settings. Run `go test -race ./...` and the project's required checks. No migration, feature flag, or unresolved design question remains.
