package providercontent

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestAdaptAnthropicToolCallerRequiresKnownDirectShape(t *testing.T) {
	source := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "fixture",
		Scope:    "fixture",
	}
	for _, tc := range []struct {
		name     string
		caller   string
		extra    string
		portable bool
	}{
		{name: "absent", portable: true},
		{name: "direct", caller: `{"type":"direct"}`, portable: true},
		{name: "null", caller: `null`},
		{name: "string", caller: `"direct"`},
		{name: "array", caller: `[]`},
		{name: "empty", caller: `{}`},
		{name: "future", caller: `{"type":"future"}`},
		{name: "wrong type", caller: `{"type":null}`},
		{name: "dependent field", caller: `{"type":"direct","tool_id":"server"}`},
		{
			name:   "programmatic",
			caller: `{"type":"code_execution_20260120","tool_id":"server"}`,
		},
		{
			name:   "unknown outer field",
			caller: `{"type":"direct"}`,
			extra:  `,"signature":"private"`,
		},
	} {
		for _, mode := range []string{"unchanged", "edited", "foreign"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				raw := `{"type":"tool_use","id":"call","name":"act","input":{}`
				if tc.caller != "" {
					raw += `,"caller":` + tc.caller
				}
				raw += tc.extra + `}`
				call := tool.Call{ID: "call", Name: "act", Args: []byte(`{}`)}
				if mode == "edited" {
					call.Args = []byte(`{"n":1}`)
				}
				input := llm.Input{Messages: []llm.Message{
					llm.AssistantMessage(llm.Content{
						Kind:     llm.ContentToolCall,
						ToolCall: &call,
						Replay: &llm.Opaque{
							Version: 1,
							Origin:  source,
							Type:    "tool_use",
							Unit:    llm.OpaqueUnitContentBlock,
							Raw:     json.RawMessage(raw),
						},
					}),
					llm.UserMessage(llm.Content{
						Kind:       llm.ContentToolResult,
						ToolResult: &llm.ToolResult{CallID: call.ID},
					}),
				}}
				original, _ := json.Marshal(input)
				target := source
				if mode == "foreign" {
					target.API = "openai.responses"
					target.Provider = "openai"
				}
				out, changes, err := Adapt(input, target)
				wantSuccess := mode == "unchanged" || tc.portable
				if (err == nil) != wantSuccess {
					t.Fatalf("caller acceptance = %v, want %v: %v", err == nil, wantSuccess, err)
				}
				after, _ := json.Marshal(input)
				if string(after) != string(original) {
					t.Fatal("caller classification changed saved history")
				}
				if err != nil {
					return
				}
				replay := out.Messages[0].Content[0].Replay
				if mode == "foreign" {
					if replay != nil {
						t.Fatal("foreign direct caller retained private replay")
					}
					if len(changes) != 1 || changes[0].Action != llm.ContentChangeStripReplay {
						t.Fatalf("conversion lost its decision: %+v", changes)
					}
				} else if replay == nil || string(replay.Raw) != raw {
					t.Fatal("compatible caller data changed")
				}
			})
		}
	}
}
