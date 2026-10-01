# Turn budgets

The application sets a turn's optional budget on `Start`. An agent has no
budget; it still owns the existing model-attempt, interruption, and deadline
limits. `MaxCost` limits model-call charges. Local tool charges are reported
separately and need an application-owned ceiling for an all-in price promise.
A zero token cap or nil cost cap means that dimension is uncapped.

```go
start := dashi.Start{
	CommandID: "start-123",
	Input:     input,
	Budget: &dashi.Budget{
		MaxOutputTokens: 4_000,
		MaxTotalTokens:  12_000,
		MaxCost: &dashi.Money{
			Amount:   money.MustParseAmount("0.50"),
			Currency: "USD",
		},
	},
}
```

Dashi lowers the next request's output allowance to the remaining output tokens
and records a warning. The total-token cap is checked after each call. For a
cost cap, the prepared request must carry model prices and a finite effective
output allowance. An unset `MaxTokens` becomes `llm.DefaultMaxTokens` (32,768,
or the model's smaller documented limit), so the default settings are bounded on
every provider. Dashi estimates the full input and output cost before
`BeforeSend`. If it cannot bound the call or the call would exceed the cap, the
turn stops `limited` before dispatch. It does not shrink the reply to fit a cost
cap. The stop's `Limit` gives the cap, spent amount, worst case (`CostLimit`,
`CostUsed`, `CostAtRisk`), and the output-token allowance that would fit. A call
whose worst case reaches the cap exactly is admitted. If input alone consumes
the remaining cap, no positive allowance fits; raise the cap or reduce
input/history. A model without prices has an explicit `UnpricedModel` detail. A
call with provider-hosted tools also stops before dispatch: within one call the
provider may bill an unbounded number of hosted requests and re-read their
results as input, so no worst case exists. Remove the hosted tools or run
without a cost cap. So does a call without an output limit, which happens only
under the strict capability policy when the model's `max_tokens` support is
unknown.

To check ahead of time that a model can run under a cost cap, call
`info.CanBoundCost("USD")` on the client's `ModelInfo` for it. It is true when
the model has prices in that currency and calls with default settings have a
finite worst case; hosted tools and one-hour cache writes without a price can
still leave a specific call unbounded.

`Outcome.Spend` (or `Replay(turn).Spend` for a loaded turn) contains token
totals, model costs by currency, counts of calls with estimated usage, and
counts of calls with unknown or incompatible-currency model cost. A call that
billed hosted requests with no list price (Anthropic code execution, for
example) is **partial**: its known cost is in `Spend.Cost`, it counts in
`Spend.CostPartial`, and the unpriced counts are in `Spend.UnpricedRequests` by
meter key. `Spend.Cost` is then a lower bound. Under a cost cap a partial charge
also counts as unknown, since a lower bound cannot show the cap holds: the turn
stops `limited`, and raising the cap does not clear it. The stop reason names
the unpriced requests; start a new turn with the history to go on.
`Spend.ToolCost` reports local tool charges by currency. Each settled model fact
records its `Charge`; the intent records `AtRisk`. When final usage is missing,
Dashi preserves streamed counts, fills absent input from the prepared request,
and counts the greater of streamed output and a visible-text estimate. It marks
the call estimated. This best guess can undercount a cut-off call, so a cost cap
is not a strict charge guarantee for that case. A tool may return
`tool.Outcome.Cost`; it is counted after the tool settles and does not consume
`MaxCost`. The application must bound and admit charged tools before execution
if it promises a cap covering them.

Amounts are exact. `money.Amount` counts nano-units (1e-9) of the currency in
an int64 and encodes in JSON as a decimal string such as `"0.5"`; it also reads
the bare JSON numbers some provider APIs report. Dashi sums charges without drift. A
worst case (`AtRisk`) rounds up to the nano, so rounding never breaches a cap,
and a settled list-price charge rounds half to even. A ledger in coarser units
converts from `Nanos()` with its own rounding, for example up for holds.

Print Spend directly for a readable cost summary:

```go
fmt.Println(out.Spend)
// model: at least 0.00014 USD; tools: 2 USD; partial: 1
```

