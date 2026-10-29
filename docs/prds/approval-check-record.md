# Keep the check that informed a recorded approval

Status: Draft\
PRD PR: TBD · Implementation PR: TBD · Updated: 2026-10-29\
Issue: [#309](https://github.com/deepnoodle-ai/dashi/issues/309) · Design: [approval-check-record](../design/approval-check-record.md)

## Problem

A delegated task can stop with a bash call that waits for approval. The
operator answers it later with `/task ID continue`. Since #304 the CLI runs
the decide command-risk check on that recorded call, and the prompt shows
the answer, such as `decide: severe 91% — flagged`. The turn record does not
keep that answer. Later, an operator who asks "why did this command run?"
finds an approval with no trace of the check that the person saw.

A live call keeps the answer: the CLI writes the decide line into the
`ToolDecision` reason and sets `RulesVersion`. The recorded path cannot do
the same. `Pending.Approve()` and `Pending.Deny(reason)` build an
`ApprovalDecision` that has only `Reason`, and the model reads `Reason` as
the result of a denied call. A check result in `Reason` would go to the
model, which must never read the check's scores: a model that follows
injected instructions could use them to shape a command below the
threshold.

## Users and job

- **Primary: an operator who inspects a turn record later**, in the CLI
  (`/task ID`) or in a host's own tools. The job: see which check informed
  each approval or denial, and what it found, on the recorded path as on
  the live path.
- **Secondary: a host developer** who approves held calls with
  `Pending.Approve` and `Pending.Deny` after its own check, such as a
  policy service or a decision model. The job: record that check with the
  decision, without a side table.

## Concepts

- **Rules version**: the existing audit name of the rules behind a
  decision, such as `decide/jev-1.13.0` or `policy-v3`. `ToolDecision`,
  `ToolVerdict`, and `HookResult` already carry it. `ApprovalDecision` gets
  the same field: there the rules informed a person's decision.
- **Rules finding**: new. What those rules found, in one line, such as a
  check's score. The record keeps it for later review. The model never
  reads it, for an approval or a denial.

## Use cases

### UC-1: The CLI records the check on a recorded approval

The operator types `/task t_k3f2 continue`. The prompt shows
`decide: severe 91% — flagged`. The operator approves or denies.

Acceptance:
- [ ] The recorded `ApprovalDecision` has `RulesVersion`
      `decide/<model>` (or `decide/unchecked`), and `RulesFinding`
      `decide: <line>`, where `<line>` is the same text the live path
      records, such as `severe 91% flagged (jev-1.13.0, request r_12)`.
- [ ] A denial's `Reason` is the operator's reason only. The model's
      result for the denied call holds no decide text.
- [ ] A call the check does not read (not workspace bash, or the check is
      off) records no rules version and no rules finding.

### UC-2: The operator inspects the task

The operator types `/task t_k3f2`.

Acceptance:
- [ ] Each tool call with a recorded approval decision shows one line:
      call ID, tool, `approved` or `denied`, and the decide answer in the
      words the tool row uses, such as `decide command: severe 91% —
      flagged`.
- [ ] A decision with no check shows no decide text.

### UC-3: A host records its own check

```go
cmd := p.Approve().WithRules("policy-v3", "allowed: deploy window open")
out, err = runner.Run(ctx, out.Turn, cmd)
// out.TurnView.Tools[p.ID].Approval.RulesFinding == "allowed: deploy window open"
```

Acceptance:
- [ ] `Approve()` and `Deny(reason)` keep their signatures. A decision
      with empty new fields records exactly what it records today.
- [ ] `WithRules` on a provider tool approval returns the command
      unchanged, with no panic.
- [ ] The fields survive a reload: replay of the record gives the same
      `ToolView.Approval`.
- [ ] No model request in the turn holds the rules finding or the rules
      version.

## Requirements

- R-1: `ApprovalDecision` gains `RulesVersion` and `RulesFinding`, both
  optional strings. Zero values mean "no check recorded". `Resolve.WithRules`
  sets them.
- R-2: Dashi never puts either field in a model request.
- R-3: The CLI fills both fields on the `/task ID continue` path from the
  check it already runs, in the format the live path uses.
- R-4: `/task ID` shows each recorded approval decision with its decide
  answer.
- R-5: The approvals guide shows how a host fills the fields; the CLI
  tasks and decide guides say the record keeps the check.

## Not in scope

- Provider tool approvals. Their decision is the provider's
  `ProviderToolApprovalResponse`, which goes to the provider; the CLI does
  not check them.
- The live path's record format. It is unchanged (see Risks).
- Decide spend (tokens and cost of the check). A parallel track designs
  how Decide spend counts toward a turn; this change adds no usage fields.
- Validation of the fields' content. They are audit text, like
  `RulesVersion` elsewhere.

## Decisions

- **Two flat fields, not a nested check struct.** `RulesVersion` matches
  the other decision facts, so readers use one word for one meaning.
  Rejected: a `Check *ApprovalCheck` struct, which adds a type for two
  strings.
- **A new field, not `Reason`.** `Reason` is model-facing on a denial.
  Rejected: put the decide line in `Reason`, as the issue warns.
- **One chained method, `Resolve.WithRules`.** `Approve()` and `Deny()`
  keep their signatures. Setting `r.Approval.…` directly would panic on a
  provider tool approval, which `out.Pending` lists with the same kind.
  Rejected: direct assignment as the documented way, and
  `ApproveWith(...)`/`DenyWith(...)` or options, which add two builders.
- **The rules finding holds the same `decide: <line>` text as the live
  reason.** The CLI parses both with one function (`splitDecide`).
- **No compatibility work.** Dashi has no external users; old records
  replay unchanged because the new keys are optional.

## Success

An operator who opens a task's record can tell, for every approval or
denial, whether decide checked the command and what it found. The model's
input does not change.

## Risks and open questions

- **The live path still shows the decide line to the model on a
  denial.** There, the CLI appends `; decide: …` to `ToolDecision.Reason`,
  which a denial gives to the model. Moving it out needs the same audit
  field on `ToolDecision` and `ToolVerdict`. This change does not do that;
  it is a follow-up question for Curtis (not blocking).
- **A retried `Resolve` must carry the same fields.** Like `Reason`, the
  new fields are part of the command, so a retry with a different note gets
  `ErrCommandIDReused`. The CLI sends each decision once; the guide tells
  hosts to save the command before they send it.
