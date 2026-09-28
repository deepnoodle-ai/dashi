package llm

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/tool"
)

func TestRequestOwnsSnapshots(t *testing.T) {
	r := Input{
		Model: "model",
		Messages: []Message{
			{
				Role: RoleUser,
				Content: []Content{
					Text("hello"),
				},
			},
		},
		Tools: []tool.Definition{
			{
				Name:   "act",
				Schema: json.RawMessage(`{"type":"object"}`),
			},
		},
	}
	body := []byte(`{"stream":false}`)
	adjustments := []Adjustment{
		{
			Field:     SettingMaxTokens,
			Action:    AdjustmentDefault,
			Requested: json.RawMessage("null"),
			Effective: json.RawMessage("1024"),
			Reason:    "default",
		},
	}
	p, err := NewRequest(RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "api",
			Model:    "model",
			Scope:    "scope",
		},
		Input: r,
		Effective: Settings{
			MaxTokens: 1024,
		},
		Body:        body,
		Adjustments: adjustments,
	})
	if err != nil {
		t.Fatalf(
			"create prepared request for ownership check: %v",
			err,
		)
	}

	// Mutate the inputs and every returned value; none may reach p.
	r.Messages[0].Content[0].Block.Text.Text = "changed"
	r.Tools[0].Schema[0] = '['
	body[0] = '['
	adjustments[0].Reason = "changed"
	adjustments[0].Effective[0] = '9'
	copy := p.Input()
	copy.Messages[0].Content[0].Block.Text.Text = "also changed"
	p.Body()[0] = '['
	p.Adjustments()[0].Reason = "also changed"
	p.Adjustments()[0].Effective[0] = '9'

	if got := p.Input().Messages[0].Text(); got != "hello" {
		t.Fatalf("request text = %q, want %q", got, "hello")
	}
	if !json.Valid(p.Input().Tools[0].Schema) {
		t.Fatal("tool schema was shared")
	}
	if !json.Valid(p.Body()) {
		t.Fatal("body was shared")
	}
	if string(p.Adjustments()[0].Effective) != "1024" {
		t.Fatal("adjustment values were shared")
	}
	if got := p.Adjustments()[0].Reason; got != "default" {
		t.Fatalf("adjustment reason = %q, want %q", got, "default")
	}
}

func TestInputValidateRejectsInvalid(t *testing.T) {
	schema := json.RawMessage(`{}`)
	tests := []struct {
		name    string
		request Input
	}{
		{
			name: "content kind mismatch",
			request: Input{
				Model: "m",
				Messages: []Message{
					{
						Role: RoleUser,
						Content: []Content{
							{
								Kind:  ContentToolCall,
								Block: Text("x").Block,
							},
						},
					},
				},
			},
		},
		{
			name: "unknown role",
			request: Input{
				Model: "m",
				Messages: []Message{
					{
						Role: "unknown",
					},
				},
			},
		},
		{
			name: "tool choice names undefined tool",
			request: Input{
				Model: "m",
				Settings: Settings{
					ToolChoice: &ToolChoice{
						Mode: ToolChoiceTool,
						Name: "missing",
					},
				},
			},
		},
		{
			name: "reasoning effort and budget",
			request: Input{
				Model: "m",
				Settings: Settings{
					Reasoning: &ReasoningSettings{
						Effort:       "high",
						BudgetTokens: 1000,
					},
				},
			},
		},
		{
			name: "duplicate tool names",
			request: Input{
				Model: "m",
				Tools: []tool.Definition{
					{
						Name:   "x",
						Schema: schema,
					},
					{
						Name:   "x",
						Schema: schema,
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.request.Validate(); err == nil {
				t.Fatalf("accepted %+v", tt.request)
			}
		})
	}
}