`Spend.String()` prints exact amounts, sorts currencies within each section,
and keeps model and local tool costs separate. Empty cost maps produce
`no recorded cost`. An explicit zero retains its currency, such as
`model: 0 USD`. Positive counters appear independently as `partial: N` and
`unknown: N`. Either counter prefixes model amounts with `at least`.
`CostUnknown` also qualifies tool amounts because it can include aggregation
overflow in either category. `CostPartial` alone qualifies only model amounts.
Usage aggregation failure instead sets `Spend.UsageIncomplete` and appends
`usage incomplete`. Independent monetary sums remain exact unless a cost
uncertainty counter also applies. Old positive `CostUnknown` values remain
conservative; their original cause cannot be inferred.
Counters can overlap and do not form a combined count of unpriced calls.
Use `Usage` and `UnpricedRequests` for detailed diagnostics. This summary
controls default `fmt` display, including `%v`. Use fields or JSON for machine
processing and storage.

Usage derivation or aggregation can exceed the integer range even when each
reported fact is valid. `UsageIncomplete` then marks retained usage as partial;
it differs from `Estimated`, which describes fallback usage. Exact independent
charges still accumulate. Actual monetary overflow retains the prior subtotal
and marks `CostUnknown`. Diagnostic counts saturate at `math.MaxInt` and then
represent lower bounds. Old Spend JSON without the new field reads it as false.

An active token cap fails closed on incomplete usage. The runner records a
terminal `limited` stop before new execution, even if retained counts are below
the cap. A cost-only budget can still admit work when its monetary bound is
exact. Before stopping, reconciliation can restore complete usage. `Continue`
can recover recorded work or record a stop after a crash; readiness for that
recovery does not permit new execution. After the terminal stop, late results
can correct Spend but cannot revive the turn. Resolve outstanding effects as
required, then start a new turn carrying prior history. Raising a cap alone
cannot repair incomplete accounting.

To raise a cap, authorize the increase in the application and pass the full
new budget to `Continue`. It can raise existing caps but cannot lower them or
introduce a new cap. Each raise is saved as a continuation step and appears in
`TurnView.Raises` with its time. A host credit hold needs no raise: after funds
arrive, `Continue` makes `BeforeSend` check the balance again.

An application can reserve credits in its recorder. `BeforeSend` receives a
`SendPlan` with the prepared request, `AtRisk`, and the remaining turn cost
allowance. It can return `SendDecision.Admission` with the account and
amount to hold. The recorder atomically saves the model intent and reserves
that amount, using the attempt ID as the idempotency key. A credit denial
returns a `Limit` of kind `host` and an application code such as
`insufficient_credits`, yielding a resumable `limited` stop. A denial that
reports money puts it in the `Cost` fields with a `Currency` (an ISO 4217 code
or the host's credit unit) and keeps `Limit` and `Used` for counts. Credit
balances and plan ceilings remain with the application.

To settle, let Dashi classify each attempt rather than reading the model
facts. `dashi.AttemptSpendOf(intent, settlement)` returns the attempt's
`AttemptSpend`: the `Usage` and `Charge` that Dashi counts into `Spend`, so
per-attempt charges sum to `Spend.Cost`. `Basis` says where the usage came
from: `measured` (complete provider usage), `estimated` (Dashi filled some in:
the call was cut short, is uncertain, still running, or its final usage never
arrived), or `not_executed` (the provider verifiably never ran it, so it costs
nothing). A nil `Charge` on an executed attempt is unknown, for example an
unpriced model. `Final` means the charge is exact: a measured, complete charge,
or zero for an attempt that never ran. A charge that is not final is an
estimate, a lower bound, or unknown. Settle a final attempt at its charge, and
settle the rest by your own rule, such as keeping the hold. A recorder settles
in the same transaction as the settlement step, with the intent fact it stored
when it reserved:

```go
func (r *Recorder) Commit(ctx context.Context, req dashi.CommitRequest) error {
	return r.db.Tx(ctx, func(tx *Tx) error {
		// ...append req.Step, and reserve on StepModelIntent...
		switch req.Step.Kind {
		case dashi.StepModelSettled, dashi.StepModelReconciled:
			intent := tx.ReservedIntent(req.Step.Model.AttemptID)
			a := dashi.AttemptSpendOf(intent, req.Step.Model)
			switch {
			case a.Basis == dashi.SpendNotExecuted:
				return tx.Release(a.AttemptID)
			case a.Final:
				return tx.Settle(a.AttemptID, roundUpToMicros(a.Charge.Total))
			}
			// Estimated, partial, or unknown: the host's rule, here
			// keeping the hold.
		}
		return nil
	})
}
```

The same entries are on `TurnView.Attempts`, in record order and including a
pending attempt, for a host that settles after each run instead.
