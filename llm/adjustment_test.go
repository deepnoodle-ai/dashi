package llm

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
)

func TestRequestRejectsUnaccountedSettingChanges(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*RequestSnapshot)
	}{
		{name: "missing decision", edit: func(p *RequestSnapshot) {
			p.Adjustments = nil
		}},
		{name: "wrong effective value", edit: func(p *RequestSnapshot) {
			p.Adjustments[0].Effective = json.RawMessage("2048")
		}},
		{name: "wrong requested value", edit: func(p *RequestSnapshot) {
			p.Adjustments[0].Requested = json.RawMessage("0")
		}},
		{name: "unknown field", edit: func(p *RequestSnapshot) {
			p.Adjustments[0].Field = "invented"
		}},
		{name: "clamp without requested value", edit: func(p *RequestSnapshot) {
			p.Adjustments[0].Action = AdjustmentClamp
		}},
		{name: "duplicate decision", edit: func(p *RequestSnapshot) {
			p.Adjustments = append(p.Adjustments, p.Adjustments[0])
		}},
		{name: "replaced explicit value", edit: func(p *RequestSnapshot) {
			p.Input.Settings.MaxTokens = 2048
		}},
		{name: "omitted explicit value", edit: func(p *RequestSnapshot) {
			p.Input.Settings.MaxTokens, p.Effective.MaxTokens = 2048, 0
			p.Adjustments = nil
		}},
		{name: "unverified under strict policy", edit: func(p *RequestSnapshot) {
			p.CapabilityPolicy = CapabilityStrict
			p.Unverified = []Feature{FeatureText}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := RequestSnapshot{
				Origin: content.Origin{Provider: "test", API: "api", Model: "m", Scope: "scope"},
				Input:  Input{Model: "m"}, Effective: Settings{MaxTokens: 1024}, Body: []byte(`{}`),
				Adjustments: []Adjustment{{Field: SettingMaxTokens, Action: AdjustmentDefault,
					Requested: json.RawMessage("null"), Effective: json.RawMessage("1024"), Reason: "default"}},
			}
			if _, err := NewRequest(p); err != nil {
				t.Fatalf("valid default rejected: %v", err)
			}
			test.edit(&p)
			if _, err := NewRequest(p); err == nil {
				t.Fatal("invalid decision record accepted")
			}
		})
	}
}
