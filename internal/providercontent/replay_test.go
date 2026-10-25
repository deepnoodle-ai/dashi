package providercontent

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// An Anthropic tool call can move to another provider unless Anthropic's code
// execution tool made it. Every call stays usable on Anthropic.
func TestAdaptAnthropicToolCallToAnotherProvider(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller string
		stay   bool
		ok     bool
	}{
		{name: "no caller", ok: true},
		{name: "direct", caller: `,"caller":{"type":"direct"}`, ok: true},
		{name: "direct with extra fields", caller: `,"caller":{"type":"direct","tool_id":"server"}`},
		{name: "code execution", caller: `,"caller":{"type":"code_execution_20260120","tool_id":"server"}`},
		{name: "code execution on Anthropic", caller: `,"caller":{"type":"code_execution_20260120","tool_id":"server"}`, stay: true, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := content.Origin{Provider: "anthropic", API: "anthropic.messages", Model: "fixture", Scope: "fixture"}
			call := tool.Call{ID: "call", Name: "act", Args: []byte(`{}`)}
			input := llm.Input{Messages: []llm.Message{
				llm.AssistantMessage(llm.Content{
					Kind:     llm.ContentToolCall,
					ToolCall: &call,
					Replay: &llm.Opaque{
						Version: 1,
						Origin:  source,
						Type:    "tool_use",
						Unit:    llm.OpaqueUnitContentBlock,
						Raw:     json.RawMessage(`{"type":"tool_use","id":"call","name":"act","input":{}` + tc.caller + `}`),
					},
				}),
				llm.UserMessage(llm.Content{
					Kind:       llm.ContentToolResult,
					ToolResult: &llm.ToolResult{CallID: call.ID},
				}),
			}}
			target := content.Origin{Provider: "openai", API: "openai.responses", Model: "fixture", Scope: "fixture"}
			if tc.stay {
				target = source
			}
			out, _, err := Adapt(input, target)
			if (err == nil) != tc.ok {
				t.Fatalf("Adapt error = %v, want ok = %v", err, tc.ok)
			}
			if err == nil && (out.Messages[0].Content[0].Replay != nil) != tc.stay {
				t.Fatalf("replay kept = %v, want %v", out.Messages[0].Content[0].Replay != nil, tc.stay)
			}
		})
	}
}

// A Responses message part can move to another provider when its phase is
// absent or one OpenAI documents. GPT-6 marks its answers "final_answer".
// An unfamiliar phase stays on OpenAI.
func TestAdaptResponsesMessagePhaseToAnotherProvider(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase string
		ok    bool
	}{
		{name: "no phase", ok: true},
		{name: "final answer", phase: `,"phase":"final_answer"`, ok: true},
		{name: "commentary", phase: `,"phase":"commentary"`, ok: true},
		{name: "unfamiliar", phase: `,"phase":"planning"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := content.Origin{Provider: "openai", API: "openai.responses", Model: "fixture", Scope: "fixture"}
			text := content.TextBlock("Done.")
			input := llm.Input{Messages: []llm.Message{
				llm.UserMessage(llm.Text("Go.")),
				llm.AssistantMessage(llm.Content{
					Kind:  llm.ContentBlock,
					Block: &text,
					Replay: &llm.Opaque{
						Version: 1,
						Origin:  source,
						Type:    "output_text",
						Unit:    llm.OpaqueUnitMessagePart,
						Raw: json.RawMessage(`{"message":{"type":"message","id":"msg_1","role":"assistant","status":"completed"` + tc.phase +
							`},"part":{"type":"output_text","text":"Done.","annotations":[],"logprobs":[]}}`),
					},
				}),
			}}
			target := content.Origin{Provider: "meta", API: "openai.completions", Model: "fixture", Scope: "fixture"}
			out, changes, err := Adapt(input, target)
			if (err == nil) != tc.ok {
				t.Fatalf("Adapt error = %v, want ok = %v", err, tc.ok)
			}
			if err != nil {
				return
			}
			if out.Messages[1].Content[0].Replay != nil {
				t.Fatal("replay kept for another provider")
			}
			if len(changes) != 1 || changes[0].Action != llm.ContentChangeStripReplay {
				t.Fatalf("changes = %+v, want one strip_replay", changes)
			}
		})
	}
}
