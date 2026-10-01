# Run results and approvals

**Status:** Accepted · **Date:** 2026-09-28 · **Addresses:**
[F](gaps/F-example-ports.md) 4.1, 4.2, 8.1, 8.2, 9.4, 9.5, 4.3; B rows 2 and 7

> **Superseded API:** hooks ([hooks.md](hooks.md), decision 31) replaced
> `Policy`. Its `BeforeModel`, `BeforeSend`, `AuthorizeTool` and `OnStop`
> are now the `BeforeModel`, `BeforeSend`, `BeforeTool` and `BeforeFinish`
> hook points; `ModelAuthorization` is `SendDecision`; approvals no longer
> carry a policy revision; and a tool with no hook opinion runs.

## Problem

A caller reads a run's result from four places: the Go error,
`Outcome.RecordingErr()`, `Turn.AnswerText()`, and `Turn.View` for spend and call
details. Pending work is bare IDs. Answering an approval means copying a digest
and a revision, naming the call twice, and inventing a command ID, and a denial
reaches the model without its reason. The approval example is 220 lines.

## Decisions

**1. One error channel (4.1).** `Outcome` is always the latest known state;
the error says whether this command applied. `Outcome` loses `Persistence`,
`Unacknowledged`, `WriteError`, and `RecordingErr()`, and
`PersistenceAcknowledged` goes away. A step write that is not acknowledged
comes back as a `*CommitError` whose `Step` is always set. A recorder's own
`*CommitError` is wrapped in a new value (its persistence and cause copied,
never mutated), so the message is not doubled. Check in this order:

1. `errors.Is(err, ErrStaleTurn)` or `ErrCommandIDReused`: another
   worker owns or advanced the turn, or the command ID was reused. The
   conflict comes plain from a pre-check or inside a `*CommitError`;
   `errors.Is` covers both. `Outcome.Turn` is not the saved record: reload the
   turn and back off.
2. `errors.As(err, &commitErr)`: the turn ran, but `commitErr.Step` was not
   saved (`NothingWritten`) or may have been (`PersistenceUnknown`, reload
   first). `Outcome.Turn` holds the steps this run knows were saved, and
   recovery treats the failure as a crash after them. If the failed step was
   the command's first, nothing ran. When it is a `model_call_finished` step,
   `out.Spend` omits that call; its usage is on `commitErr.Step`.
3. Any other error: the command was rejected and nothing was recorded.
   `Outcome` is the caller's turn and its view, unchanged, so
   `out, err = runner.Run(ctx, out.Turn, cmd)` loops never lose the turn. An
   invalid input turn gives `Outcome{Turn: turn}`.

If the runner's own record fails to replay after a commit (a bug), `Run`
returns `ErrRecordInvalid`, which also matches `ErrInvalidTurn`, with the
recorded steps; a caller's invalid input turn gives `ErrInvalidTurn` alone.
`Stream` yields its final `Outcome` with the same error in one pair. Execution
failures stay in `Outcome.Stop`: a failed or limited turn is a result.
`Continue` on a turn that is not ready returns `ErrNotReady`, wrapped with the
IDs of the calls it waits on or the stop that prevents it.

**2. The Outcome is the turn plus its view (4.2).** `Outcome` embeds
`TurnView`, so `out.Stop`, `out.Output`, `out.Warnings`, and
`out.ReadyToContinue` keep their names, and `out.Spend`, `out.Tools`, and the
fields below need no replay. `Turn.View` returns the same view for a loaded
record.

```go
type Outcome struct {
	Turn Turn // the saved record; pass it to the next Run
	TurnView  // what the record means now
}

// Added to or changed on TurnView:
Answer  string    // text of the final assistant answer
Pending []Pending // typed, in a stable order (was []string)
```

`Turn.View` and `Outcome` project an unrecorded stop as `Run` does: an unsettled
model attempt is uncertain, an in-flight call or attempt is interrupted, and
remaining pending work is waiting. A policy's view is taken mid-run, so its
`Stop` stays empty. `Turn.StructuredOutput()` is removed because it replays
the turn to return `out.Output`; `Turn.AnswerText()` is removed because
`Answer` replaces it, and `Policy.OnStop` now sees the candidate answer too.

**3. Pending items build their own answers (8.1).**

```go
type Pending struct {
	Kind   PendingKind                // PendingApproval, PendingTool, PendingModel
	ID     string                     // call, hosted request, or attempt ID
	Name   string                     // tool name, for approvals and tool calls
	Args   json.RawMessage            // the arguments a person approves
	Tool   *ToolView                  // set for local calls
	Hosted *llm.ProviderToolApprovalRequest // set for hosted approval requests
}

func (p Pending) Approve() Resolve
func (p Pending) Deny(reason string) Resolve
func (p Pending) Result(outcome tool.Outcome) Resolve
```

