# R13: Round costs consistently and explain request estimates

[All examples](README.md) · [Review](../README.md)

The database stores micro-USD while Dashi represents nanos. A 1,001-nano call
must charge two micro-USD. Separately, a “can I afford this model?” preview needs
to distinguish an approximate size estimate from the actual reservation.

## Before: current code

From [swarm/internal/runtime/dashirt/recording.go:14–22](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/recording.go#L14). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/recording.go 14 22 -->
```go
func microUSD(nanos int64) (int64, error) {
	if nanos < 0 {
		return 0, errors.New("negative model cost")
	}
	if nanos > math.MaxInt64-999 {
		return 0, errors.New("model cost conversion overflow")
	}
	return (nanos + 999) / 1000, nil
}
```

From [swarm/internal/runtime/dashirt/dashirt.go:158–179](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L158). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 158 179 -->
```go
func (r *Runtime) ReservationMicroUSD(ctx context.Context, model string, inputBytes int) (int64, bool) {
	p, native, err := route(model)
	if err != nil || r.config.Clients[p] == nil || inputBytes < 0 {
		return 0, false
	}
	input := llm.Input{Model: native, Settings: llm.Settings{MaxTokens: defaultMaxOutputTokens},
		Messages: []llm.Message{llm.UserMessage(llm.Text(strings.Repeat("x", inputBytes)))}}
	request, err := r.config.Clients[p].Prepare(ctx, input)
	if err != nil {
		return 0, false
	}
	info := request.Snapshot().ModelInfo
	if info == nil || info.Pricing == nil || info.Pricing.Currency != "USD" {
		return 0, false
	}
	cost, ok := llm.WorstCaseCost(request, *info.Pricing)
	if !ok {
		return 0, false
	}
	micro, err := microUSD(cost.Total.Nanos())
	return micro, err == nil
}
```

## After: Existing API

This uses current Dashi APIs with illustrative application storage methods.

```go
// Small local alternative: nonnegative model charge, rounded up, no nanos+999 overflow.
if nanos < 0 { return ErrNegativeModelCost }
micros := nanos / 1000
if nanos % 1000 != 0 { micros++ }
// A proposed generic Units helper must preserve signed Amount/refund semantics
// and name rounding direction explicitly, e.g. toward positive infinity.
// Existing real-request bound; an approximate UI preview is not admission evidence.
actualRisk, bounded := llm.WorstCaseCost(prepared, pricing)
```

**Observable result:** 1,001 nanos converts to two micros; negative model charges are rejected separately. A future generic converter must define signed rounding for refunds. The UI estimate would remain labeled approximate; actual admission would use the real request.

**Developer benefit:** A shared conversion helper could remove a few lines. The local arithmetic may be enough; defer a broad EstimateRequest API until a concrete UI consumer needs it.

**Application responsibility:** Choose the ledger unit, validate currency, round each call consistently, and enforce the real bound. This is a small helper improvement, not a billing framework.

**Runnable evidence:** `Example_correctionRounding` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.
