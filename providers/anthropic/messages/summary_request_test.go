package messages

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

type summaryTransport func(*http.Request) (*http.Response, error)

func (f summaryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSummaryRequestPrerequisiteAndDisplay(t *testing.T) {
	endpoint := "https://fixture.test/v1/messages"
	for _, support := range []llm.Support{llm.SupportSupported, llm.SupportUnknown, llm.SupportUnsupported} {
		for _, policy := range []llm.CapabilityPolicy{llm.CapabilityAdjust, llm.CapabilityStrict} {
			for _, value := range []*bool{nil, new(false), new(true)} {
				for _, reasoning := range []*llm.ReasoningSettings{nil, {Effort: "low"}, {BudgetTokens: 2048}} {
					on := value != nil && *value
					info := llm.ModelInfo{Evidence: llm.ModelEvidence{Model: "fixture", API: API, Endpoint: endpoint, Owner: "test", Source: "keyless summary fixture", Method: llm.EvidenceFixture, QualifiedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)}, Capabilities: llm.Capabilities{Features: map[llm.Feature]llm.Support{llm.FeatureText: llm.SupportSupported, llm.FeatureMaxTokens: llm.SupportSupported, llm.FeatureReasoningEffort: llm.SupportSupported, llm.FeatureReasoningBudget: llm.SupportSupported, llm.FeatureReasoningSummary: support}}}
					var sent []byte
					transport := summaryTransport(func(r *http.Request) (*http.Response, error) {
						sent, _ = io.ReadAll(r.Body)
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"type":"message","role":"assistant","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn"}`)), Request: r}, nil
					})
					client, err := New(Config{Provider: "fixture", Endpoint: endpoint, Models: map[string]llm.ModelInfo{"fixture": info}, CapabilityPolicy: policy, Client: &http.Client{Transport: transport}})
					if err != nil {
						t.Fatal(err)
					}
					input := llm.Input{Model: "fixture", Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))}, Settings: llm.Settings{ReasoningSummary: value, Reasoning: reasoning}}
					request, err := client.Prepare(t.Context(), input)
					if on && reasoning == nil {
						if err == nil || !strings.Contains(err.Error(), "requires configured reasoning") {
							t.Fatalf("missing prerequisite: %v", err)
						}
						continue
					}
					if on && support != llm.SupportSupported && policy == llm.CapabilityStrict {
						if err == nil {
							t.Fatal("strict unsupported/unknown accepted")
						}
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					var body map[string]json.RawMessage
					json.Unmarshal(request.Body(), &body)
					var thinking map[string]any
					json.Unmarshal(body["thinking"], &thinking)
					want := ""
					if reasoning != nil && on && support != llm.SupportUnsupported {
						want = "summarized"
					}
					if reasoning != nil && value != nil && !*value && support == llm.SupportSupported {
						want = "omitted"
					}
					got, _ := thinking["display"].(string)
					if got != want {
						t.Fatalf("support=%s policy=%s summary=%v reasoning=%v body=%s want display=%s", support, policy, value, reasoning, request.Body(), want)
					}
					if reasoning == nil && thinking != nil {
						t.Fatal("invented thinking")
					}
					result, err := client.Send(t.Context(), llm.Call{Request: request})
					if err != nil || !bytes.Equal(sent, request.Body()) || result.Stop.Kind != llm.StopFinished || result.Message.Text() != "answer" {
						t.Fatalf("absent summary/frozen dispatch: %+v %v", result, err)
					}
				}
			}
		}
	}
}
