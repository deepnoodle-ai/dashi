# R05: Use conversation helpers with application IDs

[All examples](README.md) · [Review](../README.md)

The application stores request `request-42` in conversation `support-7`.
The request stops at its model-call allowance and could resume with a larger
allowance. Another request arrives. Swarm wants an explicit decision before
closing the first request, and its database IDs must remain the request IDs.
Session already protects pending approval/tool work; that is not a missing API.

## Before: current code

From [dashi/session/session.go:540–551](https://github.com/deepnoodle-ai/dashi/blob/ea65bb432ae12893cfe79e2d9bdcbbe1ea8724ed/session/session.go#L540). Surrounding definitions are omitted.

<!-- source: dashi session/session.go 540 551 -->
```go
p.warnings = append(p.warnings, unreadable...)
c.History = history
c.Agent = s.agentFor(ctx, c.Agent, last)
// Queued user input goes first, since the user typed it first. Other
// items run as a queue turn afterwards.
if items := s.free(fromUser); len(items) > 0 {
	c.Input = append(itemMessages(items), c.Input...)
	p.reserved = s.reserve(items, c.CommandID, last+1)
}
p.n, p.id, p.command, p.runner = last+1, TurnID(s.ID, last+1), c, s.runner()
s.running[p.id]++
s.driving = p.id
```

From [swarm/internal/runtime/dashirt/compaction.go:75–81](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/compaction.go#L75). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/compaction.go 75 81 -->
```go
if prior.Purpose == "compaction" {
	if !prior.Terminal || !activated[prior.ID] {
		continue
	}
} else if !prior.Terminal {
	return nil, repo.ErrBusy
}
```

## After: Proposed API sketch

These Dashi APIs do not exist yet. Application variables and storage methods
show the intended caller responsibilities, not a complete runnable program.

```go
// Proposed extension to Session; this is not today's constructor/API.
conversation := session.New(session.Options{
    ID: "support-7", Store: appConversationStore,
    HistoryPolicy: session.HistoryStrict,
    OpenTurnPolicy: session.OpenTurnRequireDecision,
})
out, err := conversation.Send(ctx, session.SendOptions{
    TurnID: "request-43", CommandID: "submit-43", // Separate Turn and retry identities.
}, dashi.Say("Now check order 43."))
if errors.Is(err, session.ErrOpenTurn) { return showResumeOrFinishChoice() }
return showReply(out, err)
```

**Observable result:** When admitted, the new Turn would use request-43. While request-42 remains open, admission would require a decision. A new request after a safe resumable stop would require an explicit resume-or-finish decision. Pending approval/tool protection already exists today.

**Developer benefit:** Use Session-level history and summarization without replacing application identities or writing the whole conversation layer.

**Application responsibility:** Store the ordered Turns and original history/settings. Keep approval decisions and admission transactions in the application. The proposed storage contract still needs design.
