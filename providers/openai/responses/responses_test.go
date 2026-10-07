package responses

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestServiceTierSurvivesBlockingAndStreamingResponses(t *testing.T) {
	wire := codec{origin: content.Origin{Provider: "openai", API: API, Model: "model", Scope: "scope"}}
	requested := llm.ModelRef{Provider: "openai", Model: "model"}
	for _, tier := range []string{"default", "priority", ""} {
		t.Run(tier, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{
				"object": "response", "status": "completed", "model": "model", "service_tier": tier,
				"usage":  map[string]int{"input_tokens": 9, "output_tokens": 2},
				"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "Hi"}}}},
			})
			blocking, err := wire.decode(raw, requested)
			if err != nil {
				t.Fatal(err)
			}
			stream := `data: {"type":"response.completed","response":` + string(raw) + "}\n\n"
			streaming, err := wire.readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{Requested: requested}, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, result := range []llm.Result{blocking, streaming} {
				if result.ServiceTier != tier || result.Stop.Kind != llm.StopFinished || result.Message.Text() != "Hi" {
					t.Fatal("served tier or response lost")
				}
			}
		})
	}
}

func TestServiceTierSurvivesInterruptedStream(t *testing.T) {
	wire := codec{origin: content.Origin{Provider: "openai", API: API, Model: "model", Scope: "scope"}}
	stream := "data: {\"type\":\"response.created\",\"response\":{\"object\":\"response\",\"model\":\"model\",\"service_tier\":\"priority\"}}\n\n"
	result, err := wire.readStream(io.NopCloser(strings.NewReader(stream)), llm.Result{}, nil)
	if err == nil || result.ServiceTier != "priority" || result.Stop.Kind != llm.StopIncomplete {
		t.Fatal("interrupted response lost served tier")
	}
}
