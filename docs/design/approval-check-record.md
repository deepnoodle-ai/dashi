# Recording the check behind an approval decision

Status: Proposed · Author: Claude · Date: 2026-10-29\
Requirements: [Keep the check that informed a recorded approval](../prds/approval-check-record.md) · Issue: #309

## Library change

Two fields on `ApprovalDecision` (`turn_facts.go`), and one method on
`Resolve` (`turn_run_result.go`):

```go
type ApprovalDecision struct {
	CallID   string `json:"call_id"`
	Approve  bool   `json:"approve"`
	ArgsHash string `json:"args_hash"`
	// Reason explains a denial. The next Continue gives it to the model as
	// the denied call's result. It is the only field the model sees.
	Reason string `json:"reason,omitempty"`
	// RulesVersion names the rules that informed the decision, such as a
	// policy or decision model version. Recorded; never shown to the model.
	RulesVersion string `json:"rules_version,omitempty"`
	// RulesFinding is what those rules found, in one line, such as a
	// check's score. Recorded; never shown to the model.
	RulesFinding string `json:"rules_finding,omitempty"`
}

// WithRules records the rules that informed an approval decision, and what
// they found, in the decision r carries. It returns r unchanged for a
// command without one, such as a provider tool approval, whose answer goes
// to the provider.
func (r Resolve) WithRules(version, finding string) Resolve
```

`Pending.Approve()` and `Pending.Deny(reason)` do not change:

```go
cmd := p.Deny("The operator declined.").
	WithRules("decide/jev-1.13.0", "decide: severe 91% flagged (jev-1.13.0, request r_12)")
```

`WithRules` copies the `*ApprovalDecision` before it sets the fields, so a
`Resolve` value the host kept does not change under it. It exists because
`Resolve.Approval` is nil for a provider tool approval, which `out.Pending`
also lists as `PendingApproval`; setting the fields directly in a loop over
`out.Pending` would panic there. The fields stay exported, since the record
is JSON.

Nothing else in the root changes:

- **Record and replay.** `Run` copies `Resolve.Approval` into the
  `StepApprovalDecided` step; replay copies the step's decision into
  `ToolView.Approval`. Both copy the whole struct, so the fields ride along.
  JSON keys are optional, so old records replay unchanged. No shims.
- **The model.** The only reader of `ApprovalDecision` that feeds the model
  is the denial in `run_tools.go`, which reads `Reason` alone. A test pins
  that neither new field reaches a model request, for an approval and a
  denial.
- **Command identity.** The fields are part of the `Resolve` command hash,
  like `Reason`. A retry must send the same values.

## CLI change

`/task ID continue` (`taskAction`, `cmd/dashi/task_control.go`) already puts
a `decideSlot` in the prompt's context through `recordedApproval`, and the
prompt fills it. Today nobody reads it. After `c.Approve` returns, the CLI
takes the slot and, if it holds a result, calls:

```go
response = response.WithRules(result.rulesVersion(), withDecide("", result.recordLine()))
// "decide/jev-1.13.0", "decide: severe 91% flagged (jev-1.13.0, request r_12)"
```

These are the helpers the live path uses in `attributeToolHooks`, so both
paths write the same rules version and the same line. A denial's `Reason`
stays the operator's text.

**Rendering.** `/task ID` (`taskDetails`) adds one line per tool call with a
recorded approval decision:

```
Recorded approval: call c1 · bash · denied · decide command: severe 91% — flagged
```

The decide part comes from `markFromRecord`, fed a `ToolVerdict` built from
the decision (`RulesVersion`, and `RulesFinding` as the reason), so
`splitDecide` parses both paths. A decision with no check ends after
`approved` or `denied`.

## How the two paths line up

| Fact | Live call | Recorded approval |
| --- | --- | --- |
| Where | `ToolView.Verdict` (from `ToolDecision`) | `ToolView.Approval` (`ApprovalDecision`) |
| Rules version | `decide/<model>` | `decide/<model>` |
| Decide line | `Reason`, after `; decide: ` or as `decide: …` | `RulesFinding`, as `decide: …` |
| Model sees the line | on a denial, yes | never |

The one gap is the last row. Closing it means a `RulesFinding` on
`ToolDecision` and `ToolVerdict`, set by `attributeToolHooks` instead of the
reason. That changes the hook API and the live record, so it is left as a
follow-up decision.

## Tests

- Root: approve and deny with `WithRules`; the `RunResult` view and a
  replayed `turn.View()` show both fields; a `dashitest` model's requests
  hold neither string; a denial's result is the reason alone; empty fields
  encode no keys; `WithRules` on a provider tool approval returns it
  unchanged and does not change a kept `Resolve`.
- CLI: extend the recorded-approval test: run `taskAction`'s answer path
  with a decide harness, deny and approve, and check the recorded decision
  and the `taskDetails` line.

## Docs

`docs/guides/approvals.md` (`WithRules`), `docs/guides/cli-tasks.md` and
`docs/guides/cli-decide.md` (the record keeps the check), `CHANGELOG.md`.

## Alternatives

- **Decide line in `Reason`.** The model reads it on a denial. Rejected.
- **Set the fields directly on `r.Approval`.** Panics on a provider tool
  approval. Rejected as the documented way.
- **`ApproveWith(...)` and `DenyWith(...)`, or options.** Two new builders
  for one concern. Rejected.
- **A side record in the CLI session store.** Splits one decision across
  two records, and a host gets nothing. Rejected.
