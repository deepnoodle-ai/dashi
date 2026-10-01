# Recorded context for an active Turn

Status: Accepted · Author: Active-context track · Date: 2026-09-30\
Workflow: Media-estimate amendment passed named PRD re-review at `0811b0d` and root checkpoint; implementation follows.\
Requirements: [Finish a long coding request](../prds/active-turn-context.md) · PRD PR: #144 · Implementation PR: #147

## Shape and public API

One Turn owns its ordinary work, summary attempts, and applied context replacements.
Caller history remains caller-owned. Existing `Session.Compactor` remains between-Turn compaction.
The root uses private `internal/compactiontext` helpers, not the optional `compaction` package.

```go
type ActiveCompactionPolicy struct {
    Disabled bool    // explicit Session inheritance override
    Window   int     // optional caller cap; zero uses Client.ModelInfo
    Fraction float64 // zero resolves to 0.8
    MaxTokens int    // summary output; zero resolves to 2048
}
// Agent gains: ActiveCompaction *ActiveCompactionPolicy
// Nil disables protection, or inherits under existing Session rules.

func ProjectContext(t Turn) ([]llm.Message, error)
// Project remains ordinary Turn messages, with no summary exchange.

type ModelPurpose string
const ModelPurposeCompaction ModelPurpose = "compaction"
// ModelFact, ModelPlan, and SendPlan gain Purpose.
// Empty purpose means an ordinary model call, including older records.

type TurnMessageRef struct {
    Seq int64 // source step
    Index int // message within that step; zero-based
}
```

Validate nonnegative integers and a finite fraction in `(0, 1]` after default resolution.
Reject an explicit window below 1024. Select a positive caller window when `Client.ModelInfo` has no known window.
When both windows are positive, select the smaller caller cap or model window. Otherwise select the known model window.
Pin resolved fraction/output defaults in `StartFact.Agent` and its derived revision. Clone the policy pointer on public boundaries.
Pin the selected window separately in new `StartFact.ActiveCompactionWindow int`; expose it in `TurnView.ActiveCompactionWindow`.
Preserve `Policy.Window` as the caller's cap, including zero, so inherited catalog windows resolve again for each new Turn.
`Disabled` prevents inheritance without removing recorded intent.
Only when neither window is positive, pin zero, disable protection, and warn once with `active_compaction_unavailable`.
No changing catalog can alter an accepted Turn's selected window.

## Request sizing and safe coverage

Construct the ordinary candidate through existing tool selection, steering, `BeforeModel`, reference verification, and provider preparation.
For text-only candidates, use `llm.EstimatedInputTokens(request)` (wire-body bytes plus 1024) and effective `MaxTokens` as output reserve.
Saturating arithmetic computes allowance `max(0, Window - reserve)`.
Trigger proactive compaction when estimated input reaches `Fraction * allowance`.
The hard fit limit remains the allowance: reject ordinary dispatch only when input exceeds it.
The text-only estimate includes tools, hooks, and provider replay fields; it is not a provider-tokenizer guarantee.
Do not substitute cumulative usage or cached-token discounts. Catalog uncertainty never becomes a fabricated 128k window.

After reference hydration and provider preparation, inspect the candidate's resolved structured content, including caller/Turn history, hook deliveries, and nested tool-result content.
Image, PDF, audio, or video content makes its active context estimate unavailable.
Record `active_compaction_media_estimate_unavailable` once per Turn using existing warnings; replay/continuation does not duplicate it.
Skip proactive compaction and byte-based `LimitContextWindow` for that candidate, including mixed text/media, even with a known window or cap.
Do not disable the recorded policy: later text-only candidates use ordinary checks, and applied replacements remain valid.
Provider content/capability preparation and actual window acceptance govern media requests; overflow keeps existing failure semantics with no compaction retry.
`llm.EstimatedInputTokens`, `MaxCostAtRisk`, monetary admission, limits, and settlement remain unchanged.
This separates unavailable context sizing from conservative financial risk; an insufficient cost budget can still refuse the media request.

The private projector annotates messages with `TurnMessageRef` before cloning them for public callers.
Ordinary sources use their step sequence and message index. An applied summary uses its `context_compacted` sequence and index zero.
Group each assistant call message with every paired local/hosted result and its required replay artifacts.
Only complete settled groups qualify. Preserve initial `Start.Input`, system/developer messages, operator steering, and current named context items.
Also preserve live provider continuation dependencies. Drop a replay artifact only as part of an explicitly covered complete group, never by editing retained content.
Unchanged retained messages keep their origin and context-slot identity.

