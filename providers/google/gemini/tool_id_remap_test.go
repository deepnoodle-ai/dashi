package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

func TestPrepareRemapsToolIDAndSendsFrozenBody(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[stream], func(t *testing.T) {
			var sent []byte
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					var err error
					sent, err = io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read body: %v", err)
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}\n\n")
					} else {
						io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
					}
				},
			))
			defer server.Close()
			client := newTestClient(t, server)
			input := geminiToolHistory("call / 東京")
			input.Stream = stream
			original, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			request, err := client.Prepare(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if sent != nil {
				t.Fatal("Prepare dispatched a request")
			}
			changes := request.ContentChanges()
			if len(changes) != 2 {
				t.Fatalf("remap decisions = %+v", changes)
			}
			mapped := changes[0].ToID
			for i, change := range changes {
				if change.Action != llm.ContentChangeRemapToolID ||
					change.MessageIndex != i+1 || change.ContentIndex != 0 ||
					change.FromID != "call / 東京" || change.ToID != mapped {
					t.Fatalf("decision %d = %+v", i, change)
				}
			}
			if mapped == "call / 東京" || mapped == "" || strings.Contains(mapped, "/") {
				t.Fatalf("ID was not made portable: %q", mapped)
			}
			callID, resultID := geminiWireToolIDs(t, request.Body())
			if callID != mapped || resultID != mapped {
				t.Fatalf("wire IDs = %q, %q; mapped = %q", callID, resultID, mapped)
			}
			stored, err := json.Marshal(request.Input())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(stored, original) {
				t.Fatal("preparation changed original tool identity")
			}
			input.Messages[1].Content[0].ToolCall.ID = "mutated"
			if _, err := client.Send(context.Background(), request, nil); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(sent, request.Body()) {
				t.Fatal("Send changed the prepared body")
			}
		})
	}
}

