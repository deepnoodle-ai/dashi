package llm

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
)

func TestInputContentPolicyRequiresKnownChoice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy ReasoningPolicy
		valid  bool
	}{
		{"default", ReasoningReject, true},
		{"omit", ReasoningOmit, true},
		{"unknown", "rewrite", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := Input{Model: "m", ContentPolicy: ContentPolicy{UnreplayableReasoning: tc.policy}}
			if err := input.Validate(); (err == nil) != tc.valid {
				t.Fatalf("policy validation = %v, want valid %v", err, tc.valid)
			}
		})
	}
	data, err := json.Marshal(Input{Model: "m"})
	if err != nil || json.Valid(data) == false || containsPolicy(data) {
		t.Fatalf("zero policy should be omitted: %s, %v", data, err)
	}
}

func containsPolicy(data []byte) bool {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	_, present := fields["content_policy"]
	return present
}

func TestNewRequestRequiresPolicyForReasoningOmission(t *testing.T) {
	origin := content.Origin{Provider: "p", API: "anthropic.messages", Model: "target", Scope: "scope"}
	input := Input{Model: "target", Messages: []Message{AssistantMessage(Content{Kind: ContentReasoning, Reasoning: &Reasoning{Text: "thinking"}})}}
	change := ContentChange{MessageIndex: 0, ContentIndex: 0, Action: ContentChangeOmitReasoning, Reason: "unreplayable"}
	for _, tc := range []struct {
		name  string
		alter func(*RequestSnapshot)
		valid bool
	}{
		{"opted in", func(p *RequestSnapshot) {
			p.Input.ContentPolicy.UnreplayableReasoning = ReasoningOmit
		}, true},
		{"default", func(*RequestSnapshot) {
		}, false},
		{"wrong kind", func(p *RequestSnapshot) {
			p.Input.ContentPolicy.UnreplayableReasoning = ReasoningOmit
			p.Input.Messages[0].Content[0] = Text("hello")
		}, false},
		{"duplicate", func(p *RequestSnapshot) {
			p.Input.ContentPolicy.UnreplayableReasoning = ReasoningOmit
			p.ContentChanges = append(p.ContentChanges, change)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := RequestSnapshot{Origin: origin, Input: input, Body: []byte(`{}`), ContentChanges: []ContentChange{change}}
			p.Input.Messages = []Message{input.Messages[0].Clone()}
			tc.alter(&p)
			if _, err := NewRequest(p); (err == nil) != tc.valid {
				t.Fatalf("omission record validation = %v, want valid %v", err, tc.valid)
			}
		})
	}
}