Select older eligible groups in order, keeping recent groups within 25% of allowance where possible.
Never split a group. A second pass can cover the previous applied summary and newer settled groups.
Build labeled evidence with the existing bounded-transcript rules: exclude private reasoning, use media markers, and shorten oversized evidence with visible markers.
The fixed summary prompt asks for objective, findings, changes, verification, and remaining work.
Summary text uses the existing tagged-summary interpretation and user-role frame. It acquires no operator authority.

If no eligible coverage exists and estimated input plus output reserve fits the pinned window, dispatch the ordinary candidate without a summary.
If a text-only candidate exceeds allowance and no legal compaction can make it fit, stop without summary dispatch.
This no-fit result is `StopLimited` with new `LimitContextWindow`, `Limit=Window`, and `Used=estimated input + reserve`.
`Stop.Reason` is `context_window`; a bounded warning distinguishes fixed input, prior history, replay dependency, or retained exchange.
The chosen window remains immutable. A larger model or smaller original input requires a new Turn; preserve settled work before starting it.

## Recorded plans and application

Add `ModelFact.Compaction *ContextCompactionPlan` only on compaction intents.
Settlements inherit the intent's purpose; reject any explicit mismatch.

| Fact | Recorded fields and validation |
| --- | --- |
| `ContextCompactionPlan` | `Through int64`, ordered `Covered []TurnMessageRef`, `Digest string`, `Window int`, `EstimatedBefore int`. `Through` ends before the intent. Coverage names visible eligible messages at that revision. Digest uses the existing canonical message fingerprint. |
| `StepContextCompacted` (`context_compacted`) | `Step.Compacted *ContextCompactionFact`, containing `AttemptID string`, `Summary string`, `EstimatedAfter int`. The attempt must have an acknowledged complete summary result and an unapplied plan. |

Replay validates coverage, group closure, protected messages, digest, summary extraction, and strict estimate savings.
Validation operates on the frozen prefix through `Through`, including earlier replacements.
It does not depend on a current policy, model call, or supplied caller history.
The result and application both retain summary text so public readers can inspect application directly; they must agree exactly.
Repeated application is invalid. Failed, interrupted, empty, or non-shrinking summaries never produce this step.

Summary intents retain the same request digest, admission terms, pricing evidence, identities, and settlement accounting as ordinary intents.
Replay separates `pendingCompaction` from ordinary `latestResult` and `awaitingClassification`.
Summary settlements update spend and normal attempt counts, but cannot create `Answer`, tools, provider pauses, or structured output.
Before settlement, reject tool/hosted calls, incomplete stops, empty summaries, and no savings.
Preserve the raw result and accounting, record the rejection in `ModelFact.Error`, and stop with `StopReasonCompactionFailed="compaction_failed"`.
Only complete settlements without an error derive pending application. Explicit continuation can admit a new attempt after a rejected summary.
No `BeforeFinish` or output-schema handling runs for summary results.

## Runtime, hooks, and limits

At a settled boundary, the candidate may trigger one summary attempt during its ordinary preparation cycle.
Discard the unsent ordinary prepared request. Preparation is not a dispatch or billing identity.
Generate a bounded summary request through the same client's preparation/send path, with `Purpose=compaction`, no tools, and no output schema.
Start from recorded agent settings, replacing `MaxTokens` with the smaller summary allowance and clearing forced tool selection.
Reapply normal output/cost admission and text-only summary context checks before intent commit; hook-added media uses the same warning/exception.

`BeforeModel` and `BeforeSend` receive the purpose and run in registration order.
Record hook deliveries as ordinary Turn context, never as summary conversation messages.
Include deliveries in summary sizing and retain them for the next ordinary request.
Hook settings may change summary generation but cannot enable tools, native output schemas, or another destination.
`BeforeSend` receives the exact immutable summary request and normal at-risk/remaining amounts.
Hook failures, denials, deadlines, and cancellation retain current semantics.

The shared sender owns stream collection, intent/settlement, admission, and accounting; extract it privately rather than creating a second dispatch implementation.
After application, rebuild the ordinary candidate from recorded context and re-run normal preparation/hooks.
Hooks already permit repeated preparation passes; durable delivery IDs and `HookActed` retain their existing meaning.
A second sizing pass cannot start another summary in that cycle, even when hooks add context.
Reapply media detection after preparation; if unavailable, dispatch subject to ordinary admission. Otherwise enforce text-only fit.
New ordinary model/tool work permits the next compaction cycle; explicit continuation permits a new attempt after failure.

