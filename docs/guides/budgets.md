# Turn budgets

The application sets a turn's optional budget on `Start`. A definition has no
budget; it still owns the existing model-attempt, interruption, and deadline
limits. A zero token cap or nil cost cap means that dimension is uncapped.

```go
start := kernel.Start{
	ID: "start-123",
	Input: input,
	Budget: &kernel.Budget{
		MaxOutputTokens: 4_000,
		MaxTotalTokens:  12_000,
		MaxCost: &kernel.Money{
			Amount:   0.50,
			Currency: "USD",
		},
	},
}
```

Kernel lowers the next request's output allowance to the remaining output
tokens and records a warning. The total-token cap is checked after each call.
For a cost cap, the prepared request must carry model prices and an effective
output allowance. Kernel estimates the full input and output cost before
`BeforeSend`; if it cannot bound the call, or it would exceed the cap, the turn
stops `limited` before dispatch. It does not silently shorten an answer to fit
a cost cap. The stop's `Limit` gives the cap, spent amount, worst case, and the
output-token allowance that would fit. A model without prices has an explicit
`UnpricedModel` detail. Provider-hosted tools have no bounded request count,
so a capped turn cannot dispatch a request offering them.

`Replay(turn).Spend` contains token totals, costs by currency, the number of
calls whose usage had to be estimated, and the number with unknown or
incompatible-currency cost. Each settled model fact records its `Charge`; the
intent records `AtRisk`. When no usage arrived, Kernel estimates input from
the prepared request and output from the text observed, then marks the call as
estimated. This best guess can undercount a cut-off call, so a cost cap is not
a strict guarantee for that case. A tool may return `tool.Outcome.Cost`; it is
counted after the tool settles. Tool cost cannot be reserved by Kernel ahead
of execution.

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
resumable `limited` stop. Credit balances and plan ceilings remain with the
application.
