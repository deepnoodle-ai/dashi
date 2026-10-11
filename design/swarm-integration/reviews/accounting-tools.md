# Accounting and tools: findings and decisions

[Review index](README.md) · [Examples](../examples/README.md)

The reviewer found useful model-cost interpretation and a concrete MCP mismatch.
Short transaction methods had concealed the rules required for the stated benefits.

## Findings incorporated

- **R09/R26, repeated writes:** Transaction sketches now check owner, exact
  duplicate/conflict, then sequence. Exact duplicates return before projection or
  account/input effects. Projection uses the matching prefix; unknown acknowledgment
  requires reload. Those rules remain application storage work.
- **R09, evidence versus postings:** Dashi can classify per-call cost and old/new
  totals. It cannot choose charges/refunds without account balances, units, rounding
  and policy. The proposal retains those host choices and does not delete settleAttempt.
- **R10, refusal sequence:** Continue is already saved before model-start rejection.
  Only a definitely rejected reservation/start write receives the special outcome.
  Runner saves a host-limit stop; retrying the old command returns saved state.
  Funding requires a fresh Continue. Saving the stop can itself need reload.
  BeforeSend remains the smaller solution for preflight checks.
- **R11, authority and display:** Verify a billing receipt before asserting final
  cost. Deduplicate evidence separately from CommandID and retain storage ownership.
  Cost finality does not imply final usage. Raw Spend differs from rounded account
  postings; the ledger overlay stays when the UI displays posted charges.
- **R12, sources:** The shared classifier preserves cost/usage sources, missing
  prices and finality. Posting policy also receives the admitted hold. An estimate
  is not a known lower bound. Settlement errors remain distinct from model errors.
- **R13, signed money:** Generic conversion must support refunds and explicit
  signed rounding. Negative model charges are a separate rule. Local quotient/
  remainder avoids addition overflow; defer a broad request estimate API.
- **R23/R24, smaller alternative:** Existing CallRef in application envelopes and
  Pending.Result remove reverse-hash scans/command construction. A batch planner
  adds types while keeping partial-save recovery and the loop; defer it.
- **R25, format/errors:** Select MCP conversion from the admitted tool format,
  never JSON shape. Preserve journal failure and MCP isError. Typed SDK-result
  conversion is the smallest addition; raw decoding and MIME/content validation
  need explicit handling. Resource descriptions/omissions are not full resource transfer.
- **R26, actual guarantee:** Input acknowledgment means saved into the transcript,
  not dispatched to a model. A budget refusal can follow with zero sends. The
  existing Delivered callback still idempotently confirms the saved receipt.

## Decision for every recommendation

| ID | Decision | Application benefit or next proof |
| --- | --- | --- |
| R09 | Refine | Remove record scanning/classification, keep posting and database rules. |
| R10 | Refine | Supported definite funding-refusal race, with complete retry sequence. |
| R11 | Refine | Accounted correction of finished-call cost, preserving execution and authority. |
| R12 | Keep classifier; defer executor | Reuse cost rules while leaving admitted holds/settlement explicit. |
| R13 | Small conversion optional; defer estimates | Compare the local arithmetic before a broad API. |
| R23 | Use existing API | Store CallRef and use Pending.Result in application envelopes. |
| R24 | Defer batch API | Existing builder is enough for shown command construction. |
| R25 | Keep/refine | Fix the real MCP object/array mismatch with standalone validated conversion. |
| R26 | Refine receipts; defer adapter | Remove scans; acknowledge transcript persistence accurately. |

**Evidence added:** Current-API demos show an acknowledged input with zero
model sends and two nano-cost calls producing two micro-unit account postings.
They expose the guarantee boundary; they do not implement a ledger or new API.

**Follow-up review:** verified on 2026-10-11 by the same independent reviewer.
All substantive findings and final example corrections are addressed; no
unresolved substantive issue remains in this batch. This accepts the review
proposal, not the correctness of an unimplemented API.
