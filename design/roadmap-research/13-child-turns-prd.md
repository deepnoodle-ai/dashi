# 13. Child turns and delegation — draft PRD

**Status:** proposal for review; no API approved. **Size/difficulty:** L / high ([roadmap](../remaining-work.md)). **Reference:** [research](13-child-turns-research.md), `design/functional-areas.md`, `design/budgets.md`. The first consumer is Mobius's blocking `explore`/`advise` tool (`internal/agentruntime/executors/agent_subagents.go:194-327` in Mobius), which currently loses resumability when its child suspends.

## User problem

An application author wants a parent agent to ask a specialist to investigate, show that work live, and then use its answer. Today Dashi can run two agents, but it cannot tell the application that one Turn was caused by a particular parent tool call. After a worker crash, the application must guess whether to start a second billable child. It must also reconstruct child cost and decide how cancellation and suspended child work affect the parent. A developer should see one comprehensible chain: parent call → child Turn → child result → parent settlement.

## Goals

- Give each child its **own** recorded Turn and immutable parent tool-call coordinate, so its facts, spend, and stop can be inspected and recovered independently.
- Make duplicate admission discoverable and recoverable using a stable, caller-owned child Turn ID; show parent/child linkage in read APIs and examples.
- Provide a small documented host flow for waiting, progress, result settlement, cancellation, and **inclusive spend reporting** without copying the child's private transcript into the parent.
- State accurately which limits Dashi enforces per Turn and which family-wide spending decisions need a host ledger. Preserve token, cost, unknown-price, and tool-cost distinctions.

## Non-goals for this slice

No general agent graph, parallel scheduler, background supervisor, automatic inheritance of history/tools/permissions/actor, cross-host task transport, or built-in conversation store. No claim that `Budget` enforces one cap over all descendants. Billing balances, credit reservation, and authorization of a parent-child relationship stay with the application. Multi-child concurrency and grandchildren are valid future designs, but the reference flow uses **one active child per parent tool call**.

## Proposed smallest contract and user flow

1. Add a value such as `ParentToolCall{TurnID, CallID}` to `Start`, pin it in `StartFact`, and project it on `TurnView`. The relation is optional for root Turns and immutable once started. A repeated `Start` with a different parent is a command conflict. `Replay` must reveal the same link. Dashi checks syntax and immutability; the host checks that the named parent call exists and the caller may delegate from it.
2. Use the existing **external tool** and `Settle` path for the reference adapter. The parent records the pending `delegate` call before child launch. The host derives or durably stores one child ID from `(parent Turn ID, tool call ID)`, loads it on retry, and starts it with `Start.Parent`. The child runs through ordinary `Runner.Run`/`Continue`; its own Turn record is the recovery source. While it runs, the parent remains waiting. When it finishes, the host settles that same parent call with the child's short answer or an explicit failure, then continues the parent. A child pause stays a child pause; it is not turned into a fabricated final answer. The application may expose child events with both Turn IDs for live presentation.
3. Offer a documented aggregation recipe or small pure helper that accepts explicitly loaded Turns and reports `ParentSpend`, `ChildSpend`, and `InclusiveSpend`, counting each child ID once. Preserve `Spend.Cost`, `ToolCost`, `Estimated`, `CostUnknown`, and `CostPartial` separately; never stuff child model charges into `tool.Outcome.Cost`. Do not fold child facts into the parent's `Turn.Steps` or `TurnView.Spend` silently.
4. For a cost ceiling over parent plus child, the host uses `Runner.Hooks.BeforeSend` and `SendDecision.Admission` with its Recorder-backed reservation for every model call in the family (`hooks.go:212-239`). The reference adapter must show the remaining authorized amount passed to a child's own `Budget` and the family ledger checking both Turns. If price/usage is unknown, report an unenforceable or paused state rather than claiming the cap held. The core `Budget` remains scoped to one Turn (`budget.go:22-31`).
5. Cancellation is explicit: the host records/carries a request to cancel the child, issues child `Cancel`, then settles or stops the parent only after it knows the child outcome. A live context cancellation should reach a synchronous child promptly, but a process crash is reconciled from records; a timed-out cancel is **requested/unknown**, not “cancelled.” The design must specify whether parent `Cancel` with a waiting child leaves a pending child for host reconciliation, and make that state visible.

Illustrative host flow, using proposed `Parent` field and existing commands (names are sketches):

```go
// parent is waiting on a recorded external delegate call.
childID := stableChildID(parent.ID, call.ID) // save or derive before dispatch
child := loadOrNewTurn(childID)
if len(child.Steps) == 0 {
    childOutcome, err = childRunner.Run(ctx, child, dashi.Start{
        CommandID: childID + ":start",
        Parent: &dashi.ParentToolCall{TurnID: parent.ID, CallID: call.ID},
        Input: childInput,
        Budget: childBudget,
    })
} else {
    childOutcome, err = resumeChild(ctx, child)
}
// On terminal child outcome, Settle the recorded parent call by call.ID.
```

## Acceptance criteria

- A child `Start` persists the exact parent Turn and tool call IDs; `Replay` and resumed reads show them. Retrying the same child start cannot change parentage or create another Turn. A host can identify a child after losing the start acknowledgement.
- The example verifies the parent external call is recorded before launching a child, reuses a stable child ID on retry, and does not launch twice after a crash at each boundary (before child start, after start, after child completion, before parent settlement).
- Parent stays waiting while a child is running or suspended. Child completion/failure produces exactly one settlement for the original parent call; retried settlement does not repeat the child or its model calls. The parent's model sees only the chosen result, not the child's private transcript.
- A cancellation test covers live parent cancellation reaching the child and a restart case where cancellation is requested but unconfirmed; neither is reported as completed before the child record supports it.
- Inclusive spend includes partial/interrupted model use and child tool cost exactly once, preserves unknown-price flags, and has a test with child resume. The example makes it clear that parent `Budget` alone is not a family cap. A host-ledger test denies a child or later parent model call when combined authorized spend would exceed the family limit.
- A Mobius-style `explore` flow can display child progress and result from recorded events, resume a suspended child, and reconcile billing by stable child/model-call identities without parsing a nested tool-result transcript.

## Risks and open decisions

The two Turn records are not one atomic transaction. The safe launch order plus stable child ID handles replay, but production hosts still need a durable index/ledger and authority checks. Decide whether a tiny optional `ChildIndex`/aggregation interface is worth adding after the Mobius adapter proves the read path; `Recorder` only loads by Turn ID today (`recorder.go:67-83`). Decide whether cancelling a parent should automatically request cancellation of children in a later host coordination package. Family-wide cap enforcement under concurrent descendants or unknown pricing needs a separate reservation design; this proposal does not smuggle it into `Budget`.
