# R23: Return a result to the right Turn and call

[All examples](README.md) · [Review](../README.md)

Turns A and B both have a provider call named call-1. Swarm stores tool jobs
with a hash of Turn ID plus call ID, then searches the saved view to recover the
provider ID when a result arrives.

## Before: current code

From [swarm/internal/runtime/dashirt/dashirt.go:181–182](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L181). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 181 182 -->
```go
func digest(data []byte) string        { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func journalID(turn, id string) string { return "tool:" + digest([]byte(turn+"\x00"+id)) }
```

From [swarm/internal/runtime/dashirt/dashirt.go:935–944](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L935). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 935 944 -->
```go
native := ""
for call := range view.Tools {
	if journalID(id, call) == res.ToolCallID {
		native = call
		break
	}
}
if native == "" {
	return errors.New("tool result does not belong to the admitted Turn")
}
```

## After: Existing API

This uses current Dashi APIs with illustrative application storage methods.

```go
// Application envelope change using Dashi's existing CallRef.
ref := res.Call // dashi.CallRef{TurnID: "turn-A", CallID: "call-1"}
if ref.TurnID != turn.ID { return ErrWrongTurn }
pending, ok := pendingByRef[ref]
if !ok { return verifyPreviouslyResolvedResult(ref, res.Outcome) }
command := pending.Result(res.Outcome) // Existing builder.
return repo.PlanCommand(ctx, turn.ID, command)
```

**Observable result:** Turn A/call-1 and Turn B/call-1 would remain distinct without reversing a hash. Pending.Result already constructs the correct Resolve command.

**Developer benefit:** Remove view scans and hand-built result command IDs after updating Swarm's own envelopes. No new Dashi concept is required for this change. Optional key encoding can wait until remaining friction is demonstrated.

**Application responsibility:** Keep native IDs beside any opaque journal key, validate ownership/membership, and handle duplicate completed results.

**Runnable evidence:** `Example_pendingResult` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.
