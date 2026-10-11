# R21: Deliver text while the model is generating it

[All examples](README.md) · [Review](../README.md)

The agent takes several seconds to produce a reply. This Swarm watch path only
sees committed records, while Dashi already supports temporary model deltas
and tool progress for responsive presentation.

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:676–676](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L676). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 676 676 -->
```go
return dashi.Runner{Client: client, Tools: decls, Recorder: &recorder{runtime: r, owner: owner}, Hooks: hooks, OnContextDone: dashi.ContextInterrupt}, nil
```

## After: Existing API

This uses existing Dashi APIs. Application variables come from the surrounding
integration; the snippet is not a standalone program.

```go
// Existing APIs. The host owns workerCtx and joins its worker on shutdown.
var hub feed.Hub
runner.Observer = hub.Observe
// The background worker calls runner.Run(workerCtx, turn, start).
// Each browser connection has a different requestCtx:
for event, err := range feed.Follow(requestCtx, &hub, store, turnID, cursor) {
    if err != nil { return err }
    if event.Gap {
        clearIncompletePreview(event.MessageID)
        showWaitingForSavedReply(event.MessageID)
        continue // Keep following: the committed finish record still matters.
    }
    if err := sendPreviewOrRecord(event); err != nil { return err }
}
```

**Observable result:** Connected readers can receive deltas before the finished message. A reader missing deltas clears that incomplete preview and keeps following to the saved reply. After an early run/commit failure, reload saved state. Disconnecting a reader leaves the worker alone.

**Developer benefit:** Use existing streaming support. This improves responsiveness and may add UI code rather than remove it.

**Application responsibility:** Own the worker, reader access, and UI. Hub previews are local to a process; serving previews from a separate process requires a host transport preserving sequence/gaps. Distinguish previews and summary text from saved replies, billing, and published posts.

**Runnable evidence:** `Example_readerDisconnect` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.

**Additional runnable evidence:** Example_previewGapThenReply in [the offline demos](demo/).
