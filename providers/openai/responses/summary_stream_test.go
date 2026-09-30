package responses

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestSummaryStreamsPreservePublicPrivateBoundary(t *testing.T) {
	for _, allowReasoningText := range []bool{false, true} {
		wire := codec{origin: content.Origin{Provider: "fixture", API: API, Model: "fixture", Scope: "fixture"}, publicReasoningText: allowReasoningText}
		events := []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"r","encrypted_content":"CIPHERTEXT","summary":[]}}`,
			`{"type":"response.reasoning_text.delta","output_index":0,"content_index":3,"delta":"unmodeled"}`,
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"first"}`,
			`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":1,"delta":"second"}`,
		}
		stream := ""
		for _, event := range events {
			stream += "data: " + event + "\n\n"
		}
		var deltas []llm.Delta
		partial, err := wire.readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{}, func(d llm.Delta) { deltas = append(deltas, d) })
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("expected incomplete stream failure, got %v", err)
		}
		if partial.Stop.Kind != llm.StopIncomplete {
			t.Fatal("missing terminal event marked finished")
		}
		want := 2
		if allowReasoningText {
			want = 3
		}
		if len(deltas) != want {
			t.Fatalf("allow=%v deltas=%+v", allowReasoningText, deltas)
		}
		for _, delta := range deltas {
			if delta.Kind != llm.DeltaReasoning || delta.Index != 0 || strings.Contains(delta.Text, "CIPHERTEXT") {
				t.Fatal("private payload/identity lost")
			}
		}
		if deltas[len(deltas)-1].ContentIndex != 1 || deltas[len(deltas)-2].ContentIndex != 0 {
			t.Fatalf("summary part identity lost: %+v", deltas)
		}
		if partial.Message.Text() != "" || len(partial.Message.Content) != 1 {
			t.Fatal("summary became answer")
		}
		got := partial.Message.Content[0].Reasoning.Text
		wantText := "first\n\nsecond"
		if allowReasoningText {
			wantText += "\n\nunmodeled"
		}
		if got != wantText {
			t.Fatalf("public partial = %q, want %q", got, wantText)
		}
		finalEvent := `{"type":"response.completed","response":{"object":"response","status":"completed","output":[{"type":"reasoning","id":"r","encrypted_content":"CIPHERTEXT","summary":[{"type":"summary_text","text":"authoritative"}]}]}}`
		final, err := wire.readStream(io.NopCloser(strings.NewReader(stream+"data: "+finalEvent+"\n\n")), llm.Result{}, nil)
		if err != nil || final.Stop.Kind != llm.StopFinished || final.Message.Content[0].Reasoning.Text != "authoritative" {
			t.Fatalf("final did not replace preview: %+v %v", final, err)
		}
	}
}

func TestUnmodeledReasoningNeverSynthesizesSummary(t *testing.T) {
	wire := codec{origin: content.Origin{Provider: "openai", API: API, Model: "fixture", Scope: "fixture"}}
	raw := "data: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","content":[{"type":"reasoning_text","text":"PRIVATE"}],"encrypted_content":"CIPHER"}}` + "\n\ndata: " + `{"type":"response.reasoning_text.delta","output_index":0,"delta":"PRIVATE"}` + "\n\n"
	var deltas []llm.Delta
	result, err := wire.readStream(io.NopCloser(strings.NewReader(raw)), llm.Result{}, func(d llm.Delta) { deltas = append(deltas, d) })
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(deltas) != 0 || len(result.Message.Content) != 1 || result.Message.Content[0].Kind != llm.ContentOpaque || result.Message.Text() != "" {
		t.Fatalf("unmodeled reasoning leaked: %+v %+v %v", result, deltas, err)
	}
}

func TestReasoningTextPublicOnlyOnDocumentedXAIModelRoute(t *testing.T) {
	for _, endpoint := range []string{"https://api.x.ai/v1/responses", "https://api.openai.com/v1/responses", "https://proxy.test/responses"} {
		for _, model := range []string{"grok-4.7", "other-model"} {
			transport := summaryTransport(func(r *http.Request) (*http.Response, error) {
				events := []string{
					`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","summary":[]}}`,
					`{"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"designated public text"}`,
					`{"type":"response.completed","response":{"object":"response","status":"completed","output":[{"type":"reasoning","summary":[]}]}}`,
				}
				stream := ""
				for _, event := range events {
					stream += "data: " + event + "\n\n"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream)), Request: r}, nil
			})
			client, err := New(Config{Provider: "arbitrary-identity", Endpoint: endpoint, Client: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			request, err := client.Prepare(t.Context(), llm.Input{Model: model, Stream: true, Messages: []llm.Message{llm.UserMessage(llm.Text("hi"))}})
			if err != nil {
				t.Fatal(err)
			}
			var deltas []llm.Delta
			_, err = client.Send(t.Context(), llm.Call{Request: request, Sink: func(d llm.Delta) { deltas = append(deltas, d) }})
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if endpoint == "https://api.x.ai/v1/responses" && model == "grok-4.7" {
				want = 1
			}
			if len(deltas) != want {
				t.Fatalf("endpoint=%s model=%s deltas=%+v", endpoint, model, deltas)
			}
		}
	}
}

