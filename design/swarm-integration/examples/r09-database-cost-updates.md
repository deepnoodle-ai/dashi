# R09: Save model records and money together

[All examples](README.md) · [Review](../README.md)

A model call finishes with a two-cent charge. The application's Recorder must
save the finish record, update the account, and mark delivered input in one
transaction. A repeated write must not charge the account again.

## Before: current code

From [swarm/internal/runtime/dashirt/recording.go:58–67](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/recording.go#L58). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/recording.go 58 67 -->
```go
ev, err := event(projectionTurn, false)
if err != nil {
	return &dashi.CommitError{NothingWritten: true, Err: err}
}
mutation := repo.CommitModelStepRequest{Step: repo.ModelStep{TurnID: in.Step.TurnID, Seq: in.Step.Seq, Kind: string(in.Step.Kind), CommandID: in.Step.CommandID, CommandDigest: in.Step.CommandHash, JSON: raw}, ExpectedSeq: in.ExpectedSeq, Owner: s.owner, Projection: &repo.ModelProjection{Status: string(ev.Status), StopReason: ev.StopReason, CostMicroUSD: ev.CostMicroUSD, Terminal: ev.Terminal}}
if in.Step.Context != nil && in.Step.Context.Steering {
	for _, item := range in.Step.Context.Items {
		mutation.ConsumedSteerKeys = append(mutation.ConsumedSteerKeys, item.ID)
	}
}
```

From [swarm/internal/runtime/dashirt/recording.go:90–117](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/recording.go#L90). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/recording.go 90 117 -->
```go
var started *dashi.ModelFact
for i := range turn.Steps {
	step := &turn.Steps[i]
	if step.Kind == dashi.StepModelCallStarted && step.Model != nil && fact != nil && step.Model.ModelCallID == fact.ModelCallID {
		started = step.Model
	}
}
if started == nil {
	return &dashi.CommitError{NothingWritten: true, Err: errors.New("finished model call has no recorded start")}
}
call := dashi.ModelCallCostOf(started, fact)
charge, nanos := int64(0), int64(0)
if call.Cost != nil {
	if call.Cost.Currency != "USD" {
		return &dashi.CommitError{NothingWritten: true, Err: errors.New("model call cost is not USD")}
	}
	nanos = int64(call.Cost.Total)
	if charge, err = microUSD(nanos); err != nil {
		return &dashi.CommitError{NothingWritten: true, Err: err}
	}
}
quality := "estimated"
if call.UsageSource == dashi.UsageSourceNotSent {
	quality = "verified_nonexecution"
} else if call.Exact {
	quality = "measured"
}
mutation.Attempt = &repo.AttemptMutation{Kind: "settle", AttemptID: call.ModelCallID, ChargeMicroUSD: charge, ChargeNanos: nanos, Final: call.Exact, Quality: quality}
```

## After: Proposed API sketch

The proposed Dashi helper does not exist yet. Application storage methods
show the responsibilities that remain with the caller.

```go
return repo.Transaction(ctx, func(tx Tx) error {
    // Application helper checks owner, then duplicate/conflict, then expected sequence.
    disposition, err := tx.CheckCommit(req) // new, exact duplicate, or error
    if err != nil { return err } // Includes conflicting duplicates and stale owners.
    if disposition == CommitDuplicate { return nil } // No ledger/input effects again.
    changes, err := dashi.ProjectCommit(turn, req.Step) // Proposed pure helper.
    if err != nil { return err }
    // Dashi gives per-call evidence; the host chooses estimates, rounding and postings.
    postings, err := ledgerPolicy.Postings(changes.ModelCalls)
    if err != nil { return err }
    if err := tx.ApplyPostings(accountID, postings); err != nil { return err }
    if err := tx.ConsumeInputs(changes.DeliveredInputs); err != nil { return err }
    return tx.AppendStepAndView(req, changes.View)
})
// Recorder wraps transaction failures with NothingWritten only when known.
```

**Observable result:** A successful commit would save the record and two-cent charge together. Retrying the same record would make no second money change.

**Developer benefit:** Remove start-fact scanning, repeated view construction, and model-cost interpretation. Ledger postings remain application policy. The application still implements its transaction and ledger.

**Application responsibility:** Check the worker and sequence, reject conflicting duplicates, validate the admitted account, and convert currency units. Exact duplicate returns success with no effects; conflicting duplicate/stale owner fails. Unknown acknowledgment requires reload. The Turn prefix supplied to projection must match ExpectedSeq. Dashi supplies the meaning of its records, not a SQL schema.
