package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/mcp"
	"github.com/deepnoodle-ai/dashi/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func ExampleConvertResult() {
	outcome, warnings, err := mcp.ConvertResult(exampleScreenshot())
	if err != nil {
		panic(err)
	}
	fmt.Println("content:", outcome.Output.Content[0].Kind, outcome.Output.Content[1].Kind)
	fmt.Println("warnings:", len(warnings))
	// Output:
	// content: text image
	// warnings: 0
}

func ExampleConvertResultJSON() {
	// The host saved this format with the admitted tool, before execution.
	admittedFormat := "mcp_result"
	journal := struct {
		Output  json.RawMessage
		IsError bool
	}{exampleScreenshotJSON(), true}
	if admittedFormat != "mcp_result" {
		return // The host uses its application-result path for other formats.
	}
	outcome, _, err := mcp.ConvertResultJSON(journal.Output)
	if err != nil {
		panic(err)
	}
	if journal.IsError {
		outcome.State = tool.StateFailed // Either journal or MCP failure wins.
	}
	fmt.Println("image:", outcome.Output.Content[1].Kind)
	fmt.Println("model content:", outcome.Output.Content[2].Text.Text)
	fmt.Println("outcome:", outcome.State)
	// Output:
	// image: image
	// model content: {"n":9007199254740993}
	// outcome: failed
}

func ExampleConvertResultJSON_unusable() {
	runner, pending := exampleExternalResult()
	journal := json.RawMessage(`{"content":[{"type":"image","data":"bm90IGFuIGltYWdl","mimeType":"image/png"}]}`)
	original := bytes.Clone(journal)
	outcome, _, err := mcp.ConvertResultJSON(journal)
	switch {
	case errors.Is(err, mcp.ErrInputRequired):
		outcome = tool.Failed("This host does not support additional MCP input.")
	case err != nil:
		outcome = tool.Failed("Tool completed, but its result could not be used.")
	}
	saved, err := runner.Run(
		context.Background(), pending.Turn, pending.Pending[0].Result(outcome),
	)
	if err != nil {
		panic(err)
	}
	fmt.Println("original journal retained:", bytes.Equal(journal, original))
	dispatches := 0
	for _, step := range saved.Turn.Steps {
		if step.Kind == dashi.StepToolWaiting {
			dispatches++
		}
		if step.Kind == dashi.StepToolFinished {
			fmt.Println("saved result:", step.Tool.Outcome.State)
		}
	}
	fmt.Println("external handoffs:", dispatches)
	fmt.Println("new pending work:", len(saved.Pending))
	// Output:
	// original journal retained: true
	// saved result: failed
	// external handoffs: 1
	// new pending work: 0
}

func exampleScreenshot() *sdk.CallToolResult {
	var pixel bytes.Buffer
	if err := png.Encode(&pixel, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		panic(err)
	}
	return &sdk.CallToolResult{
		Content: []sdk.Content{
			&sdk.TextContent{Text: "Screenshot"},
			&sdk.ImageContent{Data: pixel.Bytes(), MIMEType: "image/png"},
		},
		StructuredContent: map[string]any{"n": json.Number("9007199254740993")},
	}
}

func exampleScreenshotJSON() json.RawMessage {
	raw, err := json.Marshal(exampleScreenshot())
	if err != nil {
		panic(err)
	}
	return raw
}

// The model requested one externally executed tool. Its journaled completion
// belongs to the application; settling it cannot repeat that execution.
func exampleExternalResult() (dashi.Runner, dashi.RunResult) {
	runner := dashi.Runner{
		Client: dashitest.NewClient(
			dashitest.CallTool("screenshot-1", "screenshot", "{}"),
			dashitest.Reply("The screenshot could not be used."),
		),
		Recorder: dashi.NewMemoryRecorder(),
		Tools: tool.List{
			{
				Definition: tool.Definition{
					Name:        "screenshot",
					Description: "Capture a screenshot in the host.",
					Kind:        tool.KindExternal,
					Schema:      json.RawMessage(`{"type":"object"}`),
				},
			},
		},
	}
	out, err := runner.Run(context.Background(), dashi.Turn{ID: "screenshot"}, dashi.Start{
		CommandID: "start",
		Agent: dashi.Agent{
			Provider: dashitest.Provider,
			API:      dashitest.API,
			Model:    dashitest.Model,
		},
		Input: []llm.Message{llm.UserMessage(llm.Text("Capture the screen."))},
	})
	if err != nil {
		panic(err)
	}
	return runner, out
}
