package completions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
)

func TestPrepareKeepsSupportedPlainReasoningWithOmissionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		policy   llm.ReasoningPolicy
		reject   bool
		omit     bool
	}{
		{name: "OpenRouter default", provider: "openrouter"},
		{
			name:     "OpenRouter opt-in",
			provider: "openrouter",
			policy:   llm.ReasoningOmit,
		},
		{name: "Mistral default", provider: "mistral"},
		{name: "Mistral opt-in", provider: "mistral", policy: llm.ReasoningOmit},
		{name: "unsupported default", provider: "openai", reject: true},
		{
			name:     "unsupported opt-in",
			provider: "openai",
			policy:   llm.ReasoningOmit,
			omit:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := New(Config{
				Provider:         tc.provider,
				Endpoint:         "https://unused.invalid/api",
				APIKey:           "test",
				CapabilityPolicy: llm.CapabilityAdjust,
			})
			if err != nil {
				t.Fatal(err)
			}
			input := testInput(false)
			input.ContentPolicy.UnreplayableReasoning = tc.policy
			input.Messages = append(
				input.Messages,
				llm.AssistantMessage(
					llm.Content{
						Kind:      llm.ContentReasoning,
						Reasoning: &llm.Reasoning{Text: "private thought"},
					},
					llm.Text("answer"),
				),
			)
			request, err := client.Prepare(t.Context(), input)
			if (err != nil) != tc.reject {
				t.Fatalf("reasoning support changed: %v", err)
			}
			if tc.reject {
				return
			}
			changes := request.ContentChanges()
			if tc.omit {
				if len(changes) != 1 ||
					changes[0].Action != llm.ContentChangeOmitReasoning {
					t.Fatalf(
						"unsupported reasoning omitted without a decision: %+v",
						changes,
					)
				}
			} else if len(changes) != 0 {
				t.Fatalf("compatible reasoning changed: %+v", changes)
			}
			if strings.Contains(
				string(request.Body()),
				"private thought",
			) == tc.omit || !strings.Contains(
				string(request.Body()),
				"answer",
			) {
				t.Fatal("encoded history disagreed with the supported reasoning policy")
			}
		})
	}
}

func TestPrepareOmitsKnownPrivateReasoningForCompletions(t *testing.T) {
	client := testClient(t, "https://unused.invalid/api")
	input := testInput(false)
	input.ContentPolicy.UnreplayableReasoning = llm.ReasoningOmit
	input.Messages = append(
		input.Messages,
		llm.AssistantMessage(
			llm.Content{
				Kind:      llm.ContentReasoning,
				Reasoning: &llm.Reasoning{},
				Replay: &llm.Opaque{
					Version: 1,
					Origin: content.Origin{
						Provider: "anthropic",
						API:      "anthropic.messages",
						Model:    "source",
						Scope:    "source-scope",
					},
					Unit: llm.OpaqueUnitContentBlock,
					Type: "redacted_thinking",
					Raw:  json.RawMessage(`{"type":"redacted_thinking","data":"private"}`),
				},
			},
			llm.Text("answer"),
		),
	)
	request, err := client.Prepare(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.ContentChanges()) != 1 ||
		request.ContentChanges()[0].Action != llm.ContentChangeOmitReasoning ||
		strings.Contains(
			string(request.Body()),
			"private",
		) {
		t.Fatal("private reasoning was not explicitly omitted for Chat Completions")
	}
}

func TestPrepareKeepsDecodedMistralThinking(t *testing.T) {
	client, err := New(Config{
		Provider:         "mistral",
		Endpoint:         "https://unused.invalid/api",
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	wire := codec{origin: client.client.Origin("requested"), provider: "mistral"}
	result, err := wire.decode(
		[]byte(`{"id":"r","model":"requested","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"private thought"}]},{"type":"text","text":"answer"}]},"finish_reason":"stop"}]}`),
		llm.ModelRef{
			Provider: "mistral",
			Model:    "requested",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	input := testInput(false)
	input.ContentPolicy.UnreplayableReasoning = llm.ReasoningOmit
	input.Messages = append(input.Messages, result.Message)
	request, err := client.Prepare(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.ContentChanges()) != 0 || !strings.Contains(
		string(request.Body()),
		`"type":"thinking"`,
	) || !strings.Contains(
		string(request.Body()),
		"private thought",
	) {
		t.Fatal("compatible Mistral reasoning was omitted or changed")
	}
}
