//go:build live

package providers_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

// Prompt cache qualification (design/prompt-cache.md, Rollout). Run with:
// DASHI_LIVE_TEST=1 ANTHROPIC_MODEL=... OPENAI_MODEL=... \
// OPENAI_KEY_CHECK_MODEL=gpt-5.6 go test -tags live ./providers \
// -run TestLivePromptCache -v
// Each subtest makes two or three paid calls with a long shared prefix and
// logs usage and the net saving against uncached input. A miss is logged,
// not failed: length minimums and eviction are the provider's choice.
// DASHI_LIVE_CACHE_PAUSE (a duration such as "6m") pauses between the calls,
// for the chat-turn-after-a-pause measurement.
func TestLivePromptCache(t *testing.T) {
	if os.Getenv("DASHI_LIVE_TEST") != "1" {
		t.Skip("set DASHI_LIVE_TEST=1 to make paid provider calls")
	}
	pause, _ := time.ParseDuration(os.Getenv("DASHI_LIVE_CACHE_PAUSE"))
	for _, ttl := range []time.Duration{0, time.Hour} {
		t.Run("anthropic/"+ttl.String(), func(t *testing.T) {
			client := liveClient(t, "ANTHROPIC", func(key string) (llm.Client, error) {
				return messages.New(messages.Config{APIKey: key})
			})
			liveCachePair(t, client, os.Getenv("ANTHROPIC_MODEL"),
				llm.PromptCache{TTL: ttl}, llm.PromptCache{TTL: ttl}, pause)
		})
	}
	t.Run("openai", func(t *testing.T) {
		client := liveClient(t, "OPENAI", func(key string) (llm.Client, error) {
			return responses.New(responses.Config{APIKey: key})
		})
		cache := llm.PromptCache{Key: "dashi-live-" + time.Now().Format("150405")}
		liveCachePair(t, client, os.Getenv("OPENAI_MODEL"), cache, cache, pause)
	})
	// The key check: one prefix under two keys. Only a cache read on the
	// second key may turn on cache_key_default for GPT-5.6 and later.
	t.Run("openai/key-check", func(t *testing.T) {
		model := os.Getenv("OPENAI_KEY_CHECK_MODEL")
		if model == "" {
			t.Skip("set OPENAI_KEY_CHECK_MODEL to run the key check")
		}
		client := liveClient(t, "OPENAI", func(key string) (llm.Client, error) {
			return responses.New(responses.Config{APIKey: key})
		})
		stamp := time.Now().Format("150405")
		second := liveCachePair(t, client, model,
			llm.PromptCache{Key: "dashi-key-a-" + stamp},
			llm.PromptCache{Key: "dashi-key-b-" + stamp}, 0)
		t.Logf("key check: second key read %d cached tokens; "+
			"cache_key_default may be set only when this is positive",
			second.CacheReadInputTokens)
	})
}

func liveClient(
	t *testing.T,
	prefix string,
	newClient func(string) (llm.Client, error),
) llm.Client {
	t.Helper()
	key := os.Getenv(prefix + "_API_KEY")
	if key == "" {
		t.Skipf("set %s_API_KEY", prefix)
	}
	client, err := newClient(key)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// liveCachePair sends two calls that share a long system prompt and first
// message, and returns the second call's usage.
func liveCachePair(
	t *testing.T,
	client llm.Client,
	model string,
	first, second llm.PromptCache,
	pause time.Duration,
) llm.Usage {
	t.Helper()
	if model == "" {
		t.Skip("set the provider's MODEL environment variable")
	}
	// About 6,000 tokens, above every documented minimum.
	system := "You answer in one word. " + strings.Repeat(
		"Reference notes: the archive holds ledgers, maps, and letters. ", 600)
	var usage llm.Usage
	for i, cache := range []llm.PromptCache{first, second} {
		if i == 1 && pause > 0 {
			time.Sleep(pause)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		request, err := client.Prepare(ctx, llm.Input{
			Model:    model,
			System:   system,
			Messages: []llm.Message{llm.UserMessage(llm.Text("Say ok."))},
			Settings: llm.Settings{MaxTokens: 16, PromptCache: cache},
		})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		result, err := client.Send(ctx, llm.Call{Request: request})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if result.Usage == nil {
			t.Fatal("no usage reported")
		}
		usage = *result.Usage
		t.Logf("call %d: body cache fields %s; usage %+v",
			i+1, cacheFields(request.Body()), usage)
		logNetSaving(t, client, model, usage)
	}
	if usage.CacheReadInputTokens == 0 {
		t.Logf("second call read nothing from the cache")
	}
	return usage
}

// logNetSaving compares the call's estimate with the same input uncached.
func logNetSaving(t *testing.T, client llm.Client, model string, u llm.Usage) {
	t.Helper()
	info, ok := client.ModelInfo(model)
	if !ok || info.Pricing == nil {
		return
	}
	cost, ok := info.Pricing.Estimate(&u)
	uncached := llm.Usage{InputTokens: u.TotalInputTokens(), OutputTokens: u.OutputTokens}
	base, baseOK := info.Pricing.Estimate(&uncached)
	if ok && baseOK {
		t.Logf("cost %s, uncached %s, net saving %s",
			cost.Total, base.Total, base.Total-cost.Total)
	}
}

func cacheFields(body []byte) string {
	var fields []string
	for _, f := range []string{"cache_control", "prompt_cache_key", "prompt_cache_retention"} {
		if strings.Contains(string(body), `"`+f+`"`) {
			fields = append(fields, f)
		}
	}
	return strings.Join(fields, ", ")
}
