# Turn budgets

The application sets a turn's optional budget on `Start`. An agent has no
budget; it still owns the existing model-call, resume, and deadline limits.
`MaxCost` limits model-call costs. Local tool charges are reported
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
every provider. Before `BeforeSend`, Dashi prices the call's worst case: its
estimated input at the highest input-side rate plus the full output allowance.
The input estimate (`llm.EstimatedInputTokens`) is realistic, not a bound:
about four bytes of request body per text token, a fixed amount per image,
document page, audio, or video instead of its encoded bytes, and never more
than the model's context window. A client may supply its own count in
`RequestSnapshot.EstimatedInputTokens`. After the first call, the next call is
estimated from the input the provider reported plus what was added since.
If it cannot bound the call or the call would exceed the cap, the turn stops `limited` before the call is sent. It does not shrink the reply to
fit a cost cap. The stop's `Limit` gives the cap, spent amount, worst case
(`CostLimit`, `CostUsed`, `WorstCaseCost`), and the output-token allowance that
would fit (`SuggestedMaxOutputTokens`). A call whose worst case reaches the cap
exactly is allowed. If input alone consumes the remaining cap, no positive
allowance fits; raise the cap or reduce input/history. A model without prices
has an explicit `UnpricedModel` detail. A call with provider tools also stops
before it is sent: within one call the provider may bill an unbounded number of
provider tool requests and re-read their results as input, so no worst case
exists. Remove the provider tools or run without a cost cap. So does a call
without an output limit, which happens only under the strict capability policy
when the model's `max_tokens` support is unknown.

To check ahead of time that a model can run under a cost cap, call
`info.CanBoundCost("USD")` on the client's `ModelInfo` for it. It is true when
the model has prices in that currency and calls with default settings have a
finite worst case; provider tools and one-hour cache writes without a price can
still leave a specific call unbounded.

`RunResult.Spend` (or `turn.View()` for a loaded turn) contains token
totals, model costs by currency, a count of calls without final usage, and
counts of calls with unknown or incompatible-currency model cost. A call that
billed provider tool requests with no list price (Anthropic code execution, for
example) is **partial**: its known cost is in `Spend.Cost`, it counts in
`Spend.CostPartial`, and the unpriced counts are in `Spend.UnpricedRequests` by
meter key. `Spend.Cost` is then a lower bound. Under a cost cap a partial cost
also counts as unknown, since a lower bound cannot show the cap holds: the turn
stops `limited`, and raising the cap does not clear it. The stop reason names
the unpriced requests; start a new turn with the history to go on.
`Spend.ToolCost` reports local tool charges by currency. A model call's
`model_call_started` step records its `WorstCaseCost`; its charge is derived
from the recorded result each time (`ModelCallCostOf`), never stored. A call
counts only the usage its provider reported. Dashi never fills in a token
count: a call with no reported usage (a worker stopped, a steer cut it, its
result is unknown) counts zero, and a call cut off after some usage arrived
counts that usage. Both count in `Spend.CallsWithoutFinalUsage`, and `Usage`
and `Cost` undercount them, so a cost cap limits known spend and is not a
strict charge guarantee for interrupted calls. A tool may return
`tool.Outcome.Cost`; it is counted after the tool finishes and does not consume
`MaxCost`. The application must bound and approve charged tools before they run
if it promises a cap covering them.

Amounts are exact. `money.Amount` counts nano-units (1e-9) of the currency in
an int64 and encodes in JSON as a decimal string such as `"0.5"`; it also reads
the bare JSON numbers some provider APIs report. Dashi sums costs without
drift. A worst case (`WorstCaseCost`) rounds up to the nano, so rounding never
breaches a cap, and a finished call's list-price cost rounds half to even. A
ledger in coarser units converts from `Nanos()` with its own rounding, for
example up for holds.

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
`usage incomplete`. Independent monetary sums remain exact unless a
`partial` or `unknown` cost counter also applies. Old positive `CostUnknown`
values remain conservative; their original cause cannot be inferred. Counters
can overlap and do not form a combined count of unpriced calls. Use `Usage` and
`UnpricedRequests` for detailed diagnostics. This summary controls default
`fmt` display, including `%v`. Use fields or JSON for machine processing and
storage.

