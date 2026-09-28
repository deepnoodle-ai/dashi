# Run results and approvals

**Status:** Proposal · **Date:** 2026-09-28 · **Addresses:**
[F](gaps/F-example-ports.md) 4.1, 4.2, 8.1, 8.2, 9.4, 9.5, 4.3; B rows 2 and 7

## Problem

After `Run`, a caller reads the result from four places: the Go error,
`Outcome.RecordingErr()`, `Outcome.Turn.AnswerText()`, and `dashi.Replay(turn)`
for spend and call details. Pending work is a list of bare IDs. Approving a
call means copying an argument digest and a policy revision into an
`ApprovalFact`, naming the call ID twice, inventing a command ID, and then
calling `Continue`. A denial reaches the model as "Tool was not executed.", so
the approval example delivers a separate note explaining it. The example is
about 220 lines; Dive's is 60.

## Decisions

**1. One error channel (4.1).** `Run` returns a recording failure as its Go
error, together with a usable `Outcome`. The recorder's `CommitError` gains a
`Step` field that `Run` fills with the step it could not record. `Outcome`
loses `Persistence`, `Unacknowledged`, `WriteError`, and `RecordingErr()`;
`PersistenceAcknowledged` goes away, because a success is a nil error.

| `Run` returns                       | Meaning                                                                                                                                                                       | `Outcome`                         |
| ----------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------- |
| `nil`                               | Every step was recorded.                                                                                                                                                      | Complete                          |
| `*CommitError`, `PersistenceRejected` | The turn ran, but `Step` was not saved. The record ends at `Outcome.Turn`. Work after the last saved step (a model call or tool run) may have happened; recovery treats it as a crash at that point. | The saved record and its view     |
| `*CommitError`, `PersistenceUnknown`  | As above, but the write may have been saved. Load the turn before acting.                                                                                                   | The last record known to be saved |
| Any other error                     | The command was rejected before anything was recorded. Nothing ran.                                                                                                           | Zero value                        |

So `if err != nil { return err }` is always safe, and a host that recovers
uses `errors.As(err, &commitErr)`. `Stream` yields its final `Outcome` event
with the same error in one pair. Execution failures stay in `Outcome.Stop`, as
today: a failed or limited turn is a result, not an error.

**2. The Outcome is the turn plus its view (4.2).** `Outcome` embeds
`TurnView`, so `out.Stop`, `out.Output`, `out.Warnings`, and
`out.ReadyToContinue` keep working, and `out.Spend`, `out.Tools`,
`out.Budget`, and the new fields below appear without a replay. `Replay`
returns the same view and stays the tool for a loaded record. Fields hold
state that `Run` computes anyway; methods on `Turn` walk the transcript on
demand.

```go
type Outcome struct {
	Turn Turn // the saved record; pass it to the next Run
	TurnView  // what the record means now
}

// Added to or changed on TurnView:
Answer  string    // text of the final assistant answer
Pending []Pending // typed, in call order (was []string)
Stop    Stop      // Replay now projects waiting/interrupted/uncertain as Run does
```

`Turn.AnswerText()` and `Turn.StructuredOutput()` are removed: they replayed
the turn to answer what the view already holds (`out.Answer`, `out.Output`).
Because `Answer` is on the view, `Policy.OnStop` also sees the candidate
answer. A policy's view is taken mid-run, so its `Stop` stays empty.

**3. Pending items build their own answers (8.1).** Each pending item carries
what a host needs and builds the exact `Settle` command:

```go
type Pending struct {
	Kind   PendingKind                // PendingApproval, PendingTool, PendingModel
	ID     string                     // call, hosted request, or attempt ID
	Tool   *ToolView                  // local call: name, args, status, decision
	Hosted *llm.HostedApprovalRequest // provider-hosted approval request
}

func (p Pending) Approve() Settle
func (p Pending) Deny(reason string) Settle
func (p Pending) Result(outcome tool.Outcome) Settle // for waiting external calls
```

`Approve` and `Deny` fill the call ID, argument digest, and policy revision
for a local call, or the request ID and origin for a hosted one. The command
ID is derived from the call (`approval:<id>`, `result:<id>`), so a retried
answer is recognized as a duplicate, and a second, different answer to the
same call fails with `ErrCommandConflict`. The digest and revision binding is
unchanged; callers stop handling it. `PendingTool` covers local calls that are
waiting, started, or uncertain (read `Tool.Status`); `PendingModel` covers a
model attempt awaiting reconciliation. Hosted requests get a stable order
(today they come from a map).

**Settle and Continue stay separate** (decision 18). A combined
`Continue{Settle: []Settle}` would save one call per round in a CLI, but it
brings partial-commit rules (some answers saved, the continue not) into the
command digest, and a server records answers in one request and continues in
another anyway. `Continue` returns an error when the turn is not ready, so the
`ReadyToContinue` check disappears from the loop. See question 1.

**4. A denial carries its reason to the model (8.2).** `ApprovalFact` gains
`Reason`. A denied call is settled as `StateNotExecuted` with the text
`The call was denied: <reason>` as its output, which the model sees as the
tool result; an empty reason gives `The call was denied.` The same default
replaces the empty text a `DecisionDeny` without a reason produces today.
Recorded format: new `approval.reason`; the denial's `tool_settled` step gains
output and carries the reason in `tool.reason` instead of `approval denied`.