func TestSummaryLabelCannotPublishOpaqueItem(t *testing.T) {
	for _, item := range []string{
		`{"type":"reasoning","content":[{"type":"reasoning_text","text":"PRIVATE"}],"encrypted_content":"CIPHER","summary":[]}`,
		`{"type":"future_reasoning","summary":[]}`,
		`{"type":"message","role":"assistant","content":[],"summary":[]}`,
	} {
		for _, allowReasoningText := range []bool{false, true} {
			for _, terminal := range []bool{false, true} {
				wire := codec{origin: content.Origin{Provider: "fixture", API: API, Model: "fixture", Scope: "fixture"}, publicReasoningText: allowReasoningText}
				stream := "data: " + `{"type":"response.output_item.added","output_index":0,"item":` + item + "}\n\n" + "data: " + `{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"UNMODELED_PRIVATE"}` + "\n\n" + "data: " + `{"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"UNMODELED_PRIVATE"}` + "\n\n"
				if terminal {
					stream += "data: " + `{"type":"response.completed","response":{"object":"response","status":"completed","output":[` + item + "]}}\n\n"
				}
				var deltas []llm.Delta
				result, err := wire.readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{}, func(d llm.Delta) { deltas = append(deltas, d) })
				if terminal && err != nil || !terminal && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("terminal=%v error=%v", terminal, err)
				}
				if len(deltas) != 0 || result.Message.Text() != "" {
					t.Fatalf("opaque preview leaked: %+v %+v", deltas, result)
				}
				for _, part := range result.Message.Content {
					if part.Reasoning != nil && strings.Contains(part.Reasoning.Text, "UNMODELED_PRIVATE") {
						t.Fatal("synthesized opaque summary")
					}
				}
			}
		}
	}
}

func TestUnmodeledStreamPartCannotPublishSummary(t *testing.T) {
	for _, partEvent := range []string{
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"reasoning_text","text":"PRIVATE"}}`,
		`{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"future_summary","text":"PRIVATE"}}`,
	} {
		wire := codec{origin: content.Origin{Provider: "fixture", API: API, Model: "fixture", Scope: "fixture"}}
		stream := "data: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","summary":[]}}` + "\n\n" + "data: " + partEvent + "\n\n" + "data: " + `{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"UNMODELED_PRIVATE"}` + "\n\n"
		var deltas []llm.Delta
		result, err := wire.readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{}, func(d llm.Delta) { deltas = append(deltas, d) })
		if !errors.Is(err, io.ErrUnexpectedEOF) || len(deltas) != 0 || len(result.Message.Content) != 1 || result.Message.Content[0].Kind != llm.ContentOpaque {
			t.Fatalf("unmodeled part leaked: %+v %+v %v", deltas, result, err)
		}
	}
}

func TestCompletedSummaryPartReplacesPreview(t *testing.T) {
	wire := codec{origin: content.Origin{Provider: "fixture", API: API, Model: "fixture", Scope: "fixture"}}
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","summary":[]}}`,
		`{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"preview"}`,
		`{"type":"response.reasoning_summary_part.done","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"authoritative part"}}`,
	}
	stream := ""
	for _, event := range events {
		stream += "data: " + event + "\n\n"
	}
	var deltas []llm.Delta
	result, err := wire.readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{}, func(d llm.Delta) { deltas = append(deltas, d) })
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(deltas) != 1 || deltas[0].Text != "preview" || result.Message.Content[0].Reasoning.Text != "authoritative part" {
		t.Fatalf("part replacement: %+v %+v %v", result, deltas, err)
	}
}

func TestLaterPublicPartCannotRelabelOpaqueParent(t *testing.T) {
	for _, parent := range []string{
		`{"type":"reasoning","summary":[{"type":"future_summary","text":"ORIGINAL_PRIVATE"}]}`,
		`{"type":"reasoning","content":[{"type":"reasoning_text","text":"ORIGINAL_PRIVATE"}],"summary":[]}`,
	} {
		for _, terminal := range []bool{false, true} {
			wire := codec{origin: content.Origin{Provider: "fixture", API: API, Model: "fixture", Scope: "fixture"}}
			stream := "data: " + `{"type":"response.output_item.added","output_index":0,"item":` + parent + "}\n\n" + "data: " + `{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}` + "\n\n" + "data: " + `{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"UNMODELED_PRIVATE"}` + "\n\n" + "data: " + `{"type":"response.reasoning_summary_part.done","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"PUBLIC_LABEL"}}` + "\n\n"
			if terminal {
				stream += "data: " + `{"type":"response.completed","response":{"object":"response","status":"completed","output":[` + parent + "]}}\n\n"
			}
			var deltas []llm.Delta
			result, err := wire.readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{}, func(d llm.Delta) { deltas = append(deltas, d) })
			if terminal && err != nil || !terminal && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("terminal=%v err=%v", terminal, err)
			}
			if len(deltas) != 0 || len(result.Message.Content) != 1 || result.Message.Content[0].Kind != llm.ContentOpaque || !strings.Contains(string(result.Message.Content[0].Opaque.Raw), "ORIGINAL_PRIVATE") {
				t.Fatalf("opaque parent changed: %+v %+v", deltas, result.Message.Content)
			}
		}
	}
}

func TestAnswerLabelCannotPublishPrivateReasoning(t *testing.T) {
	wire := codec{origin: content.Origin{Provider: "fixture", API: API, Model: "fixture", Scope: "fixture"}}
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","content":[{"type":"reasoning_text","text":"ORIGINAL_PRIVATE"}],"summary":[]}}`,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"UNMODELED_PRIVATE"}`,
	}
	stream := ""
	for _, event := range events {
		stream += "data: " + event + "\n\n"
	}
	var deltas []llm.Delta
	result, err := wire.readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{}, func(d llm.Delta) { deltas = append(deltas, d) })
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(deltas) != 0 || len(result.Message.Content) != 1 || result.Message.Content[0].Kind != llm.ContentOpaque || !strings.Contains(string(result.Message.Content[0].Opaque.Raw), "ORIGINAL_PRIVATE") {
		t.Fatalf("private reasoning published/lost: %+v %+v %v", deltas, result.Message.Content, err)
	}
}
