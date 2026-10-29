# Recording the check behind a tool decision

Status: Proposed · Author: Claude · Date: 2026-10-29\
Requirements: [Keep the check that informed an approval](../prds/approval-check-record.md) · Issue: #309

## Library change

```go
// turn_facts.go
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

// hooks.go: ToolDecision and ToolVerdict gain the same field.
RulesFinding string `json:"rules_finding,omitempty"`

// command.go
// WithRules records, in the approval decision r carries, the rules that
// informed it and what they found. It copies the decision, so a Resolve
// the caller kept does not change. A command with no approval decision,
// such as a provider tool approval, is returned unchanged.
func (r Resolve) WithRules(version, finding string) Resolve
```

`Pending.Approve()` and `Pending.Deny(reason)` do not change:

```go
cmd := p.Deny("The operator declined.").
	WithRules("decide/jev-1.13.0", "severe 91% flagged (jev-1.13.0, request r_12)")
```

`WithRules` exists because `Resolve.Approval` is nil for a provider tool
approval, which `out.Pending` also lists as `PendingApproval`; direct
assignment in a loop over `out.Pending` would panic there.

**Tool verdict.** `authorizeTool` (`hooks_tool.go`) copies the deciding
hook's `RulesFinding` into `ToolVerdict`, beside `RulesVersion`. The model
reads a denied call's `Verdict.Reason`, never the rules finding. `HookResult`
is unchanged: the verdict holds the deciding hook's rules finding.

**Record and replay.** `Run` copies `Resolve.Approval` into the
`StepApprovalDecided` step, and replay copies it into `ToolView.Approval`;
`ToolFact.Verdict` is copied whole too. The fields ride along. The keys are
optional, so old records replay unchanged. No shims.

**The model.** The only reader that turns a decision into model input is
the denial in `run_tools.go`, which reads `Reason` alone. Tests pin that
neither rules field reaches a model request, on either path.

**Command identity.** Two functions hash a command: the root
`commandHash` (`command.go`) and `delegate.commandHash`
(`delegate/control.go`), which must match it for `Tasks.Run` retries. Both
leave the rules out the same way: copy, then blank. Each calls
`v.WithRules("", "")`, which copies the `ApprovalDecision` and blanks the
copy's `RulesVersion` and `RulesFinding`; neither writes through the
caller's `Approval` pointer. This matches how `Continue.OnHistoryMismatch`
is left out. A retry with a rerun check is a duplicate; the first recorded
values stay.

## CLI change

Both paths call the same helpers in `decide_check.go`:
`result.rulesVersion()` (`decide/<model>` or `decide/unchecked`) and
`result.recordLine()`, the decide line
(`severe 91% flagged (jev-1.13.0, request r_12)`).

- **Live** (`attributeToolHooks`): sets `d.RulesVersion` and
  `d.RulesFinding`. `Reason` stays the policy's text.
- **Recorded** (`taskAction`, `continue`): `recordedApproval` already puts
  a `decideSlot` in the prompt's context, and the prompt fills it. After
  `c.Approve`, the CLI takes the slot and calls
  `response.WithRules(r.rulesVersion(), r.recordLine())`.

`withDecide`, `splitDecide`, and `decideReasonSep` go away.
`markFromRecord(rulesVersion, finding string)` builds the mark only when
the version starts with `decide/` and the rules finding is set. An old
record keeps its decide line in `Reason` and has no rules finding, so it
shows no mark. The tool row and the footer counts read
the verdict as before: the CLI draws neither for child turns, the only
turns with recorded approvals.

`/task ID` (`taskDetails`) adds one line per recorded approval decision:

```
Recorded approval: call c1 · bash · denied · decide command: severe 91% — flagged
```

The bypass-mode refusal with no one to ask drops the score: "The decide
command-risk check flagged this command, and no one can approve it…". The
model no longer sees a decide score or request ID, which reverses the
earlier CLI behavior, because the model cannot act on a score and both
approval paths now behave the same.

## How the two paths line up

| Fact | Live call | Recorded approval |
| --- | --- | --- |
| Where | `ToolView.Verdict` | `ToolView.Approval` |
| Rules version | `decide/<model>` | `decide/<model>` |
| Decide line | `RulesFinding` | `RulesFinding` |
| Model sees it | never | never |

## Tests

- Root: approve and deny with `WithRules`; the view and a reloaded turn
  show both fields; no model input holds them; a denial's result is the
  reason alone; a hook's `RulesFinding` lands in `ToolVerdict` and not in
  the model's denial result; a retry with other rules values is a
  duplicate; `WithRules` skips a provider approval and copies; hashing a
  `Resolve` leaves the caller's rules fields in place.
- CLI: the recorded path records both fields on approve and deny, and the
  `taskDetails` line; the live path records the rules finding outside
  `Reason`; the bypass refusal holds no score; a `decide/` version with no
  rules finding builds no mark.
- Delegate: the task hash of an approval with rules equals the recorded
  `Step.CommandHash`, and hashing leaves the caller's rules fields in
  place.

## Docs

`docs/guides/approvals.md` (`WithRules`), `docs/guides/hooks.md`
(`RulesFinding`), `docs/guides/cli-tasks.md`, `docs/guides/cli-decide.md`
(where each path keeps the check), `CHANGELOG.md`.

## Alternatives

- **Decide line in `Reason`.** The model reads it on a denial. Rejected.
- **Fix only the recorded path.** Leaves the common path sending scores.
  Rejected.
- **`ApproveWith`/`DenyWith`, or options.** Two builders for one concern.
  Rejected.
- **A side record in the CLI session store.** Splits one decision across
  two records, and a host gets nothing. Rejected.
