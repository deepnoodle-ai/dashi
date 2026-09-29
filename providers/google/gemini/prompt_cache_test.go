package gemini

import (
	"bytes"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Gemini caches implicitly and has no lifetime field: a requested lifetime
// is omitted with a warning, or refused under strict, and a key is ignored.
func TestPrepareOmitsPromptCacheLifetime(t *testing.T) {
	for _, policy := range []llm.CapabilityPolicy{
		llm.CapabilityAdjust,
		llm.CapabilityStrict,
	} {
		client, err := New(Config{
			Provider:         "google",
			APIKey:           "test",
			Endpoint:         "https://gemini.example/v1beta/models/{model}:generateContent",
			CapabilityPolicy: policy,
		})
		if err != nil {
			t.Fatal(err)
		}
		input := sampleInput(false)
		input.Settings.PromptCache = llm.PromptCache{TTL: time.Hour, Key: "s-1"}
		request, err := client.Prepare(t.Context(), input)
		if policy == llm.CapabilityStrict {
			if err == nil || !bytes.Contains([]byte(err.Error()), []byte("prompt_cache")) {
				t.Fatalf("strict omitted a cache lifetime: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		plain, err := client.Prepare(t.Context(), sampleInput(false))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(request.Body(), plain.Body()) {
			t.Fatalf("cache settings changed the body:\n%s\n%s", request.Body(), plain.Body())
		}
		warnings := request.Warnings()
		if len(warnings) != 1 || warnings[0].Code != llm.WarningSettingOmitted ||
			warnings[0].Field != llm.SettingPromptCacheTTL ||
			request.Effective().PromptCache.TTL != 0 {
			t.Fatalf("warnings = %+v", warnings)
		}
	}
}
