# R30: Wait to speak until the user and playback are ready

[All examples](README.md) · [Review](../README.md)

A voice tool finishes while the user is speaking. Earlier response audio is
also still playing. The provider needs an explicit Respond after tools, but
Swarm must defer it until both conditions clear and discard canceled tool results.

## Before: current code

From [swarm/internal/manager/voice/service.go:403–415](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/manager/voice/service.go#L403). Surrounding definitions are omitted.

<!-- source: swarm internal/manager/voice/service.go 403 415 -->
```go
if cmd.ToolResult != nil {
	s.mu.Lock()
	call := l.calls[cmd.ToolResult.CallID]
	canceled := call != nil && call.canceled
	if !canceled && l.conn.Info().NeedsResponseAfterTools {
		l.pendingRespond = true
	}
	s.mu.Unlock()
	if canceled {
		continue
	}
}

```

From [swarm/internal/manager/voice/service.go:551–563](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/manager/voice/service.go#L551). Surrounding definitions are omitted.

<!-- source: swarm internal/manager/voice/service.go 551 563 -->
```go
func (s *Service) respondLocked(l *live) {
	if !l.pendingRespond || l.responseActive || l.userSpeaking || l.response != "" && !l.drained[l.response] {
		return
	}
	for _, c := range l.calls {
		if !c.finished && !c.canceled {
			return
		}
	}
	if s.enqueueLocked(l, realtime.Command{Kind: realtime.CommandRespond}) {
		l.pendingRespond = false
	}
}
```

## After: Existing API

This uses current Dashi APIs. Application storage methods are illustrative;
the snippet is not a complete program.

```go
// Candidate pure policy inside the app's single Receive/event owner.
// State comes from provider events and response-ID-matched playback acknowledgments.
if needsResponse && toolResultsSent && !responseActive &&
    !userSpeaking && playbackDrained {
    // Existing Session.Send serializes writes and validates pending calls/response state.
    return conn.Send(sessionCtx, realtime.Command{Kind: realtime.CommandRespond})
}
return nil
```

**Observable result:** The policy sends no Respond while speaking or before playback drains. This snippet is not a runnable event-loop demo or proof of cancellation correctness.

**Developer benefit:** Keep the raw Session and document/test the smaller readiness policy first. Defer NewController: existing Session already serializes writes and checks pending tools and active responses.

**Application responsibility:** Provide speech/playback evidence, run authorized tools, and own devices, quotas, session duration, client ownership, and shutdown. Keep one Receive owner, response-ID correlation, bounded queues, overflow behavior, and session-lifetime send contexts. A canceled Send context closes today's connection; canceled tool results must not be submitted. Provider completion does not prove playback completion.
