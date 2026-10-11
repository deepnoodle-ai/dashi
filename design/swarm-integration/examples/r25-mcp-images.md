# R25: Give the model an MCP screenshot as an image

[All examples](README.md) · [Review](../README.md)

The user asks the agent to inspect a screenshot produced by an MCP tool.
This is the clearest implemented change with a direct input defect to fix.

## Sequence where it matters

1. The user connects an MCP server that exposes a screenshot tool and selects
   a model that accepts images.
2. The user asks, “Look at the checkout page and tell me why the button is hidden.”
3. The agent calls that admitted MCP tool. The server returns an object containing
   a content array with a PNG image and its base64-encoded bytes.
4. Swarm saves the whole response object in its tool journal and submits the
   result to its Dashi adapter.
5. Today the adapter tries to decode the object as a standalone array. That
   fails, so it creates a text result containing the JSON. The model gets no
   screenshot as visual input, even though the screenshot was captured.
6. With explicit MCP conversion, the adapter reads the whole envelope and
   creates an image block. The next model request can carry the screenshot
   as an image and the agent can inspect it.

The measured difference is the model input: JSON text before, typed image after.
The code does not establish that the model will diagnose the hidden button correctly.
The existing offline mismatch demo reproduces steps 3–5; the implementation
examples show the conversion at step 6. This is not a claimed production incident.

**Judgment:** Keep. A smaller fix is to unwrap the envelope in Swarm's current
converter. That fixes this one defect but leaves Swarm maintaining MCP content
decoding, image/audio handling, structured content, and error-state conversion.
Dashi's MCP module already converts these results for Toolset, so sharing that
conversion with application-owned tool execution has a concrete reuse case.

## Before: current code

From [swarm/internal/mcpclient/client.go:144–150](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/mcpclient/client.go#L144). Surrounding definitions are omitted.

<!-- source: swarm internal/mcpclient/client.go 144 150 -->
```go
func (c *connection) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	r, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, errors.New("MCP tool outcome unknown; not retried")
	}
	return json.Marshal(r)
}
```

From [swarm/internal/runtime/dashirt/dashirt.go:956–964](https://github.com/deepnoodle-ai/swarm/blob/a41b9a4f59617a4393e48172a97b01ca1cf76a91/internal/runtime/dashirt/dashirt.go#L956). Surrounding definitions are omitted.

<!-- source: swarm internal/runtime/dashirt/dashirt.go 956 964 -->
```go
func toolResultOutcome(res runtime.ToolResult) tool.Outcome {
	output := tool.TextOutput(string(res.Output))
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Data string `json:"data"`
		MIME string `json:"mimeType"`
	}
	if json.Unmarshal(res.Output, &blocks) == nil && len(blocks) > 0 {
```

## After: implemented in [#253](https://github.com/deepnoodle-ai/dashi/pull/253), awaiting merge

The converters are implemented. Application format selection, journal storage,
input interaction, and command persistence below remain caller responsibilities.

```go
// Admit/store an explicit output format with the tool; do not guess from JSON shape.
if admittedTool.OutputFormat != FormatMCPResult {
    return planApplicationJSONResult(journalResult) // Existing non-MCP path.
}
outcome, warnings, err := mcp.ConvertResultJSON(journalResult.Output)
if errors.Is(err, mcp.ErrInputRequired) {
    return keepOriginalRequestPending(journalResult) // Host input policy.
}
if err != nil {
    // Known completed work must be settled, not executed again.
    outcome = tool.Failed("Tool completed, but its result could not be used.")
}
if journalResult.IsError { outcome.State = tool.StateFailed } // Either error means failure.
reportConversionWarnings(warnings)
// Retain original JSON and admitted tool/version in the application journal.
return repo.PlanCommand(ctx, turnID, pending.Result(outcome))
```

**Observable result:** Today the sample becomes one text block. Correct envelope conversion produces ordered text and image blocks, so the model gets typed image content.

**Developer benefit:** Replace the manual converter and fix a real payload mismatch without giving Dashi ownership of tool execution.

**Application responsibility:** Retain the raw journal and enforce tool/network permissions. Reuse the existing conversion, then validate MIME/content. Resources may become descriptions or omission warnings. A typed SDK-result entry point is the smallest addition; raw JSON decoding must use its polymorphic content support. Never decode application JSON by shape.

The executable after-demo unwraps this envelope locally to prove the mismatch. It is explicitly a narrow prototype, not an implementation or test of the proposed full public MCP converter.

**Runnable evidence:** `Example_mcpImage` in [the offline demos](demo/).
Run `go test -v ./design/swarm-integration/examples/demo` from the repository root.
