package dashi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

// Prompt caching works with no configuration on the Claude API: Prepare
// adds a cache point to the system prompt and Anthropic's automatic one to
// the request, and records the default lifetime. TTL asks for a longer
// lifetime; OpenAI takes a key and a retention instead. Preparing makes no
// network call.
func Example_promptCache() {
	ctx := context.Background()
	claude, _ := messages.New(messages.Config{APIKey: "unused"})
	in := llm.Input{
		Model:    "claude-sonnet-5",
		System:   "You review pull requests.",
		Messages: []llm.Message{llm.UserMessage(llm.Text("Review #42."))},
	}
	show := func(r llm.Request) {
		var body map[string]json.RawMessage
		_ = json.Unmarshal(r.Body(), &body)
		var system []map[string]json.RawMessage
		_ = json.Unmarshal(body["system"], &system)
		fmt.Printf("system %s, automatic %s\n",
			system[0]["cache_control"], body["cache_control"])
	}

	r, _ := claude.Prepare(ctx, in)
	show(r)
	for _, a := range r.Adjustments() {
		if a.Field == llm.SettingPromptCacheTTL {
			fmt.Println(a.Action, string(a.Effective))
		}
	}

	in.Settings.PromptCache.TTL = time.Hour
	r, _ = claude.Prepare(ctx, in)
	show(r)

	gpt, _ := responses.New(responses.Config{APIKey: "unused"})
	in.Model = "gpt-5.5"
	in.Settings.PromptCache.Key = "conversation-42"
	r, _ = gpt.Prepare(ctx, in)
	var body map[string]json.RawMessage
	_ = json.Unmarshal(r.Body(), &body)
	fmt.Printf("key %s, retention %s\n",
		body["prompt_cache_key"], body["prompt_cache_retention"])
	// Output:
	// system {"type":"ephemeral"}, automatic {"type":"ephemeral"}
	// default "5m"
	// system {"ttl":"1h","type":"ephemeral"}, automatic {"ttl":"1h","type":"ephemeral"}
	// key "conversation-42", retention "24h"
}
