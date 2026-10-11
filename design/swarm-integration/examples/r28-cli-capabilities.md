# R28: Check CLI support before sending commands

[All examples](README.md) · [Review](../README.md)

A remote machine has an older Dashi binary. Swarm should determine whether it
supports prompt submission and request-correlated events before launching or
sending input, without guessing from help text.

## Before: current code

From [swarm/internal/codingcontrol/codingcontrol.go:178–183](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/codingcontrol/codingcontrol.go#L178). Surrounding definitions are omitted.

<!-- source: swarm internal/codingcontrol/codingcontrol.go 178 183 -->
```go
// Supports reports whether a CLI whose --help printed help can take the
// adapter's launch arguments. Only a Prober can be unsupported.
func Supports(a Adapter, help []byte) bool {
	p, ok := a.(Prober)
	return !ok || bytes.Contains(help, []byte(p.RequiredFlag()))
}
```

From [swarm/internal/codingcontrol/cli/dashi.go:90–92](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/codingcontrol/cli/dashi.go#L90). Surrounding definitions are omitted.

<!-- source: swarm internal/codingcontrol/cli/dashi.go 90 92 -->
```go
func (dashi) RequiredFlag() string { return "--turn-events" }

func (dashi) MinimumVersion() string { return "v0.5.0" }
```

## After: Proposed API sketch

These Dashi APIs do not exist yet. Application variables and storage methods
show the intended caller responsibilities, not a complete runnable program.

```go
// Proposed machine-readable capability handshake.
capabilities, err := cli.Capabilities(ctx)
if err != nil { return err }
if !capabilities.Supports("submit_new_turn", "request_events", "input_ready") {
    return fmt.Errorf("installed Dashi lacks the required control protocol")
}
// Reconnect reads a current snapshot and resumes from the acknowledged event cursor.
// Completion and readiness are separate events; readiness is only a UI hint.
if event.Kind == "turn_finished" { showResult(event) }
if event.Kind == "input_ready" { enableNextPrompt() }
```

**Observable result:** An unsupported installation would fail before input. input_ready would enable the UI, while submission still checks admission and can report busy. Stale readiness must never turn a new prompt into steering.

**Developer benefit:** Remove help scraping and implicit parser/version assumptions. Shared schema/client code would also reduce duplicate parsing.

**Application responsibility:** Choose required capabilities and handle rejected or truncated events. Preserve the current hook-event readiness guarantee during protocol changes.
