# R07: Look up one configured model consistently

[All examples](README.md) · [Review](../README.md)

The operator selects `openai/gpt-example`. Swarm must find its client for
running work and recover its provider/API identity when canceling work after
the key has been removed.

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:124–130](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L124). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 124 130 -->
```go
func route(model string) (string, string, error) {
	provider, native, ok := strings.Cut(model, "/")
	if !ok || provider == "" || native == "" {
		return "", "", fmt.Errorf("model must name provider/model")
	}
	return provider, native, nil
}
```

From [swarm/internal/runtime/dashirt/dashirt.go:604–619](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L604). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 604 619 -->
```go
func (r *Runtime) destination(model string) (string, string, error) {
	p, _, err := route(model)
	if err != nil {
		return "", "", err
	}
	if client := r.config.Clients[p]; client != nil {
		return client.Provider(), client.API(), nil
	}
	switch p {
	case "anthropic":
		return p, messages.API, nil
	case "openai":
		return p, responses.API, nil
	}
	return p, "swarm.unconfigured", nil
}
```

## After: Existing API

This uses current Dashi APIs with illustrative application storage methods.

```go
// Smaller alternative: application-owned table, with identity separate from keys.
type Destination struct {
    Provider, API, Model, EndpointKey string
    Client llm.Client // Optional: identity remains available after keys disappear.
}
destination, ok := destinations["openai/gpt-example"]
if !ok { return ErrUnknownModel }
agent.Provider, agent.API, agent.Model = destination.Provider, destination.API, destination.Model
if destination.Client == nil { return requestCredentialRepair(requestID) }
// Save EndpointKey in application admission; Agent fields alone do not pin it.
```

**Observable result:** Both paths can read the same application identity entry. Execution still needs credentials; endpoint binding remains application configuration.

**Developer benefit:** Compare this small application table first. Defer a Dashi registry until another caller shows shared lookup behavior worth standardizing.

**Application responsibility:** Configure and authorize destinations and credentials. Distinct provider APIs/endpoints need distinct registry entries.
