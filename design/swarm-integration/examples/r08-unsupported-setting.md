# R08: Drop only a setting the model rejected

[All examples](README.md) · [Review](../README.md)

The Writing desk asks for reasoning effort. One model does not support that
setting. A different request has an invalid tool schema: removing reasoning
must not hide that unrelated error.

## Before: current code

From [swarm/internal/runtime/dashirt/generate.go:66–75](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/generate.go#L66). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/generate.go 66 75 -->
```go
request, err := client.Prepare(ctx, input)
if err != nil && input.Settings.Reasoning != nil {
	// A model without an effort setting refuses it at preparation,
	// before anything is sent; it gets the request without one.
	input.Settings.Reasoning = nil
	request, err = client.Prepare(ctx, input)
}
if err != nil {
	return runtime.GenerateResult{}, err
}
```

## After: Proposed API sketch

These Dashi APIs do not exist yet. Application variables and storage methods
show the intended caller responsibilities, not a complete runnable program.

```go
// Proposed typed setting rejection, reported before any send.
request, err := client.Prepare(ctx, input)
var unsupported *llm.UnsupportedSetting
if errors.As(err, &unsupported) && unsupported.Setting == "reasoning" {
    input.Settings.Reasoning = nil
    request, err = client.Prepare(ctx, input)
}
if err != nil { return err }
return sendPrepared(request)
```

**Observable result:** Unsupported reasoning would trigger one preparation retry. An invalid schema would retain its own error and would not trigger the fallback.

**Developer benefit:** Stop masking preparation failures while retaining optional-setting negotiation.

**Application responsibility:** Decide which settings are optional and report effective settings. Existing capability policies and Adjustments should supply the implementation.
