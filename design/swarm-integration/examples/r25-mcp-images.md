# R25: Give the model an MCP screenshot as an image

[All examples](README.md) · [Review](../README.md)

An MCP server returns `{ "content": [{ "type": "image", ... }] }`.
Swarm journals that whole object. Its current converter recognizes only a
standalone content array, so the model receives JSON text instead of an image.

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

## After: Proposed API sketch

The proposed Dashi helper does not exist yet. Application storage methods
show the responsibilities that remain with the caller.

```go
// Admit/store an explicit output format with the tool; do not guess from JSON shape.
if admittedTool.OutputFormat != FormatMCPResult {
    return planApplicationJSONResult(journalResult) // Existing non-MCP path.
}
// Proposed exported converter in the separate MCP module.
outcome, warnings, err := mcp.ConvertResultJSON(journalResult.Output)
if err != nil { return err }
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
