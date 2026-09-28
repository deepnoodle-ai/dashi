package providercontent

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestAdaptOmitsOnlyUnreplayableReasoning(t *testing.T) {
	source := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "source",
		Scope:    "scope",
	}
	target := content.Origin{
		Provider: "openai",
		API:      "openai.responses",
		Model:    "target",
		Scope:    "scope",
	}
	reasoning := llm.Content{
		Kind: llm.ContentReasoning,
		Reasoning: &llm.Reasoning{
			Text: "display",
		},
		Replay: &llm.Opaque{
			Version: 1,
			Origin:  source,
			Type:    "thinking",
			Unit:    llm.OpaqueUnitContentBlock,
			Raw:     json.RawMessage(`{"type":"thinking","thinking":"private","signature":"signed"}`),
		},
	}
	input := llm.Input{
		Model: "target",
		ContentPolicy: llm.ContentPolicy{
			UnreplayableReasoning: llm.ReasoningOmit,
		},
		Messages: []llm.Message{
			llm.AssistantMessage(reasoning),
			llm.AssistantMessage(reasoning, llm.Text("answer")),
		},
	}
	out, changes, err := Adapt(input, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 ||
		len(out.Messages[0].Content) != 1 ||
		out.Messages[0].Content[0].Kind != llm.ContentBlock {
		t.Fatalf(
			"reasoning was not selectively omitted: %+v",
			out.Messages,
		)
	}
	if len(changes) != 2 ||
		changes[0].MessageIndex != 0 ||
		changes[0].ContentIndex != 0 ||
		changes[1].MessageIndex != 1 ||
		changes[1].ContentIndex != 0 {
		t.Fatalf(
			"omission indices do not refer to original content: %+v",
			changes,
		)
	}
	for _, change := range changes {
		if change.Action != llm.ContentChangeOmitReasoning {
			t.Fatalf("unexpected decision: %+v", change)
		}
	}
	if input.Messages[0].Content[0].Replay == nil ||
		len(input.Messages[0].Content) != 1 {
		t.Fatal("adaptation mutated the original history")
	}
}

func TestAdaptReasoningOmissionStaysNarrow(t *testing.T) {
	source := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "source",
		Scope:    "scope",
	}
	target := content.Origin{
		Provider: "openai",
		API:      "openai.responses",
		Model:    "target",
		Scope:    "scope",
	}
	base := llm.Content{
		Kind:      llm.ContentReasoning,
		Reasoning: &llm.Reasoning{},
		Replay: &llm.Opaque{
			Version: 1,
			Origin:  source,
			Type:    "thinking",
			Unit:    llm.OpaqueUnitContentBlock,
			Raw:     json.RawMessage(`{"type":"thinking","thinking":"private","signature":"signed"}`),
		},
	}
	for _, tc := range []struct {
		name    string
		content llm.Content
		policy  llm.ReasoningPolicy
		fail    bool
	}{
		{"strict default", base, llm.ReasoningReject, true},
		{
			"unknown field",
			withRaw(
				base,
				`{"type":"thinking","thinking":"private","signature":"signed","tool_use_id":"x"}`,
			),
			llm.ReasoningOmit,
			true,
		},
		{
			"wrong source type",
			withRaw(
				base,
				`{"type":"tool_use","thinking":"private","signature":"signed"}`,
			),
			llm.ReasoningOmit,
			true,
		},
		{"incomplete", withPartial(base), llm.ReasoningOmit, true},
		{
			"unknown wrapper",
			withVersion(
				base,
				2,
			),
			llm.ReasoningOmit,
			true,
		},
		{
			"caller authored",
			llm.Content{
				Kind: llm.ContentReasoning,
				Reasoning: &llm.Reasoning{
					Text: "private",
				},
			},
			llm.ReasoningOmit,
			false,
		},
		{
			"opaque stays strict",
			llm.Content{
				Kind:   llm.ContentOpaque,
				Opaque: base.Replay,
			},
			llm.ReasoningOmit,
			true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := llm.Input{
				Model: "target",
				ContentPolicy: llm.ContentPolicy{
					UnreplayableReasoning: tc.policy,
				},
				Messages: []llm.Message{
					llm.AssistantMessage(
						tc.content,
						llm.Text("answer"),
					),
				},
			}
			_, _, err := Adapt(input, target)
			if (err != nil) != tc.fail {
				t.Fatalf(
					"adaptation error = %v, want failure %v",
					err,
					tc.fail,
				)
			}
		})
	}
}

