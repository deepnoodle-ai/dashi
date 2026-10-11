# R27: Submit literal text to the coding CLI

[All examples](README.md) · [Review](../README.md)

Swarm wants to send `[from @lead, prompt 1] Check order 42`. Today it must escape
mention-like text, paste it into the editor, and use control-key sequences to
clear or interrupt. A program should be able to submit the literal request.

## Before: current code

From [swarm/internal/codingcontrol/cli/dashi.go:84–88](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/codingcontrol/cli/dashi.go#L84). Surrounding definitions are omitted.

<!-- source: swarm internal/codingcontrol/cli/dashi.go 84 88 -->
```go
func (dashi) ClearInput() []byte {
	out := []byte{'x'}
	out = append(out, bytes.Repeat([]byte{0x0b, 0x04}, clearRounds)...)
	return append(out, bytes.Repeat([]byte{0x15, 0x7f}, clearRounds)...)
}
```

From [swarm/internal/codingcontrol/cli/dashi.go:98–107](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/codingcontrol/cli/dashi.go#L98). Surrounding definitions are omitted.

<!-- source: swarm internal/codingcontrol/cli/dashi.go 98 107 -->
```go
func (dashi) PasteText(text string) string {
	var b strings.Builder
	for i, r := range text {
		if r == '@' && mentionStart(text[:i]) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
```

## After: Proposed API sketch

The proposed Dashi helper does not exist yet. Application storage methods
show the responsibilities that remain with the caller.

```go
// Proposed explicit operation: never reinterpret a new Turn as steering.
receipt, err := cli.SubmitNewTurn(ctx, cli.Prompt{
    SessionID: sessionID, RequestID: "prompt-42",
    Text: "[from @lead, prompt 1] Check order 42",
})
switch {
case errors.Is(err, cli.ErrRejected):
    return showPromptRejected("prompt-42", err)
case err != nil:
    return showAcceptanceUnknown(sessionID, "prompt-42")
    // Query/retry identical session/request ID and payload; never mint a new ID.
default:
    return watchCLIRequest(ctx, receipt.RequestID)
}
// Steer(turnID, ...) and Enqueue(...) are separate explicit operations.
// Interrupt(turnID) replaces Escape when the user requests interruption.
```

**Observable result:** The CLI would accept the exact @lead text with no accidental file mention. A durable receipt would identify prompt-42 before a Turn starts. Lost acknowledgment means unknown acceptance, not rejection; retry/query the same session/request pair.

**Developer benefit:** Delete mention escaping/unescaping and editor-clearing key sequences from machine submission. Keep a terminal for human use.

**Application responsibility:** Connect only to the intended local process, respect readiness and approvals, and preserve request identity across retries and CLI restarts. Acceptance must follow persistence; conflicting ID/payload reuse fails. Compare a small command port with the planned ACP adapter; both should use the same session input operations.
