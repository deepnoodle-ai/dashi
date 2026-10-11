# R10: Handle two agents competing for the last dollar

[All examples](README.md) · [Review](../README.md)

Agent A and agent B each see $1 available and each need an 80-cent reservation.
A wins the database transaction. B must wait without sending a model request;
a network failure while committing B is not proof that the reservation failed.

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:733–743](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L733). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 733 743 -->
```go
out, err := runner.Run(ctx, turn, command, dashi.WithOwner(owner.OwnerID), dashi.WithSteering(&steering{runtime: r}))
if err != nil {
	var ce *dashi.CommitError
	if errors.As(err, &ce) && ce.NothingWritten && (errors.Is(err, repo.ErrBudgetHeld) || errors.Is(err, repo.ErrRoutineCap)) {
		return true, false, nil
	}
	if ctx.Err() != nil {
		return false, false, nil
	}
	return false, false, &runtime.RecoveryError{Code: "execution_persistence", Err: err}
}
```

## After: Proposed API sketch

These Dashi APIs do not exist yet. Application variables and storage methods
show the intended caller responsibilities, not a complete runnable program.

```go
// Proposed error from the Recorder, inside the same model-start transaction.
disposition, err := tx.CheckCommit(req) // Owner, duplicate/conflict, expected sequence.
if err != nil { return err }
if disposition == CommitDuplicate { return nil }
if err := tx.Reserve(accountID, callID, risk); errors.Is(err, ErrNoMoney) {
    return &dashi.ModelCallRejected{
        Code: "shared_budget", Reason: "waiting for available balance", Resumable: true,
    }
} else if err != nil { return err }
return tx.AppendModelStart(req)

// Proposed structured outcome after Dashi records the refusal.
if out.Stop.Kind == dashi.StopLimited && out.Stop.Limit != nil && out.Stop.Limit.Code == "shared_budget" {
    return waitForTopUp(turnID)
}
```

The proposed sequence must be explicit:

1. Continue C is saved.
2. Reservation and model-start write definitely reject; neither is saved.
3. Runner saves a host-limit stop at that sequence and sends nothing.
4. Unknown acknowledgment of the stop requires reload; retrying C returns saved state.
5. After funding, the app saves a fresh Continue D.

BeforeSend already supports preflight host limits. The new contract handles the
race inside the model-start transaction, not arbitrary storage failures.

**Observable result:** A would reserve $0.80 and send. B would send nothing and wait. A missing commit acknowledgment would still require authoritative reload.

**Developer benefit:** Replace the nested CommitError/repository-error check with a supported refusal and wait outcome.

**Application responsibility:** Use a single transaction for reservation and model start. Preserve definite-vs-unknown write outcomes, choose a fresh command after a definite refusal, and bound contention retries.
