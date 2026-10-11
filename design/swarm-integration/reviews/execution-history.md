# Execution and history: findings and decisions

[Review index](README.md) · [Examples](../examples/README.md)

The reviewer favored small APIs that remove interpretation. The broad driver and
summary coordinator concealed application policies and had not earned their size.

## Findings incorporated

- **R05, existing protection:** Session already rejects pending approvals/tools
  and unresolved effects. The example now uses a safely resumable model-call
  limit. Turn identity and command retry identity are separate. Optional ordered
  IDs and a close-or-resume policy remain useful; a mandatory Store is not justified.
- **R01, ownership:** Both admission and cancellation now visibly pass WithOwner.
  Resolved settings and compaction-window evidence must survive missing keys.
- **R17, interaction:** Enabling ActiveCompaction currently requires a Client,
  even with an explicit window. Its adoption cannot silently break credential-free
  queued cancellation. The example and recommendations require qualification first.
- **R02, smaller contract:** End references the saved stop sequence rather than
  accepting a replacement Stop. Stale references fail; exact retries and late
  results retain their existing semantics. The caller cannot invent completion.
- **R03, vocabulary and timing:** Saved failure facts extend Next. The sketch no
  longer invents Decision categories. Retry authorization checks send evidence;
  due time comes from the recorded failure time, so restart does not reset it.
- **R04, hidden adapters:** The deferred experiment now shows journal, original
  Start/history, owner and application planner. Stable commands deduplicate saved
  records, not all remote effects. Its test proves a saved Start retry, not a new driver.
- **R15, existing alternative:** BuildHistory already permits a suffix beginning
  at a completed summary. The example uses a query from a validated applied summary.
  No new lazy source is needed for the illustrated 13-of-100 read count. Strict
  history cannot independently validate records the caller omitted.
- **R16, unequal comparison:** A pure validation/activation-value helper replaces
  the coordinator. The app still owns job creation/reuse, summary execution,
  frozen terms, source revision, account, deadline and atomic activation.
- **R07/R08, scope:** Compare an application destination table before a registry.
  Keep a typed setting-preparation error; reuse Setting/Adjustments instead of
  introducing a general settings negotiation framework.

## Decision for every recommendation

| ID | Decision | Application benefit or next proof |
| --- | --- | --- |
| R01 | Refine | Obvious record-only operation; preserve ownership and resolved evidence. |
| R02 | Refine | Remove cancellation/reason translation while referencing the saved stop. |
| R03 | Refine | Retain safe failure facts and stable retry timing without another decision API. |
| R04 | Defer | Compare the full journal/planner adapter before adding a driver. |
| R05 | Refine | Optional app IDs and explicit treatment of resumable stops. |
| R06 | Keep | Initial context removes an initialization hook and sequence guard. |
| R07 | Defer registry | An application table is a smaller first alternative. |
| R08 | Keep narrowly | Reject only the unsupported setting; retain unrelated preparation errors. |
| R14 | Keep | Discoverable strict integrity policy for supplied records. |
| R15 | Use existing API | Query a trusted summary suffix; measure before adding loaders. |
| R16 | Narrow; defer coordinator | Pure validation/activation data before durable job abstractions. |
| R17 | Qualify existing API | Test client-free admission interaction, budgets, tools and images. |
| R18 | Keep existing API | Stable conversation CacheKey; no promised hits or savings. |

**Follow-up review:** verified on 2026-10-11 by the same independent reviewer.
All substantive findings and final example corrections are addressed; no
unresolved substantive issue remains in this batch. This accepts the review
proposal, not the correctness of an unimplemented API.
