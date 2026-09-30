package completions

import (
	"bytes"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestCompletionsCannotRetainSummaryOptIn(t *testing.T) {
	endpoint := "https://fixture.test/completions"
	info := llm.ModelInfo{Evidence: llm.ModelEvidence{Model: "fixture", API: API, Endpoint: endpoint, Owner: "test", Source: "keyless fixture", Method: llm.EvidenceFixture, QualifiedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)}, Capabilities: llm.Capabilities{Features: map[llm.Feature]llm.Support{llm.FeatureText: llm.SupportSupported, llm.FeatureMaxTokens: llm.SupportSupported, llm.FeatureReasoningSummary: llm.SupportSupported}}}
	for _, policy := range []llm.CapabilityPolicy{llm.CapabilityAdjust, llm.CapabilityStrict} {
		for _, value := range []bool{false, true} {
			client, err := New(Config{Provider: "fixture", Endpoint: endpoint, Models: map[string]llm.ModelInfo{"fixture": info}, CapabilityPolicy: policy})
			if err != nil {
				t.Fatal(err)
			}
			request, err := client.Prepare(t.Context(), llm.Input{Model: "fixture", Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))}, Settings: llm.Settings{ReasoningSummary: new(value)}})
			if value && policy == llm.CapabilityStrict {
				if err == nil {
					t.Fatal("unimplemented mapping accepted")
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(request.Body(), []byte(`"summary"`)) {
				t.Fatalf("invalid wire control: %s", request.Body())
			}
			effective := request.Effective().ReasoningSummary
			if value && effective != nil || !value && (effective == nil || *effective) {
				t.Fatal("incorrect effective preference")
			}
		}
	}
}
