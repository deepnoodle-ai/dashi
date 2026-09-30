package gemini

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

func TestSummaryControlIndependentFromBudget(t *testing.T) {
	endpoint := "https://fixture.test/v1beta/models/{model}:generateContent"
	for _, support := range []llm.Support{llm.SupportSupported, llm.SupportUnknown, llm.SupportUnsupported} {
		for _, policy := range []llm.CapabilityPolicy{llm.CapabilityAdjust, llm.CapabilityStrict} {
			for _, value := range []*bool{nil, new(false), new(true)} {
				for _, budget := range []int{0, 2048} {
					for _, stream := range []bool{false, true} {
						on := value != nil && *value
						info := llm.ModelInfo{Evidence: llm.ModelEvidence{Model: "fixture", API: API, Endpoint: strings.ReplaceAll(endpoint, "{model}", "fixture"), Owner: "test", Source: "keyless summary fixture", Method: llm.EvidenceFixture, QualifiedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)}, Capabilities: llm.Capabilities{Features: map[llm.Feature]llm.Support{llm.FeatureText: llm.SupportSupported, llm.FeatureMaxTokens: llm.SupportSupported, llm.FeatureStreaming: llm.SupportSupported, llm.FeatureReasoningBudget: llm.SupportSupported, llm.FeatureReasoningSummary: support}}}
						var sent []byte
						transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
							sent, _ = io.ReadAll(r.Body)
							raw := `{"modelVersion":"fixture","candidates":[{"content":{"role":"model","parts":[{"text":"public","thought":true,"thoughtSignature":"UFJJVkFURQ=="},{"text":"answer"}]},"finishReason":"STOP"}]}`
							kind := "application/json"
							if stream {
								raw = "data: " + raw + "\n\n"
								kind = "text/event-stream"
							}
							return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
						})
						client, err := New(Config{Provider: "fixture", Endpoint: endpoint, Models: map[string]llm.ModelInfo{"fixture": info}, CapabilityPolicy: policy, Client: &http.Client{Transport: transport}})
						if err != nil {
							t.Fatal(err)
						}
						input := llm.Input{Model: "fixture", Stream: stream, Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))}, Settings: llm.Settings{ReasoningSummary: value}}
						if budget > 0 {
							input.Settings.Reasoning = &llm.ReasoningSettings{BudgetTokens: budget}
						}
						request, err := client.Prepare(t.Context(), input)
						if on && support != llm.SupportSupported && policy == llm.CapabilityStrict {
							if err == nil {
								t.Fatal("strict unsupported/unknown accepted")
							}
							continue
						}
						if err != nil {
							t.Fatal(err)
						}
						var body struct {
							GenerationConfig struct{ ThinkingConfig map[string]any }
						}
						json.Unmarshal(request.Body(), &body)
						thinking := body.GenerationConfig.ThinkingConfig
						got, present := thinking["includeThoughts"]
						wantPresent := value != nil && (support == llm.SupportSupported || on && support == llm.SupportUnknown)
						if present != wantPresent || present && got != *value {
							t.Fatalf("support=%s policy=%s value=%v body=%s", support, policy, value, request.Body())
						}
						if budget > 0 && thinking["thinkingBudget"] != float64(budget) || budget == 0 && thinking["thinkingBudget"] != nil {
							t.Fatalf("invented/lost budget: %s", request.Body())
						}
						if sent != nil {
							t.Fatal("Prepare dispatched")
						}
						var previews []llm.Delta
						result, err := client.Send(t.Context(), llm.Call{Request: request, Sink: func(d llm.Delta) { previews = append(previews, d) }})
						if err != nil || !bytes.Equal(sent, request.Body()) || result.Stop.Kind != llm.StopFinished || result.Message.Text() != "answer" {
							t.Fatalf("frozen dispatch: %+v %v", result, err)
						}
						if result.Message.Content[0].Reasoning.Text != "public" || strings.Contains(result.Message.Content[0].Reasoning.Text, "PRIVATE") {
							t.Fatal("public/private split lost")
						}
						for _, delta := range previews {
							if strings.Contains(delta.Text, "PRIVATE") {
								t.Fatal("signature leaked")
							}
						}
					}
				}
			}
		}
	}
}
