# R11: Replace an estimated charge with final billing

[All examples](README.md) · [Review](../README.md)

A finished request has an estimated four-cent charge. Later billing confirms
three cents. The model-cost view should show three cents, the account should receive a
one-cent adjustment once, and the original reply should stay intact. Account
postings may differ from raw model cost because each call is rounded.

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:1346–1354](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L1346). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 1346 1354 -->
```go
ev, err := event(turn, live)
if err != nil {
	return err
}
saved, err := r.scope(ctx, swarmID, id)
if err != nil {
	return err
}
ev.CostMicroUSD = saved.CostMicroUSD
```

From [swarm/internal/repo/entrepo/model_budget.go:436–446](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/repo/entrepo/model_budget.go#L436). Surrounding definitions are omitted.

<!-- source: swarm internal/repo/entrepo/model_budget.go 436 446 -->
```go
key := "reconcile:" + in.TurnID + ":" + in.AttemptID + ":" + in.Key
old, e := tx.BudgetEntry.Get(ctx, recordID(a.ID, key+":receipt"))
if e == nil {
	if old.Data.Quality != jsonDigest(in) {
		return repo.ErrConflict
	}
	return nil
}
if !ent.IsNotFound(e) {
	return e
}
```

From [swarm/internal/repo/entrepo/model_budget.go:447–455](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/repo/entrepo/model_budget.go#L447). Surrounding definitions are omitted.

<!-- source: swarm internal/repo/entrepo/model_budget.go 447 455 -->
```go
data := t.Data
account := a.Data
if e = settleAttempt(ctx, tx, &data, &account, attempt, repo.AttemptMutation{Kind: "settle", AttemptID: in.AttemptID, ChargeMicroUSD: in.ChargeMicroUSD, ChargeNanos: in.ChargeNanos, Final: in.Final, Quality: in.Quality}, key, 0); e != nil {
	return e
}
if e = appendBudgetEntry(ctx, tx, account, repo.BudgetEntry{Key: key + ":receipt", Kind: "reconciliation", TurnID: t.ID, AttemptID: in.AttemptID, Quality: jsonDigest(in)}); e != nil {
	return e
}
return tx.ModelTurn.UpdateOneID(t.ID).SetData(data).Exec(ctx)
```

## After: Proposed API sketch

These Dashi APIs do not exist yet. Application variables and storage methods
show the intended caller responsibilities, not a complete runnable program.

```go
// Application verifies provenance/authority before asserting final billing.
receipt, err := verifyBillingReceipt(providerReceipt, finishedCallID)
if err != nil { return err }
// Proposed: accounting-only; Resolve still handles pending model results.
out, err := runner.Run(ctx, savedTurn, dashi.CorrectModelCost{
    CommandID: "billing:" + receipt.ID, CallID: finishedCallID, EvidenceID: receipt.ID,
    Cost: &llm.Cost{
        Currency: "USD", Source: llm.CostSourceProviderReported,
        Total: receipt.Total, // This example's verified receipt is USD 0.03.
    },
    Final: true,
}, dashi.WithOwner(owner.OwnerID))
if err != nil { return err }
showModelCost(out.Spend.Cost["USD"]) // Evidence, before application posting rules.
showAccountCharge(loadPostedCharge(turnID)) // Includes per-call rounding.
```

**Observable result:** Today Swarm can correct its account while Dashi retains four cents. Afterward model evidence would show three cents and the rounded account adjustment would apply once. Reply/tools stay unchanged. Two one-nano calls still total two nanos in Dashi and two micros in Swarm's ledger.

**Developer benefit:** Remove conflicting model-cost interpretations. A UI showing posted charges still needs the application ledger; a raw Spend total cannot replace it.

**Application responsibility:** Save the correction and ledger difference atomically. Deduplicate EvidenceID independently of CommandID. Reject conflicting receipt reuse and changes to authoritative final evidence. Caller-provided Source/Final fields do not prove authority. Cost correction does not by itself correct usage. Round the old/new totals before calculating the adjustment.

**Runnable evidence:** `Example_finishedCostCorrection / Example_correctionRounding` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.

**Additional runnable evidence:** Example_calculatedAndPostedCost in [the offline demos](demo/).
