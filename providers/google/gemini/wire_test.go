package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Gemini omits args, or sends null, when it calls a tool with no arguments.
// The call carries an empty object, which a tool can execute and the next
// request can replay.
func TestDecodeCallWithoutArgsHasEmptyObject(t *testing.T) {
	for _, call := range []string{
		`{"id":"call-1","name":"ping"}`,
		`{"id":"call-1","name":"ping","args":null}`,
	} {
		response := `{"responseId":"r1","modelVersion":"served","candidates":[{"content":{"role":"model","parts":[{"functionCall":` +
			call + `}]},"finishReason":"STOP"}]}`
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(response))
		}))
		p := newTestClient(t, server)
		input := sampleInput(false)
		input.Tools = []tool.Definition{{Name: "ping", Schema: json.RawMessage(`{"type":"object"}`)}}
		prepared, err := p.Prepare(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := p.Send(t.Context(), llm.Call{Request: prepared})
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		got := result.Message.Content[0].ToolCall
		if got == nil || string(got.Args) != `{}` {
			t.Fatalf("%s: call = %+v", call, got)
		}
		ping, err := tool.Func(
			"ping",
			"",
			func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
				return tool.Succeeded("pong"), nil
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := ping.Executor.Execute(t.Context(), *got)
		if err != nil || outcome.State != tool.StateSucceeded {
			t.Fatalf("%s: zero-argument call did not execute: %+v %v", call, outcome, err)
		}
		input.Messages = append(
			input.Messages,
			result.Message,
			llm.UserMessage(llm.Content{
				Kind:       llm.ContentToolResult,
				ToolResult: &llm.ToolResult{CallID: got.ID},
			}),
		)
		if _, err := p.Prepare(t.Context(), input); err != nil {
			t.Fatalf("%s: replay failed: %v", call, err)
		}
	}
}

// A response without content or model version invents neither message
// provenance nor a half-filled served model.
func TestDecodeEmptyResponseHasNoProvenance(t *testing.T) {
	wire := codec{origin: meterOrigin(t)}
	result, err := wire.decode(
		[]byte(`{"responseId":"r1","candidates":[{"finishReason":"STOP"}]}`),
		llm.ModelRef{Provider: "google", Model: "requested"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Message.Source != nil || result.Served != (llm.ModelRef{}) {
		t.Fatalf("empty response invented provenance: %+v", result)
	}
}

// A part that fails to decode keeps provenance for the parts before it.
func TestDecodeFailureKeepsProvenanceOfEarlierParts(t *testing.T) {
	wire := codec{origin: meterOrigin(t)}
	result, err := wire.decode(
		[]byte(`{"responseId":"r1","modelVersion":"served","candidates":[{"content":{"parts":[{"text":"hi"},{"functionCall":{"name":""}}]},"finishReason":"STOP"}]}`),
		llm.ModelRef{Provider: "google", Model: "requested"},
	)
	if err == nil || !strings.Contains(err.Error(), "requires name") {
		t.Fatalf("err = %v", err)
	}
	if len(result.Message.Content) != 1 || result.Message.Source == nil {
		t.Fatalf("partial result lost provenance: %+v", result)
	}
}
