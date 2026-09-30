package responses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

func summaryModel(endpoint string, support llm.Support) llm.ModelInfo {
	return llm.ModelInfo{
		Evidence:     llm.ModelEvidence{Model: "fixture", API: API, Endpoint: endpoint, Owner: "test", Source: "keyless summary fixture", Method: llm.EvidenceFixture, QualifiedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)},
		Capabilities: llm.Capabilities{Features: map[llm.Feature]llm.Support{llm.FeatureText: llm.SupportSupported, llm.FeatureMaxTokens: llm.SupportSupported, llm.FeatureStreaming: llm.SupportSupported, llm.FeatureReasoningEffort: llm.SupportSupported, llm.FeatureReasoningSummary: support}},
	}
}

type summaryTransport func(*http.Request) (*http.Response, error)

func (f summaryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSummaryNativeBodyAndFrozenDispatch(t *testing.T) {
	endpoint := "https://fixture.test/v1/responses"
	for _, stream := range []bool{false, true} {
		for _, effort := range []string{"", "low"} {
			for _, value := range []*bool{nil, new(false), new(true)} {
				t.Run(fmt.Sprintf("stream=%v/effort=%s/value=%v", stream, effort, value), func(t *testing.T) {
					var sent []byte
					transport := summaryTransport(func(r *http.Request) (*http.Response, error) {
						sent, _ = io.ReadAll(r.Body)
						raw := `{"object":"response","id":"resp","status":"completed","output":[{"type":"reasoning","encrypted_content":"PRIVATE","summary":[{"type":"summary_text","text":"public"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`
						kind := "application/json"
						if stream {
							raw = "data: " + `{"type":"response.completed","response":` + raw + "}\n\n"
							kind = "text/event-stream"
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
					})
					client, err := New(Config{Provider: "fixture", Endpoint: endpoint, Client: &http.Client{Transport: transport}, Models: map[string]llm.ModelInfo{"fixture": summaryModel(endpoint, llm.SupportSupported)}, CapabilityPolicy: llm.CapabilityStrict})
					if err != nil {
						t.Fatal(err)
					}
					input := llm.Input{Model: "fixture", Stream: stream, Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))}, Settings: llm.Settings{ReasoningSummary: value}}
					if effort != "" {
						input.Settings.Reasoning = &llm.ReasoningSettings{Effort: effort}
					}
					request, err := client.Prepare(t.Context(), input)
					if err != nil {
						t.Fatal(err)
					}
					if sent != nil {
						t.Fatal("Prepare dispatched")
					}
					var body map[string]json.RawMessage
					json.Unmarshal(request.Body(), &body)
					var reasoning map[string]any
					json.Unmarshal(body["reasoning"], &reasoning)
					wantSummary := value != nil && *value
					if (reasoning["summary"] == "auto") != wantSummary {
						t.Fatalf("reasoning=%v", reasoning)
					}
					if effort != "" && reasoning["effort"] != effort || effort == "" && reasoning["effort"] != nil {
						t.Fatalf("invented/lost effort: %v", reasoning)
					}
					if value != nil {
						*value = !*value
					}
					if input.Settings.Reasoning != nil {
						input.Settings.Reasoning.Effort = "high"
					}
					result, err := client.Send(t.Context(), llm.Call{Request: request})
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(sent, request.Body()) || result.Message.Text() != "answer" || result.Message.Content[0].Reasoning.Text != "public" {
						t.Fatalf("dispatch/result changed: %s %+v", sent, result)
					}
					snapshotRaw, _ := json.Marshal(request.Snapshot())
					var snapshot llm.RequestSnapshot
					json.Unmarshal(snapshotRaw, &snapshot)
					reopened, err := llm.NewRequest(snapshot)
					if err != nil || !bytes.Equal(reopened.Body(), sent) {
						t.Fatalf("reopen: %v", err)
					}
				})
			}
		}
	}
}

func TestSummaryUnsupportedAndUnknownRoutes(t *testing.T) {
	endpoint := "https://fixture.test/v1/responses"
	for _, support := range []llm.Support{llm.SupportUnsupported, llm.SupportUnknown} {
		for _, policy := range []llm.CapabilityPolicy{llm.CapabilityAdjust, llm.CapabilityStrict} {
			for _, value := range []bool{false, true} {
				client, err := New(Config{Provider: "fixture", Endpoint: endpoint, Models: map[string]llm.ModelInfo{"fixture": summaryModel(endpoint, support)}, CapabilityPolicy: policy})
				if err != nil {
					t.Fatal(err)
				}
				request, err := client.Prepare(t.Context(), llm.Input{Model: "fixture", Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))}, Settings: llm.Settings{ReasoningSummary: new(value)}})
				if value && policy == llm.CapabilityStrict {
					if err == nil {
						t.Fatal("strict opt-in accepted")
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				got := request.Effective().ReasoningSummary
				if value && support == llm.SupportUnsupported {
					if got != nil {
						t.Fatal("unsupported retained")
					}
				} else if got == nil || *got != value {
					t.Fatal("intent lost")
				}
				if slices.Contains(request.Snapshot().Unverified, llm.FeatureReasoningSummary) != (value && support == llm.SupportUnknown) {
					t.Fatal("unverified incorrect")
				}
				if !value && bytes.Contains(request.Body(), []byte(`"summary"`)) {
					t.Fatalf("off unknown sent summary: %s", request.Body())
				}
			}
		}
	}
	for _, endpoint := range []string{"https://api.x.ai/v1/responses", "https://api.meta.ai/v1/responses"} {
		client, err := New(Config{Provider: "compat", Endpoint: endpoint})
		if err != nil {
			t.Fatal(err)
		}
		request, err := client.Prepare(t.Context(), llm.Input{Model: "unknown", Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))}, Settings: llm.Settings{ReasoningSummary: new(true)}})
		if err != nil || request.Effective().ReasoningSummary != nil || bytes.Contains(request.Body(), []byte(`"summary"`)) {
			t.Fatalf("unqualified dialect accepted: %v", err)
		}
	}
}
