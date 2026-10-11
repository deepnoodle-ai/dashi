# Strict history and validated summary replacements

Status: Accepted · Author: History helpers track · Date: 2026-10-11\
Workflow: Write the spec, wait for independent PRD review, then implement.\
Requirements: [Continue only validated conversation history](../prds/swarm-history-helpers.md)

## Context and goals

`BuildHistory` reads newest first and stops at a successful compaction Turn.
Its source-hash check rebuilds covered history tolerantly and discards that construction's warnings.
A warning wrapper therefore cannot reliably reject every supplied corrupt record.
Applications also repeat checks before activating a completed summary.
Provide strict construction and a pure replacement value using the current history rules.

## Strict construction

Add this public function and a small identifying error in the root history area:

```go
func BuildHistoryStrict(turns []Turn) ([]llm.Message, error)

type HistoryError struct {
    TurnID string
    Err    error
}
```

`HistoryError` implements `Error` and `Unwrap`.
Callers inspect `TurnID` with `errors.As`. Replay errors preserve `ErrInvalidTurn`.
Add `ErrCompactionMismatch` so callers can identify mismatched summary coverage with `errors.Is`.
Errors name the failed requirement. They never claim that replacement history was used.

Check every supplied Turn's view before building output.
Check ordinary Turn message construction even when a newer summary will replace those messages.
Failed or empty compaction Turns still contribute no conversation messages.
Check each successful compaction with a nonempty hash when supplied records precede it.
Build that prefix with the same replacement rules, propagating its integrity failures.
Then assemble history from the newest successful summary and subsequent ordinary Turns.
Return nil history on the first failure in supplied order.

Share selection and assembly logic with `BuildHistory` rather than maintaining two history rules.
Cache validated views and messages during one construction.
Keep the tolerant builder's warning codes, order, fallback, and output unchanged.
Strict construction does not turn recorded hook or budget notices into integrity failures.

The input contract remains oldest first.
Do not infer omitted history from IDs or `CompactionFact.Through`.
A first summary has no supplied prefix to compare.
Valid historical compaction records without hashes remain accepted, with no source hash guarantee.
Document both limits beside the strict function and in the guide.

## Pure summary validation

Add this API in `compaction/replacement.go`:

```go
func ValidateReplacement(
    frozen dashi.Start,
    source []llm.Message,
    summary dashi.Turn,
) (Replacement, error)

type Replacement struct {
    TurnID         string
    SourceHash     string
    SourceMessages int
    Summary        string
    History        []llm.Message
}
```

The helper accepts the exact saved Start used to admit the summary, after application edits.
Require compaction origin, valid placement and retained messages, and a nonempty `Compaction.Hash`.
Validate frozen source message content before hashing it.
Check `dashi.HistoryHash(source)` against that hash.
Replay `summary` through `View` and require compaction origin and `StopCompleted`.
Compare every `CompactionFact` field with the frozen fact, including descriptive context identity in retained messages.
Compare the recorded initial input with `frozen.Input`.
Check the recorded prior-history hash against `frozen.History`.
When the frozen command ID is nonempty, require the same recorded start command ID.

Inspect recorded changes through the stop that completed the summary.
Reject every nonempty context fact added by an `AddMessages` command or marked `Steering`, regardless of item origin.
Compare accumulated `TurnView.Input` with the frozen initial input to reject changed recorded user input.
Reject any recorded `WarningHistoryMismatch` in this prefix.
A later continuation with the original history cannot erase an earlier mismatch.
Do not classify other recorded warnings as frozen-input failures.

Permit recorded context that policy hooks add without an application command or steering source.
This includes `BeforeModel` additions and `BeforeFinish` feedback that requests another model call.
Hook additions and settings changes lie outside the frozen comparison.
The application authorizes those hooks. The helper does not prove the complete model request or hook behavior.

Recovery is valid when these comparisons still pass and the Turn ultimately completes.
Permit authoritative `Resolve.ModelResult` or `Resolve.ModelFailure` and Dashi-authorized continuation with unchanged prior history.
Earlier interruptions, failures, and limit stops do not invalidate a subsequently completed summary by themselves.
The application remains responsible for authorizing budget raises and preserving job account and deadline.
Late results after the ending stop remain accounting evidence and cannot change replacement input or summary text.

Use `ReplacementHistory` and the existing summary extraction rule.
Reject a missing or whitespace-only extracted summary.
Return detached history and summary text without the framing prefix.
Errors identify the summary Turn and failed check, wrapping replay or encoding errors where applicable.
Provide JSON field tags and `Replacement.Clone` because the value contains message content.
The zero replacement returned with an error contains no usable history.

The value supplies data for activation, not an activation result.
The application verifies its bound Turn ID, account, deadline, and current source revision.
It atomically activates its job once, compares conflicting retries, and reloads after an unknown write.
Old history remains active until that transaction commits.
The application keeps required failed summaries from admitting oversized work.

## Alternatives and consequences

- A strict warning wrapper is smaller, but current traversal can hide supplied corruption and nested mismatches.
  Shared construction with explicit error propagation covers those cases.
- Changing `BuildHistory` defaults would protect unaware adopters, but changes tolerant Session behavior and existing callers.
  An explicit function makes the application's choice visible.
- Rebuilding the compaction Start would avoid saving it, but policy or prompt changes could validate a different summary after restart.
  Compare saved metadata instead.
- A durable coordinator could manage jobs and activation, but would require application storage, revision, account, and scheduling contracts.
  The pure value removes repeated interpretation without introducing another authority.

Strict construction can cost more than tolerant traversal because it reads all supplied records.
Reuse validated projections and existing hash encoding. Avoid a global cache or benchmark claim.
The helper cannot prove omitted history, summary accuracy, current revision, or committed activation.
No new Turn fact, record version, storage interface, or release-module dependency is required.

## Initial-context interaction

The turn-inputs track adds `Start.Context` and `StartFact.Context` outside this branch's baseline.
This branch must compile against its baseline and must not probe future fields through reflection or JSON.
After composition, compare those fields directly in their full ordered form, including identity, role, snapshot, and lifetime.
Compare accumulated user input against `Start.Input` plus the initial user items' recorded message projection.
Do not misclassify those initial items as later input.
The history-helper author owns this comparison and its tests once the typed fields are available.
The orchestrator runs a combined-tree check before reporting all tracks ready and routes any missing comparison back to this author.

## Verification and rollout

Pin unreadable ordinary and compaction records, unresolved ordinary work, and corruption hidden by a later summary.
Cover nested compactions, nested hash mismatches, failed/empty summaries, valid hashless records, and trusted suffixes.
Check harmless notices, deterministic error identity, nil output on errors, and unchanged tolerant results.

For replacements, alter each frozen metadata field and source history independently.
Cover failed, limited, interrupted, empty, malformed, and wrong-origin summary records.
Reject later user and non-user `AddMessages`, steering input, and resumed history mismatch, including mismatch followed by restoration.
Accept hook-added context and legitimate recovery through same-history continuation or authoritative model-result resolution.
Round-trip Start, source, Turn, and replacement through JSON.
Repeat validation after reload and mutate returned/cloned messages to prove independent ownership.
Add short public examples for refusal and finished-summary reuse.
In the combined tree, reject changed initial context and accept unchanged initial user context without mistaking it for later input.

Keep changes in `history.go`, new `history_` helpers, and `compaction/`, plus examples, guides, and changelog.
Do not edit root admission, replay, facts/views, run loops, feed, MCP, or Swarm.
Run focused tests with the race detector and `make check` before publishing the implementation PR.
The APIs are additive. Existing stored Turns and tolerant consumers retain their behavior.
