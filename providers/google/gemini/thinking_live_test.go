//go:build live

package gemini

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Run with DASHI_LIVE_THINKING_TEST=1 and GEMINI_API_KEY. It makes nine small
// paid calls: one per documented thinking level on gemini-4-hydrogen (4) and
// gemini-4-argon (3), a raw probe of the unlisted "minimal" level on
// gemini-4-hydrogen, and a thinkingBudget-only call on gemini-4-hydrogen.
func TestLiveThinkingLevels(t *testing.T) {
	if os.Getenv("DASHI_LIVE_THINKING_TEST") != "1" {
		t.Skip("set DASHI_LIVE_THINKING_TEST=1 to make paid Gemini LLM calls")
	}
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Fatal("GEMINI_API_KEY is required")
	}
	client, err := New(Config{APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	call := func(t *testing.T, model string, reasoning llm.ReasoningSettings) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		request, err := client.Prepare(ctx, llm.Input{
			Model:    model,
			Settings: llm.Settings{MaxTokens: 8192, Reasoning: &reasoning},
			Messages: []llm.Message{
				llm.UserMessage(llm.Text("What is 17 * 23? Reply with the number only.")),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if warnings := request.Warnings(); len(warnings) != 0 {
			t.Fatalf("documented setting warned: %+v", warnings)
		}
		result, err := client.Send(ctx, llm.Call{Request: request})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf(
			"stop=%s reasoning_tokens=%d output_tokens=%d text=%q",
			result.Stop.Kind,
			result.Usage.ReasoningTokens,
			result.Usage.OutputTokens,
			result.Message.Text(),
		)
		if !strings.Contains(result.Message.Text(), "391") {
			t.Fatalf("answer = %q", result.Message.Text())
		}
	}
	for model, levels := range map[string][]string{
		"gemini-4-hydrogen": {"low", "medium", "high", "xhigh"},
		"gemini-4-argon":    {"low", "medium", "high"},
	} {
		for _, level := range levels {
			t.Run(model+"/"+level, func(t *testing.T) {
				call(t, model, llm.ReasoningSettings{Effort: level})
			})
		}
	}
	t.Run("gemini-4-hydrogen/budget", func(t *testing.T) {
		call(t, "gemini-4-hydrogen", llm.ReasoningSettings{BudgetTokens: 1024})
	})
	// Fit never sends an unlisted level, so the probe posts it directly.
	t.Run("gemini-4-hydrogen/minimal-raw", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		body := `{"contents":[{"role":"user","parts":[{"text":"Say ok."}]}],` +
			`"generationConfig":{"thinkingConfig":{"thinkingLevel":"minimal"}}}`
		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			strings.ReplaceAll(DefaultEndpoint, "{model}", "gemini-4-hydrogen"),
			bytes.NewBufferString(body),
		)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Goog-Api-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		t.Logf("status=%d body=%s", resp.StatusCode, raw)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("minimal on gemini-4-hydrogen = %d, documented as an error", resp.StatusCode)
		}
	})
}
