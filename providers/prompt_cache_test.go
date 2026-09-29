package providers_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
)

// cacheBody decodes a prepared body for field checks.
func cacheBody(t *testing.T, r llm.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(r.Body(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func cacheInput(model string, ttl time.Duration) llm.Input {
	return llm.Input{
		Model:  model,
		System: "You are terse.",
		Tools: []tool.Definition{
			{Name: "look", Description: "Look.", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "fetch", Description: "Fetch.", Schema: json.RawMessage(`{"type":"object"}`)},
		},
		Messages: []llm.Message{llm.UserMessage(llm.Text("hello"))},
		Settings: llm.Settings{PromptCache: llm.PromptCache{TTL: ttl}},
	}
}

// ttlAdjustment returns the prompt cache lifetime decision, if any.
func ttlAdjustment(r llm.Request) (llm.Adjustment, bool) {
	for _, a := range r.Adjustments() {
		if a.Field == llm.SettingPromptCacheTTL {
			return a, true
		}
	}
	return llm.Adjustment{}, false
}

func TestAnthropicCachePointsPerLifetime(t *testing.T) {
	p := newMessages(t, messages.Config{APIKey: "test"})
	ephemeral := map[string]any{"type": "ephemeral"}
	oneHour := map[string]any{"type": "ephemeral", "ttl": "1h"}
	for _, tc := range []struct {
		name      string
		ttl       time.Duration
		point     map[string]any
		effective time.Duration
		action    llm.AdjustmentAction // empty means none
	}{
		{"zero caches by default", 0, ephemeral, 5 * time.Minute, llm.AdjustmentDefault},
		{"5m is the default lifetime", 5 * time.Minute, ephemeral, 5 * time.Minute, ""},
		{"30m is met by 1h", 30 * time.Minute, oneHour, 30 * time.Minute, ""},
		{"1h", time.Hour, oneHour, time.Hour, ""},
		{"2h clamps to 1h", 2 * time.Hour, oneHour, time.Hour, llm.AdjustmentClamp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := cacheInput("claude-sonnet-5", tc.ttl)
			before, _ := json.Marshal(in)
			r, err := p.Prepare(t.Context(), in)
			if err != nil {
				t.Fatal(err)
			}
			if after, _ := json.Marshal(in); string(after) != string(before) {
				t.Fatal("preparation changed caller input")
			}
			body := cacheBody(t, r)
			system := body["system"].([]any)[0].(map[string]any)
			tools := body["tools"].([]any)
			if !reflect.DeepEqual(system["cache_control"], tc.point) ||
				!reflect.DeepEqual(body["cache_control"], tc.point) {
				t.Fatalf("cache points: system %v, top %v", system["cache_control"], body["cache_control"])
			}
			for _, def := range tools {
				if _, ok := def.(map[string]any)["cache_control"]; ok {
					t.Fatal("a tool has a cache point although the system prompt holds it")
				}
			}
			if got := r.Effective().PromptCache.TTL; got != tc.effective {
				t.Fatalf("effective ttl = %v, want %v", got, tc.effective)
			}
			a, ok := ttlAdjustment(r)
			if ok != (tc.action != "") || a.Action != tc.action {
				t.Fatalf("ttl adjustment = %+v", r.Adjustments())
			}
			warned := len(r.Warnings()) > 0
			if warned != (tc.action == llm.AdjustmentClamp) {
				t.Fatalf("warnings = %+v", r.Warnings())
			}
		})
	}
}

func TestAnthropicFixedCachePointPlacement(t *testing.T) {
	p := newMessages(t, messages.Config{APIKey: "test"})

	// Without system text the fixed point is on the last tool definition.
	in := cacheInput("claude-sonnet-5", 0)
	in.System = ""
	r, err := p.Prepare(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	body := cacheBody(t, r)
	tools := body["tools"].([]any)
	if _, ok := tools[0].(map[string]any)["cache_control"]; ok {
		t.Fatal("the first tool has a cache point")
	}
	if tools[1].(map[string]any)["cache_control"] == nil ||
		body["cache_control"] == nil || body["system"] != nil {
		t.Fatalf("tool-only cache points: %s", r.Body())
	}

	// System text hoisted from the history follows the fixed point.
	in = cacheInput("claude-sonnet-5", 0)
	in.Messages = append(
		[]llm.Message{{Role: llm.RoleSystem, Content: []llm.Content{llm.Text("Be kind.")}}},
		in.Messages...,
	)
	r, err = p.Prepare(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	system := cacheBody(t, r)["system"].([]any)
	if len(system) != 2 || system[0].(map[string]any)["cache_control"] == nil ||
		system[1].(map[string]any)["cache_control"] != nil {
		t.Fatalf("hoisted system text: %v", system)
	}

	// With neither, only the automatic point remains.
	in = cacheInput("claude-sonnet-5", 0)
	in.System, in.Tools = "", nil
	r, err = p.Prepare(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if body := cacheBody(t, r); body["cache_control"] == nil ||
		strings.Count(string(r.Body()), "cache_control") != 1 {
		t.Fatalf("automatic point only: %s", r.Body())
	}
}

func TestAnthropicCacheDisableAndDestinations(t *testing.T) {
	public := newMessages(t, messages.Config{APIKey: "test"})
	in := cacheInput("claude-sonnet-5", 0)
	in.Settings.PromptCache.Disable = true
	r, err := public.Prepare(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(r.Body()), "cache_control") || len(r.Adjustments()) != 1 {
		t.Fatalf("disabled cache sent %s, %+v", r.Body(), r.Adjustments())
	}

	in.Settings.PromptCache.TTL = time.Hour
	if _, err := public.Prepare(t.Context(), in); err == nil ||
		!strings.Contains(err.Error(), "disable and ttl are both set") {
		t.Fatalf("disable with ttl: %v", err)
	}

	// A proxy is not a catalog destination: no default, and an explicit
	// lifetime is sent as unverified, or refused under strict.
	proxy := newMessages(t, messages.Config{
		Provider: "proxy",
		Endpoint: "https://proxy.example/v1/messages",
		APIKey:   "test",
	})
	r, err = proxy.Prepare(t.Context(), cacheInput("claude-sonnet-5", 0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(r.Body()), "cache_control") {
		t.Fatalf("proxy cached by default: %s", r.Body())
	}
	r, err = proxy.Prepare(t.Context(), cacheInput("claude-sonnet-5", time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.Body()), `"ttl":"1h"`) ||
		!slices.Contains(r.Snapshot().Unverified, llm.FeaturePromptCache) {
		t.Fatalf("explicit proxy cache: %s, %v", r.Body(), r.Snapshot().Unverified)
	}
	strict := newMessages(t, messages.Config{
		Provider:         "proxy",
		Endpoint:         "https://proxy.example/v1/messages",
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityStrict,
	})
	if _, err := strict.Prepare(t.Context(), cacheInput("claude-sonnet-5", time.Hour)); err == nil {
		t.Fatal("strict sent a cache request of unknown support")
	}
	strictPublic := newMessages(t, messages.Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityStrict,
	})
	if _, err := strictPublic.Prepare(t.Context(), cacheInput("claude-sonnet-5", 2*time.Hour)); err == nil {
		t.Fatal("strict clamped a cache lifetime")
	}
}

func TestResponsesCacheKeyAndRetention(t *testing.T) {
	p := newResponses(t, responses.Config{APIKey: "test"})
	for _, tc := range []struct {
		name      string
		model     string
		ttl       time.Duration
		retention any // nil means absent
		effective time.Duration
		action    llm.AdjustmentAction
	}{
		{"listed model keeps its default for 5m", "gpt-5.5", 5 * time.Minute, nil, 5 * time.Minute, ""},
		{"listed model sends 24h for 1h", "gpt-5.5", time.Hour, "24h", time.Hour, ""},
		{"unlisted model clamps 1h to 5m", "gpt-5-mini", time.Hour, nil, 5 * time.Minute, llm.AdjustmentClamp},
		{"GPT-5.6 meets 30m without a field", "gpt-5.6", 30 * time.Minute, nil, 30 * time.Minute, ""},
		{"GPT-5.6 clamps 1h to 30m", "gpt-5.6", time.Hour, nil, 30 * time.Minute, llm.AdjustmentClamp},
		{"zero sends no lifetime", "gpt-5.5", 0, nil, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := cacheInput(tc.model, tc.ttl)
			in.Settings.PromptCache.Key = "conversation-7"
			r, err := p.Prepare(t.Context(), in)
			if err != nil {
				t.Fatal(err)
			}
			body := cacheBody(t, r)
			if body["prompt_cache_key"] != "conversation-7" ||
				body["prompt_cache_retention"] != tc.retention ||
				body["prompt_cache_options"] != nil {
				t.Fatalf("cache fields: %s", r.Body())
			}
			if got := r.Effective().PromptCache.TTL; got != tc.effective {
				t.Fatalf("effective ttl = %v, want %v", got, tc.effective)
			}
			a, ok := ttlAdjustment(r)
			if ok != (tc.action != "") || a.Action != tc.action ||
				(len(r.Warnings()) > 0) != (tc.action != "") {
				t.Fatalf("adjustments %+v warnings %+v", r.Adjustments(), r.Warnings())
			}
		})
	}
	strict := newResponses(t, responses.Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityStrict,
	})
	if _, err := strict.Prepare(t.Context(), cacheInput("gpt-5.6", time.Hour)); err == nil {
		t.Fatal("strict clamped a cache lifetime")
	}
}

func TestResponsesCacheKeyIsSentAsGiven(t *testing.T) {
	p := newResponses(t, responses.Config{APIKey: "test"})
	key := func(k string, disable bool) any {
		in := cacheInput("gpt-5.5", 0)
		in.Settings.PromptCache = llm.PromptCache{Key: k, Disable: disable}
		r, err := p.Prepare(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		if r.Effective().PromptCache.Key != k {
			t.Fatalf("effective key changed: %q", r.Effective().PromptCache.Key)
		}
		return cacheBody(t, r)["prompt_cache_key"]
	}
	exact := strings.Repeat("k", 64)
	if got := key(exact, false); got != exact {
		t.Fatalf("64-character key sent as %v", got)
	}
	long := strings.Repeat("k", 65)
	hashed, _ := key(long, false).(string)
	if len(hashed) != 38 || !strings.HasPrefix(hashed, "dashi-") ||
		key(long, false) != hashed {
		t.Fatalf("long key sent as %q", hashed)
	}
	// Disable turns off cache requests, not OpenAI's per-key accounting.
	if got := key("tenant-4", true); got != "tenant-4" {
		t.Fatalf("disabled cache dropped the key: %v", got)
	}

	// Another Responses endpoint gets the key only with evidence.
	other := newResponses(t, responses.Config{
		Provider: "other",
		Endpoint: "https://other.example/v1/responses",
		APIKey:   "test",
	})
	in := cacheInput("gpt-5.5", 0)
	in.Settings.PromptCache.Key = "tenant-4"
	r, err := other.Prepare(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(r.Body()), "prompt_cache") {
		t.Fatalf("unqualified endpoint got cache fields: %s", r.Body())
	}
}

func TestCompletionsOmitsCacheLifetime(t *testing.T) {
	p, err := completions.New(completions.Config{APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	in := cacheInput("gpt-5.5", time.Hour)
	in.Settings.PromptCache.Key = "tenant-4"
	r, err := p.Prepare(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := ttlAdjustment(r)
	if !ok || a.Action != llm.AdjustmentOmit || len(r.Warnings()) != 1 ||
		strings.Contains(string(r.Body()), "cache") {
		t.Fatalf("completions: %s, %+v", r.Body(), r.Adjustments())
	}
	strict, err := completions.New(completions.Config{
		APIKey:           "test",
		CapabilityPolicy: llm.CapabilityStrict,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.Prepare(t.Context(), in); err == nil {
		t.Fatal("strict omitted a cache lifetime")
	}
}
