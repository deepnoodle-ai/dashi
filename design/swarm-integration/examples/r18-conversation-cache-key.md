# R18: Use one cache identity across a conversation

[All examples](README.md) · [Review](../README.md)

Two consecutive requests use the same support conversation and mostly the
same instructions/history. Swarm currently constructs Runner without a cache
key, even though Dashi supports one.

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
// Existing API. Set this on the conversation's runner before running it.
runner.CacheKey = req.ConversationKey // Or a stable privacy-preserving hash.
out, err := runner.Run(ctx, turn, start, dashi.WithOwner(owner.OwnerID))
```

**Observable result:** Models whose catalog entry enables this default can receive the same conversation cache identity across Turns.

**Developer benefit:** Use existing provider caching support with one assignment. Provider behavior still determines cache hits and savings.

**Application responsibility:** Choose a stable identity and review what identifier is exposed to the provider. Other application billing rules remain unchanged.
