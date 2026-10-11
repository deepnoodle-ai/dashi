# R17: Keep a tool-heavy Turn within its context window

[All examples](README.md) · [Review](../README.md)

An agent reads several large tool results while answering one request. Its
current Turn grows too large. Swarm's Start configuration does not enable
Dashi's existing summarization within a Turn.

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:867–867](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L867). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 867 867 -->
```go
start.Agent = dashi.Agent{ID: req.AgentID, Version: saved.BehaviorRevision, Provider: provider, API: api, Model: native, Instructions: req.Instructions, Settings: llm.Settings{MaxTokens: maxTokens}, NoTools: req.Purpose == "compaction", Limits: dashi.Limits{ModelCalls: req.MaxIterations, Resumes: 100, ProviderPauses: -1, ToolConcurrency: len(req.Tools) + 1, NoReservedFinalReply: true, Deadline: req.Deadline}}
```

## After: Existing API

This uses existing Dashi APIs. Application variables come from the surrounding
integration; the snippet is not a standalone program.

```go
// Existing API: set before accepting the Turn, not halfway through a continuation.
start.Agent.ActiveCompaction = &dashi.ActiveCompactionPolicy{
    Window: 128000, Fraction: 0.8, MaxTokens: 2048,
}
out, err := runner.Run(ctx, turn, start, dashi.WithOwner(owner.OwnerID))
// ModelPurposeCompaction distinguishes summary calls from the user-facing reply.
```

**Observable result:** Dashi can summarize completed exchanges inside the Turn before further model work. Swarm may avoid closing it merely to summarize and start a successor.

**Developer benefit:** Adopt an existing feature instead of building another current-Turn summarization path. No universal recovery or cost saving is promised.

**Application responsibility:** Pin the policy at Start, budget summary calls, and validate behavior with tool outputs and images. This does not rewrite prior history supplied by the application. Today ActiveCompaction requires a live Client even with an explicit Window, so it breaks R01's no-client queued-cancellation path. Qualify or fix record-only admission before enabling it in Swarm; preserve resolved window evidence.

**Runnable evidence:** `Example_activeCompaction` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.
