package llm

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/tool"
)

func TestModelInfoValidateRejectsNegativeContextWindow(t *testing.T) {
	info := ModelInfo{
		Capabilities: Capabilities{ContextWindow: -1},
		Evidence: ModelEvidence{
			Model:       "m",
			API:         "api",
			Endpoint:    "https://example.test/v1",
			Owner:       "test",
			Source:      "fixture",
			Method:      EvidenceFixture,
			QualifiedAt: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC),
		},
	}
	if err := info.Validate(); err == nil {
		t.Fatal("negative context window was accepted")
	}
}

func TestModelInfoDecodesRecordWithoutContextWindow(t *testing.T) {
	// A record written before context_window existed must still decode.
	raw := `{"capabilities":{"max_output_tokens":8192},` +
		`"evidence":{"model":"m","api":"api",` +
		`"endpoint":"https://example.test/v1","owner":"test",` +
		`"source":"fixture","method":"fixture",` +
		`"qualified_at":"2026-09-27T00:00:00Z"}}`
	var info ModelInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatalf("decode older record: %v", err)
	}
	if info.Capabilities.ContextWindow != 0 {
		t.Fatalf("context window = %d, want unknown", info.Capabilities.ContextWindow)
	}
	if info.Capabilities.MaxOutputTokens != 8192 {
		t.Fatal("existing output limit was lost")
	}
	if err := info.Validate(); err != nil {
		t.Fatalf("older record no longer validates: %v", err)
	}
}

func TestCapabilitiesCheckIncludesToolResultMedia(t *testing.T) {
	r := Input{Model: "m", Messages: []Message{UserMessage(Content{
		Kind: ContentToolResult,
		ToolResult: &ToolResult{CallID: "call", Output: tool.Output{Content: []content.Block{
			content.ImageBlock(content.Source{URL: "https://example.com/image.png"}),
		}}},
	})}}
	c := Capabilities{Features: map[Feature]Support{
		FeatureText: SupportSupported, FeatureTools: SupportSupported,
	}}
	if _, err := c.Check(r, CapabilityStrict); err == nil {
		t.Fatal("tool output image bypassed capability validation")
	}
	unverified, err := c.Check(r, CapabilityAdjust)
	if err != nil || !slices.Equal(unverified, []Feature{FeatureImage}) {
		t.Fatal("tool output media was not reported as unverified", err)
	}
	c.Features[FeatureImage] = SupportUnsupported
	if _, err := c.Check(r, CapabilityAdjust); err == nil {
		t.Fatal("passthrough permitted known unsupported media")
	}
}

func TestCapabilitiesCheckScopesHostedToolType(t *testing.T) {
	input := Input{
		Model: "m",
		Tools: []tool.Definition{{
			Name: "web_search",
			Kind: tool.KindProviderHosted,
			Hosted: &tool.HostedSpec{
				Provider: "anthropic",
				API:      "anthropic.messages",
				Type:     "web_search_20250305",
			},
		}},
	}
	capabilities := Capabilities{
		Features: map[Feature]Support{
			FeatureText:  SupportSupported,
			FeatureTools: SupportSupported,
		},
	}
	if _, err := capabilities.Check(input, CapabilityStrict); err == nil {
		t.Fatal("unknown hosted tool support was accepted")
	}
	feature := HostedToolFeature("web_search_20250305")
	unverified, err := capabilities.Check(input, CapabilityAdjust)
	if err != nil || !slices.Equal(unverified, []Feature{feature}) {
		t.Fatalf("hosted type evidence was not reported: %v, %v", unverified, err)
	}
	capabilities.Features[feature] = SupportUnsupported
	if _, err := capabilities.Check(input, CapabilityAdjust); err == nil {
		t.Fatal("known unsupported hosted tool was accepted")
	}
}

func TestCapabilitiesCheckReasoningConstraints(t *testing.T) {
	c := Capabilities{
		Features: map[Feature]Support{FeatureText: SupportSupported,
			FeatureReasoningEffort: SupportSupported, FeatureReasoningBudget: SupportSupported},
		ReasoningEfforts: []string{"low"}, MinReasoningBudget: 1024, MaxReasoningBudget: 4096,
	}
	for _, tt := range []struct {
		name     string
		settings ReasoningSettings
		accepted bool
	}{
		{"supported effort", ReasoningSettings{Effort: "low"}, true},
		{"unsupported effort", ReasoningSettings{Effort: "high"}, false},
		{"budget below minimum", ReasoningSettings{BudgetTokens: 1000}, false},
		{"budget above maximum", ReasoningSettings{BudgetTokens: 4097}, false},
		{"budget at minimum", ReasoningSettings{BudgetTokens: 1024}, true},
		{"budget at maximum", ReasoningSettings{BudgetTokens: 4096}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := Input{Model: "m", Settings: Settings{Reasoning: &tt.settings}}
			_, err := c.Check(r, CapabilityAdjust)
			if (err == nil) != tt.accepted {
				t.Fatal("model constraints were not enforced", err)
			}
		})
	}
}

func TestRequestRecordPreservesExactBodyAndEffectiveSettings(t *testing.T) {
	effectiveTemp := 0.3
	r := Input{Model: "m"}
	body := []byte("{\n  \"private\": 9007199254740993, \"stream\": false\n}")
	p, err := NewRequest(RequestSnapshot{
		Origin: content.Origin{Provider: "p", API: "api", Model: "m", Scope: "scope"},
		Input:  r, Effective: Settings{Temperature: &effectiveTemp}, Body: body,
		Adjustments: []Adjustment{{Field: SettingTemperature, Action: AdjustmentDefault,
			Requested: json.RawMessage("null"), Effective: json.RawMessage("0.3"), Reason: "default"}},
	})
	if err != nil {
		t.Fatalf("create preparation evidence: %v", err)
	}
	effectiveTemp = 9
	copy := p.Effective()
	*copy.Temperature = 10
	if p.Input().Settings.Temperature != nil || *p.Effective().Temperature != 0.3 {
		t.Fatal("requested and effective settings were shared")
	}
	raw, err := json.Marshal(p.Snapshot())
	if err != nil {
		t.Fatalf("encode preparation evidence: %v", err)
	}
	var record RequestSnapshot
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decode preparation evidence: %v", err)
	}
	if string(record.Body) != string(body) {
		t.Fatal("recording normalized the exact request body")
	}
}
