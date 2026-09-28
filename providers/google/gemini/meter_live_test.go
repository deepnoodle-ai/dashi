//go:build live

package gemini

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Run with DASHI_LIVE_TEST=1 and GEMINI_API_KEY; GEMINI_MODEL overrides the
// default gemini-2.5-flash. The blocking and streaming subtests each make one
// paid call grounded with Google Search, which is billed per grounded prompt
// or per query depending on the model.
func TestLiveGroundedSearchMeters(t *testing.T) {
	if os.Getenv("DASHI_LIVE_TEST") != "1" {
		t.Skip("set DASHI_LIVE_TEST=1 to make paid Gemini calls")
	}
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Fatal("GEMINI_API_KEY is required")
	}
	modelName := os.Getenv("GEMINI_MODEL")
	if modelName == "" {
		modelName = "gemini-2.5-flash"
	}
	client, err := New(Config{
		APIKey:           key,
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		stream bool
	}{{"blocking", false}, {"streaming", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			request, err := client.Prepare(ctx, llm.Input{
				Model:  modelName,
				Stream: tc.stream,
				Messages: []llm.Message{
					llm.UserMessage(llm.Text(
						"Search the web: what is today's top headline on " +
							"bbc.com? Answer in one sentence.",
					)),
				},
				Tools: []tool.Definition{
					NewGoogleSearchTool(GoogleSearchToolOptions{}).Definition,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Send(ctx, llm.Call{Request: request})
			if err != nil {
				t.Fatal(err)
			}
			if result.Usage == nil {
				t.Fatal("grounded call returned no usage")
			}
			requests := result.Usage.Requests
			t.Logf("model %s requests %v", result.Served.Model, requests)
			if requests[llm.MeterWebSearch] < 1 ||
				requests[llm.MeterGroundedPrompt] != 1 {
				t.Fatalf(
					"grounded call missing search meters: %v\nraw: %s",
					requests,
					result.Raw,
				)
			}
		})
	}
}
