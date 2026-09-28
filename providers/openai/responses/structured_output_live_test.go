//go:build live

package responses

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

// TestLiveNativeStructuredOutput is an opt-in, paid route probe. A pass
// qualifies only the named model and the public Responses endpoint on the
// date it ran; the ordinary test suite uses offline fixtures.
func TestLiveNativeStructuredOutput(t *testing.T) {
	if os.Getenv("DASHI_LIVE_STRUCTURED_OUTPUT_TEST") != "1" {
		t.Skip("set DASHI_LIVE_STRUCTURED_OUTPUT_TEST=1 to make paid calls")
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("OPENAI_API_KEY is required")
	}
	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		t.Skip("OPENAI_MODEL is required to identify the probed model")
	}
	client, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"type":"object","properties":{"label":{"type":"string"}},"required":["label"],"additionalProperties":false}`)
	for _, stream := range []bool{false, true} {
		name := "blocking"
		if stream {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			input := llm.Input{
				Model:        model,
				Messages:     []llm.Message{llm.UserMessage(llm.Text("Reply with label equal to ok."))},
				OutputSchema: schema,
				Stream:       stream,
			}
			result, err := llm.Generate(t.Context(), client, input, nil)
			if err != nil {
				t.Fatalf("model=%s stream=%t: %v (stop=%+v)", model, stream, err, result.Stop)
			}
			var value struct {
				Label string `json:"label"`
			}
			if result.Stop.Kind != llm.StopFinished || len(result.Output) == 0 ||
				json.Unmarshal(result.Output, &value) != nil || value.Label != "ok" {
				t.Fatalf("model=%s stream=%t result=%+v", model, stream, result)
			}
			t.Logf(
				"date=%s endpoint=%s model=%s stream=%t schema=%s output=%s usage=%+v",
				time.Now().UTC().Format(time.RFC3339),
				openAIEndpoint,
				model,
				stream,
				schema,
				result.Output,
				result.Usage,
			)
		})
	}
}
