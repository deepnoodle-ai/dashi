# R12: Reuse billing rules for a Writing desk request

[All examples](README.md) · [Review](../README.md)

A Writing desk request reserves 20 cents, then its stream ends with incomplete
usage. Swarm's direct request path posts the whole reservation as an estimate;
the Turn path can preserve a known partial cost. The developer needs to choose
that policy explicitly.

## Before: current code

From [swarm/internal/runtime/dashirt/generate.go:114–137](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/generate.go#L114). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/generate.go 114 137 -->
```go
func generateCost(result llm.Result, sendErr error, pricing llm.Pricing, riskNanos int64) (int64, string) {
	if sendErr != nil {
		var le *llm.Error
		if errors.As(sendErr, &le) && (le.SendStatus == llm.SendStatusNotSent || le.SendStatus == llm.SendStatusProviderRejected) {
			return 0, "verified_nonexecution"
		}
		return riskNanos, "estimated"
	}
	// Only a call that says it ended has final usage.
	switch result.Stop.Kind {
	case llm.StopFinished, llm.StopToolUse, llm.StopOutputLimit, llm.StopContextLimit, llm.StopRefusal, llm.StopPause, llm.StopOther:
	default:
		return riskNanos, "estimated"
	}
	if c := result.Cost; c != nil && c.Currency == "USD" && !c.Partial() {
		return c.Total.Nanos(), "measured"
	}
	if result.Usage != nil {
		if c, ok := pricing.Estimate(result.Usage); ok && !c.Partial() {
			return c.Total.Nanos(), "measured"
		}
	}
	return riskNanos, "estimated"
}
```

## After: Proposed API sketch

The proposed Dashi helper does not exist yet. Application methods are illustrative.

```go
result, sendErr := client.Send(ctx, llm.Call{Request: prepared})
observation := llm.ClassifyCost(prepared, result, sendErr) // Proposed shared classifier.
// Keep cost source, usage source, finality, missing prices and admitted hold visible.
settlement := deskPostingPolicy.Choose(observation, admittedHold)
if err := ledger.Settle(context.WithoutCancel(ctx), holdID, settlement); err != nil {
    return &DeskError{Model: sendErr, Settlement: err} // Application typed error.
}
return sendErr
```

**Observable result:** The classifier would distinguish estimates from reported/partial costs, their sources, finality, and missing prices. Swarm could retain its conservative estimate policy without copying stop/usage rules.

**Developer benefit:** Delete generateCost while avoiding an agent tool loop for one text request.

**Application responsibility:** Reserve before dispatch, store settlement, join shutdown, and recover stranded holds after a crash. For example, retain a full-hold provisional charge or post calculated cost while holding unresolved exposure. An estimate is not a known lower bound. Defer a storage-owning executor; use a recorded one-call Turn when restart recovery is required.
