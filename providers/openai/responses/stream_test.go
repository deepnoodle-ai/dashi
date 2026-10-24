package responses

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func streamCodec() codec {
	return codec{origin: content.Origin{Provider: "openai", API: API, Model: "model", Scope: "scope"}}
}

func readEvents(t *testing.T, events ...string) (llm.Result, error) {
	t.Helper()
	stream := "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
	return streamCodec().readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{}, nil)
}

// TestStreamKeepsPartsFromAddedMessageItem covers a message item whose
// output_item.added event already carries content. An interrupted stream
// must keep those parts, and a text delta must not overwrite an opaque part.
func TestStreamKeepsPartsFromAddedMessageItem(t *testing.T) {
	t.Run("earlier part kept", func(t *testing.T) {
		result, err := readEvents(t,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello "}]}}`,
			`{"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":"world"}`,
		)
		if err == nil {
			t.Fatal("interrupted stream succeeded")
		}
		if got := result.Message.Text(); got != "Hello world" {
			t.Fatalf("text = %q, want %q", got, "Hello world")
		}
	})
	t.Run("opaque part not overwritten", func(t *testing.T) {
		result, err := readEvents(t,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"future_part"}]}}`,
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"leak"}`,
		)
		if err == nil {
			t.Fatal("interrupted stream succeeded")
		}
		if len(result.Message.Content) != 1 ||
			result.Message.Content[0].Opaque == nil ||
			result.Message.Content[0].Opaque.Type != "future_part" {
			t.Fatalf("content = %+v, want the opaque part", result.Message.Content)
		}
	})
}

// TestStreamCutOffUsageIsNotComplete covers usage seen before a terminal
// event. Only a terminal response reports final provider tool counts.
func TestStreamCutOffUsageIsNotComplete(t *testing.T) {
	result, err := readEvents(t,
		`{"type":"response.created","response":{"object":"response","status":"in_progress","model":"model","usage":{"input_tokens":10,"output_tokens":0}}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"in_progress"}}`,
	)
	if err == nil {
		t.Fatal("interrupted stream succeeded")
	}
	if result.Usage == nil || result.Usage.RequestsComplete {
		t.Fatalf("usage = %+v, want incomplete request counts", result.Usage)
	}
}

// TestStreamErrorEventKeepsProviderMessage covers a stream error event. The
// error must carry the provider's code and message.
func TestStreamErrorEventKeepsProviderMessage(t *testing.T) {
	_, err := readEvents(t, `{"type":"error","code":"server_error","message":"boom"}`)
	e, ok := errors.AsType[*llm.Error](err)
	if !ok || e.Code != "server_error" ||
		!strings.HasSuffix(err.Error(), "provider stream error: boom") {
		t.Fatalf("err = %v, want the provider's code and message", err)
	}
}