`PendingApproval` covers a local call deferred by policy and a provider-hosted
approval request. `PendingTool` covers a local call that is started, waiting,
or uncertain; read `Tool.Status`. `PendingModel` covers a model attempt
awaiting reconciliation. Calls that are only requested, allowed, or denied
need nothing but `Continue` and are not listed. Order: model attempts, local
calls in call order, then hosted requests in the order the model made them.

`Approve` and `Deny` fill the call ID, argument digest, and policy revision,
or the request ID and origin for a hosted request. `Result` takes a value, so
callers never write `new(...)`. Command IDs derive from the item
(`approval:<id>`, `hosted-approval:<attempt>:<id>`, `result:<id>`): a retried
answer is a duplicate, and a different second answer is `ErrCommandIDReused`.
Local call IDs are unique within a turn (replay rejects a repeat), but a
provider may reuse a hosted request ID once the first was answered, so
`Pending.ModelCallID` scopes that command. A builder on the wrong kind of item
produces a command `Run` rejects.

`dashi.RequireApproval(tools ...string)` is a policy that allows local tools
and defers the named ones, with a fixed revision derived from the names. It
can later be expressed as a `BeforeTool` hook. `Resolve` and `Continue` stay
separate (decision 18); a combined command is deferred.

**4. A denial carries its reason to the model (8.2).** `ApprovalDecision` gains
`Reason`. `Resolve` records only the approval, as decision 18 intends; today
it also writes an uncommanded `tool_finished` step, and a crash between the two
writes loses the operator's reason. Replay keeps the policy's decision and
records the answer beside it in `ToolView.Approval`, and the next `Continue`
settles the call as not executed with the reason as its
output. The model sees that text as the tool result; an empty reason from an
approval or a policy gives `The call was denied.`

**5. Answer text keeps sentences intact (9.4).** Adjacent text blocks are
concatenated as is, because providers split a cited sentence into fragments.
When any non-text item (a hosted use or result, a tool call, reasoning) sits
between two text blocks, they are joined with a blank line unless the first
ends with a newline. Messages the provider paused (`StopPause`) and then
continued are one answer, joined by the same rule.

**6. Turn-wide helpers (9.5).** Two methods walk every settled assistant
message, because hosted work can span several model calls:

```go
func (t Turn) Citations() []content.Citation // in order, as reported
func (t Turn) ProviderToolUses() []llm.ProviderToolUse   // each with its paired result
```

`ProviderToolUses` merges a separate `ProviderToolResult` into its use's `Output` and
`IsError`. `Citations` does not deduplicate: one source may carry two
excerpts.

**7. Restart deduplication is documented (4.3)** in the recovering-turns guide:
a repeated command returns the recorded result without repeating work only
when the same recorder holds the turn and the command has the same `CommandID`
and content.

## Examples

`examples/approval` becomes the blocking form: the policy asks the person
inside `AuthorizeTool` and allows or denies, all within one `Run` (hooks FR-12).
The deferred form moves to `examples/durable_approval`, for servers that record
an answer in one request and resume in another:

```go
runner.Hooks = dashi.RequireApproval("deploy")
out, err := runner.Run(ctx, dashi.NewTurn("deploy"), start)
for err == nil && out.Stop.Kind == dashi.StopWaiting {
	for _, p := range out.Pending {
		if p.Kind != dashi.PendingApproval {
			continue
		}
		answer := p.Deny("The operator declined.")
		if ask(p.Name, p.Args) {
			answer = p.Approve()
		}
		if out, err = runner.Run(ctx, out.Turn, answer); err != nil {
			return err
		}
	}
	out, err = runner.Run(ctx, out.Turn, dashi.Continue{CommandID: next()})
}
```

## Tests

- Commit failures: rejected and unknown writes return `*CommitError` with
  `Step`; a recorder's `ErrStaleTurn` matches `errors.Is` with no doubled
  message; a failed `model_call_finished` write leaves the usage on `Step`.
- A rejected command returns the caller's turn and view.
- A crash between recording a denial and continuing keeps the operator's
  reason; the model receives it.
- A mixed pending list (local approval, hosted approval, waiting external
  call) in stable order; builders settle each; retries are duplicates.
- A policy's mid-run view has an empty `Stop`; `Turn.View` and `Run` agree.
- Answer text with citation fragments, reasoning blocks, hosted items, and a
  provider pause; `Citations` and `ProviderToolUses` across calls.