func TestPrepareOverlaysUnsignedGeminiToolReplayID(t *testing.T) {
	client, err := New(Config{
		Provider:            "google",
		APIKey:              "test",
		UnknownCapabilities: llm.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := geminiToolHistory("source / id")
	raw := json.RawMessage(`{"functionCall":{"id":"source / id","name":"search","args":{"q":"yes"}}}`)
	input.Messages[1].Content[0].Replay = &llm.Opaque{
		Version: 1,
		Origin:  clientOrigin(t, client, input.Model),
		Type:    "function_call",
		Unit:    llm.OpaqueUnitContentBlock,
		Raw:     raw,
	}
	request, err := client.Prepare(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	mapped := request.ContentChanges()[0].ToID
	callID, resultID := geminiWireToolIDs(t, request.Body())
	if callID != mapped || resultID != mapped {
		t.Fatalf("wire IDs = %q, %q; mapped = %q", callID, resultID, mapped)
	}
	if got := request.Input().Messages[1].Content[0]; got.ToolCall.ID != "source / id" || !bytes.Equal(got.Replay.Raw, raw) {
		t.Fatal("original canonical or replay identity changed")
	}
}

func TestPrepareRemapPreservesGeminiReplayArgumentNumber(t *testing.T) {
	client, err := New(Config{
		Provider:            "google",
		APIKey:              "test",
		UnknownCapabilities: llm.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := geminiToolHistory("source / id")
	input.Messages[1].Content[0].ToolCall.Args = json.RawMessage(
		`{"n":9007199254740993}`,
	)
	input.Messages[1].Content[0].Replay = &llm.Opaque{
		Version: 1,
		Origin:  clientOrigin(t, client, input.Model),
		Type:    "function_call",
		Unit:    llm.OpaqueUnitContentBlock,
		Raw: json.RawMessage(
			`{"functionCall":{"id":"source / id","name":"search","args":{"n":9007199254740993}}}`,
		),
	}
	request, err := client.Prepare(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(request.Body(), []byte(`"n":9007199254740993`)) {
		t.Fatalf("remap changed replay argument number: %s", request.Body())
	}
}

func TestDecodeThenPrepareRemapPreservesGeminiArgumentNumber(t *testing.T) {
	client, err := New(Config{
		Provider:            "google",
		APIKey:              "test",
		UnknownCapabilities: llm.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	origin := clientOrigin(t, client, "gemini-test")
	codec := codec{origin: origin}
	response := []byte(
		`{"responseId":"r1","candidates":[{"content":{"parts":[{"functionCall":{"id":"source / id","name":"search","args":{"n":9007199254740993}}}]},"finishReason":"STOP"}]}`,
	)
	decoded, err := codec.decode(response, llm.ModelRef{
		Provider: "google",
		Model:    "gemini-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	call := decoded.Message.Content[0]
	if !bytes.Contains(call.ToolCall.Args, []byte(`9007199254740993`)) {
		t.Fatalf("provider decode rounded argument number: %s", call.ToolCall.Args)
	}
	input := sampleInput(false)
	input.Messages = append(input.Messages, decoded.Message,
		llm.UserMessage(llm.Content{
			Kind: llm.ContentToolResult,
			ToolResult: &llm.ToolResult{
				CallID: call.ToolCall.ID,
			},
		}),
	)
	request, err := client.Prepare(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.ContentChanges()) != 2 {
		t.Fatalf("decoded tool exchange was not remapped: %+v", request.ContentChanges())
	}
	if !bytes.Contains(request.Body(), []byte(`"n":9007199254740993`)) ||
		bytes.Contains(request.Body(), []byte(`9007199254740992`)) {
		t.Fatalf("prepared body changed provider argument number: %s", request.Body())
	}
	if !bytes.Contains(request.Input().Messages[1].Content[0].Replay.Raw,
		[]byte(`"n":9007199254740993`)) {
		t.Fatal("snapshot changed original provider replay")
	}
}

func TestPrepareRejectsUnsafeGeminiToolIDRemap(t *testing.T) {
	client, err := New(Config{
		Provider:            "google",
		APIKey:              "test",
		UnknownCapabilities: llm.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
	}{
		{
			name: "signed",
			raw: json.RawMessage(
				`{"functionCall":{"id":"signed / id","name":"search","args":{"q":"yes"}},"thoughtSignature":"c2ln"}`,
			),
		},
		{
			name: "unknown dependent field",
			raw: json.RawMessage(
				`{"functionCall":{"id":"signed / id","name":"search","args":{"q":"yes"},"relatedCallID":"signed / id"}}`,
			),
		},
		{
			name: "raw ID differs from original",
			raw: json.RawMessage(
				`{"functionCall":{"id":"another / id","name":"search","args":{"q":"yes"}}}`,
			),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := geminiToolHistory("signed / id")
			input.Messages[1].Content[0].Replay = &llm.Opaque{
				Version: 1,
				Origin:  clientOrigin(t, client, input.Model),
				Type:    "function_call",
				Unit:    llm.OpaqueUnitContentBlock,
				Raw:     tc.raw,
			}
			if _, err := client.Prepare(context.Background(), input); err == nil {
				t.Fatal("unsafe tool call ID edit was accepted")
			}
		})
	}
}

func TestPrepareKeepsSynthesizedGeminiToolID(t *testing.T) {
	client, err := New(Config{
		Provider:            "google",
		APIKey:              "test",
		UnknownCapabilities: llm.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := geminiToolHistory("temporary")
	origin := clientOrigin(t, client, input.Model)
	codec := codec{origin: origin}
	raw := json.RawMessage(
		`{"functionCall":{"name":"search","args":{"q":"yes"}}}`,
	)
	part, err := codec.decodePart(raw, "response-one", 0)
	if err != nil {
		t.Fatal(err)
	}
	id := part.ToolCall.ID
	input.Messages[1].Content[0] = part
	input.Messages[2].Content[0].ToolResult.CallID = id
	request, err := client.Prepare(context.Background(), input)
	if err != nil {
		t.Fatalf("synthesized ID could not replay: %v", err)
	}
	if len(request.ContentChanges()) != 0 {
		t.Fatalf("synthesized portable ID was remapped: %+v", request.ContentChanges())
	}
	callID, resultID := geminiWireToolIDs(t, request.Body())
	if callID != id || resultID != id {
		t.Fatalf("synthesized ID changed: call=%q result=%q want=%q", callID, resultID, id)
	}
	if !bytes.Equal(request.Input().Messages[1].Content[0].Replay.Raw, raw) {
		t.Fatal("original replay was rewritten")
	}
}

func TestDecodeThenPrepareRejectsSignedIDLessGeminiToolCall(t *testing.T) {
	client, err := New(Config{
		Provider:            "google",
		APIKey:              "test",
		UnknownCapabilities: llm.UnknownAllow,
	})
	if err != nil {
		t.Fatal(err)
	}
	origin := clientOrigin(t, client, "gemini-test")
	codec := codec{origin: origin}
	raw := json.RawMessage(
		`{"functionCall":{"name":"search","args":{"q":"yes"}},"thoughtSignature":"c2ln"}`,
	)
	part, err := codec.decodePart(raw, "response-one", 0)
	if err != nil {
		t.Fatal(err)
	}
	input := sampleInput(false)
	input.Messages = append(input.Messages,
		llm.AssistantMessage(part),
		llm.UserMessage(llm.Content{
			Kind: llm.ContentToolResult,
			ToolResult: &llm.ToolResult{
				CallID: part.ToolCall.ID,
			},
		}),
	)
	if _, err := client.Prepare(context.Background(), input); err == nil {
		t.Fatal("signed ID-less replay was rewritten with a synthesized ID")
	}
}

func clientOrigin(t *testing.T, client *Client, model string) content.Origin {
	t.Helper()
	httpClient, err := client.httpClient(model)
	if err != nil {
		t.Fatal(err)
	}
	return httpClient.Origin(model)
}

func geminiToolHistory(id string) llm.Input {
	input := sampleInput(false)
	input.Messages = append(input.Messages,
		llm.AssistantMessage(llm.Content{
			Kind: llm.ContentToolCall,
			ToolCall: &tool.Call{
				ID:   id,
				Name: "search",
				Args: json.RawMessage(`{"q":"yes"}`),
			},
		}),
		llm.UserMessage(llm.Content{
			Kind: llm.ContentToolResult,
			ToolResult: &llm.ToolResult{
				CallID: id,
			},
		}),
	)
	return input
}

func geminiWireToolIDs(t *testing.T, body []byte) (string, string) {
	t.Helper()
	var request struct {
		Contents []struct {
			Parts []struct {
				FunctionCall *struct {
					ID string `json:"id"`
				} `json:"functionCall"`
				FunctionResponse *struct {
					ID string `json:"id"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	var callID, resultID string
	for _, message := range request.Contents {
		for _, part := range message.Parts {
			if part.FunctionCall != nil {
				callID = part.FunctionCall.ID
			}
			if part.FunctionResponse != nil {
				resultID = part.FunctionResponse.ID
			}
		}
	}
	return callID, resultID
}