func TestAdaptRetainsCompatibleReasoning(t *testing.T) {
	origin := content.Origin{
		Provider: "anthropic",
		API:      "anthropic.messages",
		Model:    "m",
		Scope:    "scope",
	}
	c := llm.Content{
		Kind: llm.ContentReasoning,
		Reasoning: &llm.Reasoning{
			Text: "display",
		},
		Replay: &llm.Opaque{
			Version: 1,
			Origin:  origin,
			Type:    "thinking",
			Unit:    llm.OpaqueUnitContentBlock,
			Raw:     json.RawMessage(`{"type":"thinking","thinking":"private","signature":"signed"}`),
		},
	}
	input := llm.Input{
		Model: "m",
		ContentPolicy: llm.ContentPolicy{
			UnreplayableReasoning: llm.ReasoningOmit,
		},
		Messages: []llm.Message{
			llm.AssistantMessage(c),
		},
	}
	out, changes, err := Adapt(input, origin)
	if err != nil ||
		len(changes) != 0 ||
		len(out.Messages) != 1 ||
		out.Messages[0].Content[0].Replay == nil {
		t.Fatalf(
			"compatible reasoning changed: %+v, %+v, %v",
			out,
			changes,
			err,
		)
	}
}

func TestAdaptPlainReasoningCapability(t *testing.T) {
	origin := content.Origin{
		Provider: "openrouter",
		API:      "openai.completions",
		Model:    "m",
		Scope:    "scope",
	}
	c := llm.Content{
		Kind: llm.ContentReasoning,
		Reasoning: &llm.Reasoning{
			Text: "display",
		},
	}
	input := llm.Input{
		Model: "m",
		Messages: []llm.Message{
			llm.AssistantMessage(c),
		},
	}
	if _, _, err := Adapt(input, origin); err == nil {
		t.Fatal("unsupported plain reasoning was accepted by default")
	}
	out, changes, err := Adapt(
		input,
		origin,
		AdaptOptions{
			ReplayPlainReasoning: true,
		},
	)
	if err != nil || len(changes) != 0 || len(out.Messages) != 1 {
		t.Fatalf(
			"supported plain reasoning was changed: %+v, %+v, %v",
			out,
			changes,
			err,
		)
	}
}

func TestAdaptRejectsHistoryEmptiedByReasoningOmission(t *testing.T) {
	origin := content.Origin{
		Provider: "openai",
		API:      "openai.responses",
		Model:    "m",
		Scope:    "scope",
	}
	reasoning := llm.Content{
		Kind: llm.ContentReasoning,
		Reasoning: &llm.Reasoning{
			Text: "private",
		},
	}
	for _, tc := range []struct {
		name     string
		origin   content.Origin
		messages []llm.Message
		fail     bool
	}{
		{
			"reasoning only",
			origin,
			[]llm.Message{
				llm.AssistantMessage(reasoning),
			},
			true,
		},
		{
			"empty user then reasoning",
			origin,
			[]llm.Message{
				{
					Role: llm.RoleUser,
				},
				llm.AssistantMessage(reasoning),
			},
			true,
		},
		{
			"user text then reasoning",
			origin,
			[]llm.Message{
				llm.UserMessage(llm.Text("question")),
				llm.AssistantMessage(reasoning),
			},
			false,
		},
		{
			"anthropic instructions then reasoning",
			content.Origin{
				Provider: "anthropic",
				API:      "anthropic.messages",
				Model:    "m",
				Scope:    "scope",
			},
			[]llm.Message{
				{
					Role: llm.RoleSystem,
					Content: []llm.Content{
						llm.Text("instructions"),
					},
				},
				llm.AssistantMessage(reasoning),
			},
			true,
		},
		{
			"responses instructions then reasoning",
			origin,
			[]llm.Message{
				{
					Role: llm.RoleSystem,
					Content: []llm.Content{
						llm.Text("instructions"),
					},
				},
				llm.AssistantMessage(reasoning),
			},
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := llm.Input{
				Model: "m",
				ContentPolicy: llm.ContentPolicy{
					UnreplayableReasoning: llm.ReasoningOmit,
				},
				Messages: tc.messages,
			}
			_, _, err := Adapt(input, tc.origin)
			if (err != nil) != tc.fail {
				t.Fatalf(
					"usable history check = %v, want failure %v",
					err,
					tc.fail,
				)
			}
		})
	}
	input := llm.Input{
		Model: "m",
		Messages: []llm.Message{
			{
				Role: llm.RoleUser,
			},
		},
	}
	if _, _, err := Adapt(input, origin); err != nil {
		t.Fatalf(
			"preexisting empty input behavior changed without omission: %v",
			err,
		)
	}
}