**5. Answer text joins blocks sensibly (9.4).** Adjacent text blocks are
concatenated as is: providers split one sentence into several blocks around
citations. When a non-text item, such as a hosted tool use or result, sits
between two text blocks, they are joined with a blank line unless the first
already ends with a newline. When the provider paused (`StopPause`) and the
turn continued, the paused messages' text is part of the same answer and is
prepended.

**6. Turn-wide helpers (9.5).** Two methods on `Turn` walk every settled
assistant message, because hosted work can span several model calls:

```go
func (t Turn) Citations() []content.Citation // in order, as reported
func (t Turn) HostedUses() []llm.HostedUse   // each with its paired result
```

`HostedUses` merges a separate `HostedResult` (Anthropic's shape) into its
use's `Output` and `IsError`, so callers see one list of hosted calls either
way. `Citations` does not deduplicate: a source cited twice may carry two
excerpts.

**7. Restart deduplication is documented (4.3).** The running-turns guide
states that re-running a command returns the recorded result without repeating
model or tool work only when the same recorder holds the turn and the command
has the same `CommandID` and content; a different ID is a new command.

## The approval example, before and after

Before (the answer loop, 70 of 220 lines):

```go
for round := 1; outcome.Stop.Kind == dashi.StopWaiting; round++ {
	view, err := dashi.Replay(outcome.Turn)
	// ...error check...
	for _, id := range outcome.Pending {
		call := view.Tools[id]
		// ...status check, ask...
		outcome, err = apply(ctx, runner, outcome.Turn, dashi.Settle{
			CommandID: "answer-" + id,
			CallID:    id,
			Approval: &dashi.ApprovalFact{
				CallID:         id,
				Approve:        approve,
				ArgsDigest:     call.ArgsDigest,
				PolicyRevision: call.Decision.Revision,
			},
		})
		// ...on denial, Deliver a note so the model knows why...
	}
	if !outcome.ReadyToContinue { /* ... */ }
	outcome, err = apply(ctx, runner, outcome.Turn, dashi.Continue{
		CommandID: fmt.Sprintf("continue-%d", round),
	})
}
fmt.Printf("Stop: %s\n%s\n", outcome.Stop.Kind, outcome.Turn.AnswerText())
// plus an apply helper that checks both error channels
```

After (IDs and the tool list assume the zero-config and tool-ergonomics
streams):

```go
out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{Input: input})
for err == nil && out.Stop.Kind == dashi.StopWaiting {
	for _, p := range out.Pending {
		answer := p.Deny("The operator declined.")
		if ask(in, p.Tool.Call) {
			answer = p.Approve()
		}
		if out, err = runner.Run(ctx, out.Turn, answer); err != nil {
			return err
		}
	}
	out, err = runner.Run(ctx, out.Turn, dashi.Continue{})
}
if err != nil {
	return err
}
fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Answer)
```

The whole example drops to about 110 lines, of which the deferring policy is
15 (question 2). `tools`, `hosted_tools`, and `resume` lose their `Replay`
calls and `apply` helpers; `hosted_tools` replaces its step walk with
`Citations` and `HostedUses`.

## Tests

- A rejected and an unknown commit each return `*CommitError` with `Step` set
  and an `Outcome` whose turn is the saved prefix; a rejected command returns
  a plain error and a zero `Outcome`; `Stream` yields outcome and error
  together.
- `Approve` and `Deny` settle local and hosted approvals; a retried answer is
  a duplicate; a conflicting one is `ErrCommandConflict`; `Approve` on a
  non-approval item is rejected by `Run`.
- A denial's reason reaches the model: `Project(turn)` holds it as the tool
  result, and a scripted model receives it on `Continue`.
- `Pending` kinds and order, including hosted requests; `Replay` and `Run`
  report the same `Stop`; `out.Spend` equals the replayed spend.
- `Answer` across citations, hosted items, and a provider pause;
  `Citations` and `HostedUses` over multi-call turns.

## Files

`turn.go` (remove the two `Turn` methods, `ApprovalFact.Reason`), a new
`outcome.go` (`Outcome`, `Pending`, builders), a new `turn_content.go`
(answer, citations, hosted uses), `state.go` (typed pending, projected stop,
answer), `runner.go` (`Run` and `Stream` errors, `outcome()`, the denial
step), `recorder.go` (`CommitError.Step`), one line in `tools_loop.go` (the
default denial text), tests, the four owned examples plus the one-line
`RecordingErr` removal in `agent` and `structured_output`,
`internal/providertest`, `dashitest/recovery_test.go`,
`docs/guides/running-turns.md`, `docs/agent-turns.md`, and the `RecordingErr`
line in `design/steering.md`.

## Risks

- Removing `Outcome.Persistence` touches about 20 test files; the edits are
  mechanical (`err != nil`).
- Embedding makes `Outcome` a larger value; `Run` already built most of it.
- `runner.go` is shared with the zero-config stream (validation at the top of
  `run`); this stream edits `Run`, `Stream`, `outcome()`, and the approval
  branch of `Settle` only.

## Open questions

1. Should `Continue` accept answers (`Continue{Settle: []Settle}`) so a CLI
   answers and resumes in one call? Recommended: not now.
2. A small approval policy (for example `dashi.RequireApproval("deploy")`)
   would remove the example's 15-line policy type. It overlaps the hooks PRD's
   `BeforeTool`; decide there or here.
3. `CommitError` is reused for the returned error (one type, `Step` unset
   when a recorder returns it). A separate `RecordingError` would read more
   plainly but adds a noun. Recommended: reuse.