Summary attempts use existing `ModelAttempts`, interruption, and cumulative budget rules.
Do not dispatch a summary when fewer than two ordinary attempt slots remain: leave one ordinary response attempt available.
This permits a final response but does not guarantee completion if the model selects another tool.
The existing final-answer reserve applies only under its documented conditions; active compaction adds no forced-answer contract.
Return the existing model-attempt limit instead. Interrupted attempts retain existing separate interruption counting.
Unknown/partial summary usage keeps existing conservative accounting and capped-admission behavior.

## Projection and recovery

`Project` returns full ordinary Turn messages and skips summary intents/results/application messages.
`ProjectContext` applies recorded replacements to those messages in source order.
Runner request construction combines unchanged caller history with `ProjectContext`.
`Session.History` uses `Project`; `Session.ProjectedHistory` and later Turn starts use `ProjectContext` after existing between-Turn replacement rules.
Cache these views separately. Existing caller-history fingerprints and ended-Turn cutoffs remain authoritative.
Expose applied replacements as `TurnView.ContextCompactions []ContextCompactionFact` for inspection.
Named context presence reflects retained `ProjectContext` messages; a summary mention never establishes presence.

On reload, an acknowledged successful summary settlement with no application derives pending application.
Validate it and commit `context_compacted` once, without another model call.
An acknowledged summary intent without result uses `recoverModelIntent` on explicit `Continue`.
Its no-tools status permits established safe repeat; retain the interrupted attempt and all accounting evidence before a new admitted attempt.
An ambiguous recorder write returns the existing `CommitError` and requires authoritative reload before further dispatch.
Do not reconcile it by inventing an application or repeating coding tools.
Cancel ends foreground and summary work; interruption preserves resumability. Previews never apply replacements.

## CLI, rollout, and proof

Enable `Agent.ActiveCompaction=&dashi.ActiveCompactionPolicy{}` in the CLI's agent configuration.
Purpose on each intent identifies following deltas and settlement as summary progress, never ordinary answer text.
Emit markers for start, application, failure, and unavailable window/media estimate in screen and line modes.
A known pinned window must not imply media protection; explain that provider acceptance governs those requests.
Reload derives applied markers from facts. Estimated savings and actual summary spend remain separate.
Keep Wonton, existing `/compact`, and the between-Turn workflow.

Implementation owns new core context helpers, replay/projection changes, tests, and examples.
Root schedules shared CLI integration after MCP completes.
The publication chain is composer → MCP → active context → attachments; no implementation starts before the root checkpoint.
Old records keep ordinary purpose and identical projections. New step kinds require an updated reader; document that compatibility boundary.

Keyless evidence covers two compactions, both Session views after JSON reload, objective/evidence retention, hook sizing, and replay-artifact groups.
Prepare a generated valid ~1 MiB PNG with actual wire encoding at known 128k/200k windows: sufficient budget permits ordinary `Send`, no summary or false context limit, one recorded/replayed warning.
Repeat with historical/hydrated-reference/tool-result media; repeat/reload candidates without warning duplication. Verify text-only checks and unchanged cost-risk/cost-budget refusal separately.
Fault fixtures cover crashes after intent, settlement, and application, ambiguous writes, cancellation, concurrent continuation, and counting tools with no repeated effects.
Accounting fixtures cover exhausted attempts, summary failures, partial usage, token/cost caps, and unchanged between-Turn behavior.
Ship an `Example*`, guide, CLI transcript, Wonton recordings, and required independent reviews.
Run formatting, vet, build, root race tests, and affected-module tests.

## Alternatives and remaining costs

An unrecorded hook rewrite is smaller but cannot reconstruct applied context after restart.
A separate summary Turn fits Session compaction but changes ownership and permits a separate budget during one request.
Rewriting caller history would reduce more context but violates the accepted continuation contract.

The chosen approach adds record kinds, conservative early text compaction, and billed summary attempts.
Media context protection is unavailable; encoded-byte formulas would reject usable inputs, while a qualified provider estimator is separate work.
It can stop when protected input dominates. Summaries can lose evidence despite preserving source facts.
Provider overflow after a conservative estimate remains ordinary failure, without compact-and-retry.
Named PRD re-review returned ready for the media amendment; root approved its implementation checkpoint.
