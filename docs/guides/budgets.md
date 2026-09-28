# Turn budgets

The application sets a turn's optional budget on `Start`. A definition has no
budget; it still owns the existing model-attempt, interruption, and deadline
limits. `MaxCost` limits model-call charges. Local tool charges are reported
separately and need an application-owned ceiling for an all-in price promise.
A zero token cap or nil cost cap means that dimension is uncapped.

```go
start := kernel.Start{
	ID: "start-123",
	Input: input,
	Budget: &kernel.Budget{
		MaxOutputTokens: 4_000,
		MaxTotalTokens:  12_000,
		MaxCost: &kernel.Money{
			Amount:   money.MustParseAmount("0.50"),
			Currency: "USD",
		},
	},
}
```

Kernel lowers the next request's output allowance to the remaining output
tokens and records a warning. The total-token cap is checked after each call.
For a cost cap, the prepared request must carry model prices and a finite
effective output allowance. Kernel estimates the full input and output cost
before `BeforeSend`. If it cannot bound the call or the call would exceed the
cap, the turn stops `limited` before dispatch. It does not shrink the reply to
fit a cost cap. The stop's `Limit` gives the cap, spent amount, worst case
(`CostLimit`, `CostUsed`, `CostAtRisk`), and the output-token allowance that
would fit. A call whose worst case reaches the cap exactly is admitted. If input alone consumes the remaining
cap, no positive allowance fits; raise the cap or reduce input/history.
A model without prices has an explicit `UnpricedModel` detail. A call without
a finite `MaxTokens` or with provider-hosted tools also stops before dispatch,
since its cost cannot be bounded.

`Replay(turn).Spend` contains token totals, model costs by currency, counts of
calls with estimated usage, and counts of calls with unknown or
incompatible-currency model cost. `Spend.ToolCost` reports local tool charges
by currency. Each settled model fact records its `Charge`; the intent records
`AtRisk`. When final usage is missing, Kernel preserves streamed counts,
fills absent input from the prepared request, and counts the greater of
streamed output and a visible-text estimate. It marks the call estimated.
This best guess can undercount a cut-off call, so a cost cap is not a strict
charge guarantee for that case. A tool may return `tool.Outcome.Cost`; it is
counted after the tool settles and does not consume
`MaxCost`. The application must bound and admit charged tools before execution
if it promises a cap covering them.

Amounts are exact. `money.Amount` counts nano-units (1e-9) of the currency in
an int64 and encodes in JSON as a decimal string such as `"0.5"`; it also reads
the bare JSON numbers some provider APIs report. Kernel sums charges without drift. A
worst case (`AtRisk`) rounds up to the nano, so rounding never breaches a cap,
and a settled list-price charge rounds half to even. A ledger in coarser units
converts from `Nanos()` with its own rounding, for example up for holds.

To raise a cap, authorize the increase in the application and pass the full
new budget to `Continue`. It can raise existing caps but cannot lower them or
introduce a new cap. Each raise is saved as a continuation step and appears in
`TurnView.Raises` with its time. A host credit hold needs no raise: after funds
arrive, `Continue` makes `BeforeSend` check the balance again.

An application can reserve credits in its recorder. `BeforeSend` receives a
`SendPlan` with the prepared request, `AtRisk`, and the remaining turn cost
allowance. It can return `ModelAuthorization.Admission` with the account and
amount to hold. The recorder atomically saves the model intent and reserves
that amount, using the attempt ID as the idempotency key. On settlement it
replaces the hold with `Charge`; on a result with no usage, it may retain the
hold until its own reconciliation. A credit denial returns a `Limit` of kind
`host` and an application code such as `insufficient_credits`, yielding a
resumable `limited` stop. A denial that reports money puts it in the `Cost`
fields with a `Currency` (an ISO 4217 code or the host's credit unit) and keeps
`Limit` and `Used` for counts. Credit balances and plan ceilings remain with the
application.
