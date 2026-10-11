# R32: Try the integration without paying a provider

[All examples](README.md) · [Review](../README.md)

An adopter wants to test saving, retrying after a restart, and delivering a
reply before configuring keys or a production database. Swarm's application
adapter shows many operations at once; a small runnable example makes the core
library behavior inspectable.

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:697–704](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L697). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 697 704 -->
```go
var req runtime.TurnRequest
if err = json.Unmarshal(saved.Request, &req); err != nil {
	return false, false, err
}
var history []llm.Message
if err = json.Unmarshal(saved.History, &history); err != nil {
	return false, false, err
}
```

## After: Existing API

This uses existing Dashi APIs. Application variables come from the surrounding
integration; the snippet is not a standalone program.

```go
// Existing APIs used by the executable Example_restartAndRetry beside these notes.
store := dashi.NewMemoryRecorder()
client := dashitest.NewClient(dashitest.Reply("Order 42 is ready."))
runner := dashi.Runner{Recorder: store, Client: client}
// savedStart(), pausedTurn, and loadedTurn are shown in the complete demo.
retry, err := runner.Run(ctx, dashi.Turn{ID: pausedTurn.ID}, savedStart())
if err != nil { return err }
out, err := runner.Run(ctx, loadedTurn, dashi.Continue{
    CommandID: "continue-42", OnHistoryMismatch: dashi.HistoryMismatchReject,
})
```

**Observable result:** The complete demo prints zero records added by a duplicate Start, the reply, and one scripted model call. It uses no API key or paid service.

**Developer benefit:** Inspect actual recovery behavior and expected output before evaluating proposed convenience APIs.

**Application responsibility:** The memory store and JSON round trip demonstrate library mechanics, not SQL durability or a process crash. A future database guide must cover atomic holds/input delivery, ownership, compaction, and failed writes.

**Runnable evidence:** `Example_restartAndRetry` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.