Usage derivation or aggregation can exceed the integer range even when each
reported fact is valid. `UsageIncomplete` then marks retained usage as partial;
it differs from `CallsWithoutFinalUsage`, which counts calls the provider
never reported final usage for. Exact independent
charges still accumulate. Actual monetary overflow retains the prior subtotal
and marks `CostUnknown`. Diagnostic counts saturate at `math.MaxInt` and then
represent lower bounds. Old Spend JSON without the new field reads it as false.

An active token cap fails closed on incomplete usage. The runner records a
`limited` stop that ends the turn before any new call runs, even if retained
counts are below the cap. A cost-only budget can still allow calls when its
monetary bound is exact. Before stopping, a host-supplied result can restore
complete usage. `Continue` can recover recorded work or record a stop after a
crash; being ready for that recovery does not permit new calls. After the stop
that ends the turn, late model results can correct Spend but cannot revive the
turn; tool results are refused.
Resolve calls with unknown results as required, then start a new turn carrying
prior history. Raising a cap alone cannot repair incomplete accounting.

To raise a cap, authorize the increase in the application and pass the full
new budget to `Continue`. It can raise existing caps but cannot lower them or
introduce a new cap. Each raise is saved as a continuation step and appears in
`TurnView.Raises` with its time. A host credit hold needs no raise: after funds
arrive, `Continue` makes `BeforeSend` check the balance again.

An application can reserve credits in its recorder. `BeforeSend` receives a
`SendPlan` with the prepared request, `WorstCaseCost`, and the remaining turn
cost allowance. It can return `SendDecision.Reservation` with the account and
amount to hold. The recorder atomically saves the `model_call_started` step and
reserves that amount, using the model call ID as the idempotency key. A credit
denial returns a `Limit` of kind `host` and an application code such as
`insufficient_credits`, yielding a resumable `limited` stop. A denial that
reports money puts it in the `Cost` fields with a `Currency` (an ISO 4217 code
or the host's credit unit) and keeps `Limit` and `Used` for counts. Credit
balances and plan ceilings remain with the application.

To charge or release a hold when a call finishes, let Dashi classify each
model call rather than reading the model facts.
`dashi.ModelCallCostOf(callStart, finish)` returns the call's `ModelCallCost`:
the `Usage` and `Cost` that Dashi counts into `Spend`, so per-call costs sum to
`Spend.Cost`. `UsageSource` says where the usage came from: `reported`
(complete provider usage), `cut_off` (some usage was reported, but the call did
not finish normally; a lower bound), `missing` (no usage was reported for a
call that may have run, such as one still running or lost with its worker; it
counts zero), or `not_sent` (the provider verifiably never ran it, so it costs
nothing). A nil `Cost` on a sent call with reported usage is unknown, for
example an unpriced model; on a `missing` call it means zero. `Exact` means the cost is exact: a reported, complete cost, or
zero for a call that never ran. A cost that is not exact is a lower bound or
unknown. Charge an exact call at its cost. For the rest, charge the known
lower bound and release the hold, or keep the hold by your own rule if you
need a strict guarantee. A recorder charges in the same
transaction as the call's finish step, with the start fact it stored when it
reserved:

```go
func (r *Recorder) Commit(ctx context.Context, req dashi.CommitRequest) error {
	return r.db.Tx(ctx, func(tx *Tx) error {
		// ...append req.Step, and reserve on StepModelCallStarted...
		switch req.Step.Kind {
		case dashi.StepModelCallFinished, dashi.StepModelCallFinishedByHost:
			callStart := tx.ReservedCallStart(req.Step.Model.ModelCallID)
			c := dashi.ModelCallCostOf(callStart, req.Step.Model)
			switch {
			case c.UsageSource == dashi.UsageSourceNotSent:
				return tx.Release(c.ModelCallID)
			case c.Cost != nil && !c.Cost.Partial():
				// Exact, or the known lower bound of a cut-off or missing
				// call. A host that needs a strict guarantee keeps the
				// hold when !c.Exact instead.
				return tx.Charge(c.ModelCallID, roundUpToMicros(c.Cost.Total))
			}
			// Partial or unknown cost: the host's rule, here keeping the
			// hold.
		}
		return nil
	})
}
```

The same entries are on `TurnView.ModelCallCosts`, in record order and
including a pending model call, for a host that charges after each run
instead.