func TestAdaptRecognizesOnlyCompleteReasoningSourceUnits(t *testing.T) {
	target := content.Origin{
		Provider: "other",
		API:      "openai.responses",
		Model:    "target",
		Scope:    "scope",
	}
	for _, tc := range []struct {
		name   string
		origin content.Origin
		kind   string
		unit   llm.OpaqueUnit
		raw    string
		valid  bool
	}{
		{
			"anthropic redacted",
			content.Origin{
				Provider: "anthropic",
				API:      "anthropic.messages",
				Model:    "source",
				Scope:    "scope",
			},
			"redacted_thinking",
			llm.OpaqueUnitContentBlock,
			`{"type":"redacted_thinking","data":"encrypted"}`,
			true,
		},
		{
			"responses reasoning",
			content.Origin{
				Provider: "openai",
				API:      "openai.responses",
				Model:    "source",
				Scope:    "scope",
			},
			"reasoning",
			llm.OpaqueUnitItem,
			`{"type":"reasoning","id":"rs_1","status":"completed","summary":[{"type":"summary_text","text":"summary"}],"encrypted_content":"encrypted"}`,
			true,
		},
		{
			"responses wrong unit",
			content.Origin{
				Provider: "openai",
				API:      "openai.responses",
				Model:    "source",
				Scope:    "scope",
			},
			"reasoning",
			llm.OpaqueUnitContentBlock,
			`{"type":"reasoning","id":"rs_1","summary":[]}`,
			false,
		},
		{
			"responses missing data",
			content.Origin{
				Provider: "openai",
				API:      "openai.responses",
				Model:    "source",
				Scope:    "scope",
			},
			"reasoning",
			llm.OpaqueUnitItem,
			`{"type":"reasoning","id":"rs_1"}`,
			false,
		},
		{
			"responses active",
			content.Origin{
				Provider: "openai",
				API:      "openai.responses",
				Model:    "source",
				Scope:    "scope",
			},
			"reasoning",
			llm.OpaqueUnitItem,
			`{"type":"reasoning","id":"rs_1","status":"in_progress","summary":[]}`,
			false,
		},
		{
			"responses null status",
			content.Origin{
				Provider: "openai",
				API:      "openai.responses",
				Model:    "source",
				Scope:    "scope",
			},
			"reasoning",
			llm.OpaqueUnitItem,
			`{"type":"reasoning","id":"rs_1","status":null,"summary":[]}`,
			false,
		},
		{
			"responses null summary",
			content.Origin{
				Provider: "openai",
				API:      "openai.responses",
				Model:    "source",
				Scope:    "scope",
			},
			"reasoning",
			llm.OpaqueUnitItem,
			`{"type":"reasoning","id":"rs_1","summary":null}`,
			false,
		},
		{
			"responses null summary text",
			content.Origin{
				Provider: "openai",
				API:      "openai.responses",
				Model:    "source",
				Scope:    "scope",
			},
			"reasoning",
			llm.OpaqueUnitItem,
			`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":null}]}`,
			false,
		},
		{
			"responses null encrypted",
			content.Origin{
				Provider: "openai",
				API:      "openai.responses",
				Model:    "source",
				Scope:    "scope",
			},
			"reasoning",
			llm.OpaqueUnitItem,
			`{"type":"reasoning","id":"rs_1","encrypted_content":null}`,
			false,
		},
		{
			"responses wrong summary type",
			content.Origin{
				Provider: "openai",
				API:      "openai.responses",
				Model:    "source",
				Scope:    "scope",
			},
			"reasoning",
			llm.OpaqueUnitItem,
			`{"type":"reasoning","id":"rs_1","summary":[{"type":"output_text","text":"summary"}]}`,
			false,
		},
		{
			"anthropic null thinking",
			content.Origin{
				Provider: "anthropic",
				API:      "anthropic.messages",
				Model:    "source",
				Scope:    "scope",
			},
			"thinking",
			llm.OpaqueUnitContentBlock,
			`{"type":"thinking","thinking":null,"signature":"signed"}`,
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := llm.Content{
				Kind:      llm.ContentReasoning,
				Reasoning: &llm.Reasoning{},
				Replay: &llm.Opaque{
					Version: 1,
					Origin:  tc.origin,
					Type:    tc.kind,
					Unit:    tc.unit,
					Raw:     json.RawMessage(tc.raw),
				},
			}
			input := llm.Input{
				Model: "target",
				ContentPolicy: llm.ContentPolicy{
					UnreplayableReasoning: llm.ReasoningOmit,
				},
				Messages: []llm.Message{
					llm.AssistantMessage(
						c,
						llm.Text("answer"),
					),
				},
			}
			_, _, err := Adapt(input, target)
			if (err == nil) != tc.valid {
				t.Fatalf(
					"source shape acceptance = %v, want valid %v",
					err,
					tc.valid,
				)
			}
		})
	}
}

func withRaw(c llm.Content, raw string) llm.Content {
	c = c.Clone()
	c.Replay.Raw = json.RawMessage(raw)
	return c
}

func withPartial(c llm.Content) llm.Content {
	c = c.Clone()
	c.Replay.Partial = true
	return c
}

func withVersion(c llm.Content, version uint16) llm.Content {
	c = c.Clone()
	c.Replay.Version = version
	return c
}
