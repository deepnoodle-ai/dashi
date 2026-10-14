package completions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

const refusalText = "I can't help with that."

func refusalOf(t *testing.T, result llm.Result) string {
	t.Helper()
	for _, c := range result.Message.Content {
		if c.Kind == llm.ContentRefusal {
			return *c.Refusal
		}
	}
	t.Fatalf("no refusal in %+v", result.Message.Content)
	return ""
}

func TestRefusalIsDecodedInBothTransports(t *testing.T) {
	complete := `{"id":"chat-1","model":"served","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"` +
		refusalText + `"},"finish_reason":"stop"}]}`
	streamed := dialectEvents(
		`{"id":"chat-1","model":"served","choices":[{"index":0,"delta":{"role":"assistant","content":null,"refusal":""}}]}`,
		`{"choices":[{"index":0,"delta":{"refusal":"I can't "}}]}`,
		`{"choices":[{"index":0,"delta":{"refusal":"help with that."}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	for _, tc := range []struct {
		name     string
		response string
		stream   bool
	}{
		{"complete", complete, false},
		{"stream", streamed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var live strings.Builder
			result, err := dialectSend(t, tc.response, tc.stream, func(d llm.Delta) {
				if d.Kind == llm.DeltaText {
					live.WriteString(d.Text)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := refusalOf(t, result); got != refusalText {
				t.Fatalf("refusal = %q", got)
			}
			if result.Stop.Kind != llm.StopRefusal || result.Stop.Raw != "stop" {
				t.Fatalf("stop = %+v", result.Stop)
			}
			if result.Message.Source == nil {
				t.Fatal("refusal message has no source")
			}
			if tc.stream && live.String() != refusalText {
				t.Fatalf("live text = %q", live.String())
			}
		})
	}
}

func TestRefusalWithOutputLimitKeepsLimitStop(t *testing.T) {
	result, err := dialectSend(
		t,
		`{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","refusal":"I can't"},"finish_reason":"length"}]}`,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stop.Kind != llm.StopOutputLimit {
		t.Fatalf("stop = %+v", result.Stop)
	}
}

func TestRefusalReplaysAsAssistantMessage(t *testing.T) {
	history := []llm.Message{
		llm.UserMessage(llm.Text("hello")),
		{Role: llm.RoleAssistant, Content: []llm.Content{refusal(refusalText)}},
		llm.UserMessage(llm.Text("please")),
	}
	for _, tc := range []struct {
		provider string
		want     map[string]any
	}{
		{"openai", map[string]any{"role": "assistant", "content": "", "refusal": refusalText}},
		{"mistral", map[string]any{"role": "assistant", "content": refusalText}},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			client, err := New(Config{
				Provider: tc.provider,
				Endpoint: "https://example.test/v1/chat/completions",
				APIKey:   "test",
			})
			if err != nil {
				t.Fatal(err)
			}
			request, err := client.Prepare(t.Context(), llm.Input{Model: "m", Messages: history})
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Messages []map[string]any `json:"messages"`
			}
			if err := json.Unmarshal(request.Body(), &body); err != nil {
				t.Fatal(err)
			}
			if got := completionsJSON(t, body.Messages[1]); got != completionsJSON(t, tc.want) {
				t.Fatalf("assistant message = %s", got)
			}
		})
	}
}

func TestStreamedReasoningDetailsMatchCompleteResponse(t *testing.T) {
	complete := `{"id":"r","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"answer","reasoning":"think","reasoning_details":[{"type":"reasoning.text","text":"think","index":0,"signature":"sig"},{"type":"reasoning.encrypted","data":"abc","index":1}]},"finish_reason":"stop"}]}`
	streamed := dialectEvents(
		`{"id":"r","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning":"thi","reasoning_details":[{"type":"reasoning.text","text":"thi","index":0}]}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning":"nk","reasoning_details":[{"type":"reasoning.text","text":"nk","index":0,"signature":null}]}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.text","text":"","index":0,"signature":"sig"},{"type":"reasoning.encrypted","data":"ab","index":1}]}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.encrypted","data":"c","index":1}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"answer","reasoning_details":null}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	want, err := dialectSend(t, complete, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := dialectSend(t, streamed, true)
	if err != nil {
		t.Fatalf("stream with reasoning details failed: %v", err)
	}
	if len(got.Message.Content) != 3 || len(want.Message.Content) != 3 {
		t.Fatalf("content: stream %+v complete %+v", got.Message.Content, want.Message.Content)
	}
	for i := range want.Message.Content {
		w, g := want.Message.Content[i], got.Message.Content[i]
		if w.Kind != g.Kind {
			t.Fatalf("content %d kind: stream %q complete %q", i, g.Kind, w.Kind)
		}
		if w.Opaque != nil && string(w.Opaque.Raw) != string(g.Opaque.Raw) {
			t.Fatalf("details: stream %s complete %s", g.Opaque.Raw, w.Opaque.Raw)
		}
	}
	// Fragments sharing an index replay as one merged item; separate
	// indexes stay separate.
	details := got.Message.Content[1]
	encoded, err := codec{origin: details.Opaque.Origin, provider: "openai"}.encodeMessage(
		llm.Message{Role: llm.RoleAssistant, Content: []llm.Content{details}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if replay := completionsJSON(t, encoded[0].(map[string]any)["reasoning_details"]); replay !=
		`[{"type":"reasoning.text","text":"think","index":0,"signature":"sig"},{"type":"reasoning.encrypted","data":"abc","index":1}]` {
		t.Fatalf("replayed details = %s", replay)
	}
}

func TestStreamedReasoningDetailsMustBeAnArray(t *testing.T) {
	_, err := dialectSend(t, dialectEvents(
		`{"choices":[{"index":0,"delta":{"reasoning_details":{"type":"reasoning.text"}}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	), true)
	if !strings.Contains(err.Error(), "reasoning details") {
		t.Fatalf("err = %v", err)
	}
}
