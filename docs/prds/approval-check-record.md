# Keep the check that informed an approval, out of the model's view

Status: Draft\
PRD PR: [#340](https://github.com/deepnoodle-ai/dashi/pull/340) · Implementation PR: stacked on #340 · Updated: 2026-10-29\
Issue: [#309](https://github.com/deepnoodle-ai/dashi/issues/309) · Design: [approval-check-record](../design/approval-check-record.md)

## Problem

A delegated task can stop with a bash call that waits for approval. The
operator answers it later with `/task ID continue`. Since #304 the CLI runs
the decide command-risk check on that recorded call, and the prompt shows
the answer, such as `decide: severe 91% — flagged`. The turn record does not
keep that answer. Later, an operator who asks "why did this command run?"
finds an approval with no trace of the check that the person saw.

The recorded path has no place for the answer. `Pending.Approve()` and
`Pending.Deny(reason)` build an `ApprovalDecision` that has only `Reason`,
and the model reads `Reason` as the result of a denied call.

The live path keeps the answer in the wrong place. The CLI appends
`; decide: severe 91% flagged (…)` to `ToolDecision.Reason`, which a denial
also gives to the model. The model must not read the check's scores: a
model that follows injected instructions could use them to shape a command
below the threshold.

## Users and job

- **Primary: an operator who inspects a turn record later**, in the CLI
  (`/task ID`, a reopened session) or in a host's own tools. The job: see
  which check informed each approval or denial, and what it found, on the
  live and the recorded path alike.
- **Secondary: a host developer** who decides held calls with
  `Pending.Approve` and `Pending.Deny` after its own check, such as a
  policy service or a decision model. The job: record that check with the
  decision, without a side table.

## Concepts

- **Rules version**: the existing audit name of the rules behind a
  decision, such as `decide/jev-1.13.0` or `policy-v3`. `ToolDecision`,
  `ToolVerdict`, and `HookResult` already carry it. `ApprovalDecision` gets
  the same field: there the rules informed a person's decision.
- **Rules finding**: new. What those rules found, in one line, such as
  `severe 91% flagged (jev-1.13.0, request r_12)`. The record keeps it for
  later review. The model never reads it.

## Use cases

### UC-1: The CLI records the check on a recorded approval

The operator types `/task t_k3f2 continue`. The prompt shows
`decide: severe 91% — flagged`. The operator approves or denies.

Acceptance:
- [ ] The recorded `ApprovalDecision` has `RulesVersion` `decide/<model>`
      (or `decide/unchecked`) and `RulesFinding` set to the decide line,
      such as `severe 91% flagged (jev-1.13.0, request r_12)`.
- [ ] A denial's `Reason` is the operator's reason only.
- [ ] A call the check does not read (not workspace bash, or the check is
      off) records no rules version and no rules finding.

### UC-2: A live call records the check the same way

The operator answers a live bash prompt that decide checked.

Acceptance:
- [ ] `ToolView.Verdict` has the same `RulesVersion` and `RulesFinding`
      as UC-1 would record. `Reason` is the policy's or the operator's
      text, with no decide line.
- [ ] On a denial, the model's result for the call holds no decide text.

### UC-3: The operator inspects the record

Acceptance:
- [ ] `/task t_k3f2` shows one line for each recorded approval decision:
      call ID, tool, `approved` or `denied`, and the decide answer in the
      tool row's words, such as `decide command: severe 91% — flagged`. A
      decision with no check shows no decide text.
- [ ] A reopened session shows the same tool-row marks and footer counts
      for live calls as before this change. The footer covers only the
      session's own turns, which never hold a recorded approval.

### UC-4: A host records its own check

```go
cmd := p.Approve().WithRules("policy-v3", "deploy window open")
out, err = runner.Run(ctx, out.Turn, cmd)
// out.Tools[p.ID].Approval.RulesFinding == "deploy window open"
```

Acceptance:
- [ ] `Approve()` and `Deny(reason)` keep their signatures. A decision
      with empty new fields records exactly what it records today.
- [ ] `WithRules` on a provider tool approval returns the command
      unchanged, with no panic.
- [ ] The fields survive a reload: replay gives the same
      `ToolView.Approval`.
- [ ] No model request holds the rules finding or the rules version.
- [ ] A retried `Resolve` whose rules fields differ is a duplicate, not
      `ErrCommandIDReused`; the first recorded values stay.

## Requirements

- R-1: `ApprovalDecision`, `ToolDecision`, and `ToolVerdict` carry
  `RulesFinding`; `ApprovalDecision` also gains `RulesVersion`. Zero
  values mean "no check recorded". `Resolve.WithRules` sets both on an
  approval decision.
- R-2: Dashi never puts a rules version or a rules finding in a model
  request.
- R-3: The CLI writes the decide line to `RulesFinding` on both paths,
  never to `Reason`.
- R-4: `/task ID` shows recorded approvals with their checks (UC-3).
- R-5: The approvals and hooks guides show the fields. The CLI tasks and
  decide guides say where each path keeps the check.

## Not in scope

- Provider tool approvals. Their answer goes to the provider; the CLI
  does not check them.
- `RulesFinding` on `HookResult`, `ModelDecision`, and `ResultDecision`.
  Their `Reason` never reaches the model, so no hook needs a second line.
- Decide spend (tokens and cost of the check). A parallel track designs
  it; this change adds no usage fields.
- Validation of the fields' content. They are audit text.

## Decisions

- **Two flat fields, not a nested check struct.** `RulesVersion` matches
  the other decision facts. Rejected: a `Check *ApprovalCheck` struct.
- **Fix the live path in this change.** The rule "the model never reads
  the check" must hold on the common path too. Rejected: leave the live
  reason as is and defer.
- **One chained method, `Resolve.WithRules`.** Setting `r.Approval.…`
  directly panics on a provider tool approval, which `out.Pending` lists
  with the same kind. Rejected: `ApproveWith`/`DenyWith` or options.
- **The rules fields are left out of the command hash**, as
  `Continue.OnHistoryMismatch` is. A host that reruns its check on a retry
  gets a new request ID; the retry must still count as a duplicate.
  Rejected: hash them, and make hosts save the command first.
- **No compatibility work.** Dashi has no users yet. Old CLI records that
  hold `; decide: …` in `Reason` lose their decide marks.

## Success

An operator who opens a record can tell, for every approval or denial,
whether decide checked the command and what it found. No model request
holds a decide score.

## Risks and open questions

- The non-interactive refusal in bypass mode tells the model "the decide
  command-risk check flagged this command (severe 91%)". That text is the
  model's instruction, not a record; this change drops the score from it
  and keeps the rest. Not blocking.
