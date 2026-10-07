# Bounded compaction and settled session recovery

Status: Accepted · Author: Compaction recovery track · Date: 2026-10-07\
Workflow: Write the spec, then build with independent implementation review.\
Requirements: [Continue after compaction or context limits](../prds/compaction-recovery.md)

## Context and goals

Summary output limits currently fail an active Turn immediately.
Later prompts cannot use automatic history compaction after that failure or a context limit.
The CLI also refuses these stopped parents before Session can apply its safe closure rules.
Fix these cases without new configuration, record kinds, or a provider-specific policy.

## Root summary retry

Keep the resolved 2,048-token default. Build the summary prompt after hooks and output-budget clamping, with the actual requested output allowance.
On retry, ask for a shorter summary of the same evidence instead of continuing the partial output.
Partial output remains audit and accounting evidence only.

The ordinary sender records the rejected result first.
The run loop permits one new admitted summary when the latest summary returned `StopOutputLimit`, without interruption, transport uncertainty, or tool content.
It reuses the original `ContextCompactionPlan`, preserving covered source order and hash.
Count recorded summary starts for the same coverage and hash, rather than using a process-local retry counter.
Every new start consumes that retry allowance, including a start whose result is lost.
An interrupted retry can stop safely after recovery. It cannot authorize a third automatic summary.
Explicit `Continue` keeps its existing ability to request new work after a stopped failure.

Require two remaining model calls before retry: its summary call and one ordinary response call.
Use the same admission, hooks, deadline, reservation, usage, and charge path.
A successful retry clears the prior summary failure before applying its replacement once.
Budget refusal, cancellation, and ambiguous recorder writes stop through existing contracts.
Readers reconstruct this behavior from existing purpose, plan, start, finish, and application facts.

## Session history protection

Extend the stock compaction policy's due check to consider projected text history size alongside reported model usage.
Keep this estimate in the shared compaction owner so alternative compactors using the policy receive the same behavior.
Use conservative encoded text bytes for the proactive trigger, without claiming tokenizer accuracy or hard fit.
Do not use encoded media bytes as context tokens. A history containing non-text media uses the existing reported-usage trigger only.
Keep the reminder, authority, bounded transcript, and complete-exchange rules unchanged.

Permit configured automatic compaction after safely closed failed or context-limited ordinary work.
Remember the pre-closure outcome while planning the next Start; closure changes it to canceled.
Keep existing skip behavior for unrelated cancellations, failures, and failed compaction Turns.
A call still runs at most one between-Turn summary. Failed compaction keeps history and proceeds through existing warning behavior.
The next Turn's active guard reports a remaining no-fit condition before ordinary dispatch.

Session's existing worker refinement, unknown-effect checks, and `closeLast` own closure.
No full record or conversation message is removed. The completed summary supplies only replacement model history.

## CLI admission and presentation

The automation snapshot recognizes only `compaction_failed` and `LimitContextWindow` as candidates for later-request recovery.
It retains missing-notice and unresolved-child gates.
For these candidates, it delegates actual parent admission to Session's existing safe closure predicate.
Do not independently infer settled effects from a stopped process, and do not introduce a CLI-owned history estimate.

Line and print UIs remember the last formatted state per compaction block ID.
Suppress identical states from the stopped step and final outcome, while printing changed authoritative outcomes and distinct compactions.
Keep summary text hidden and stdout limited to the ordinary answer in print mode.

## Alternatives and consequences

Raising summary defaults would increase cost without bounding verbose output. Budget-aware prompting and one retry provide a bounded recovery path.
Continuing the ordinary candidate after summary failure could dispatch an oversized request. Retain failure and repair later history through Session instead.
A CLI-only truncation would leave embedded applications stuck and obscure retained facts. Shared compaction and Session ownership serve both callers.
Removing final outcome presentation would lose updates after incomplete event delivery. Deduplicate presentation states instead.

One failed summary can add one paid call under the caller's existing budget.
Text history estimates can compact earlier than a tokenizer would require.
Media history still has no qualified byte-based context estimate.
No paid request is needed to validate this implementation.

## Verification and rollout

Use keyless output-limit, failed-retry, interrupted-start, lost-finish-acknowledgement, budget, and model-call fixtures.
Count summary starts, applied replacements, charges, and completed effects after reload.
Use fresh Session fixtures with large settled tool results and low last-model usage.
Cover failed retry followed by a smaller new prompt, media history, and unsafe parent/child admission.
Count CLI notices in automatic/manual print and plain flows. Preserve screen behavior.
Add a short public Session example and CLI transcript. Run focused race tests and `make check`.

Amend the existing active context design and guides. Record the user-visible fixes in the changelog.
Existing facts remain readable. Retry behavior changes only known output-limited summaries; no storage migration or release change is required.
