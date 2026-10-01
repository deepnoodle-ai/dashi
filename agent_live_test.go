//go:build live

package dashi_test

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/internal/providertest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
)

// Run with DASHI_LIVE_AGENT_TEST=1 and the corresponding provider key/model.
// Each streamed final turn makes one paid call; each tool turn makes two.
// The Gemini equivalents live in the providers/google/gemini module.

// liveProviders are the routes the agent probes run against. The prefix
// selects the <PREFIX>_API_KEY and <PREFIX>_MODEL variables.
var liveProviders = []struct {
	name string
	new  func(string) (llm.Client, error)
}{
	{
		"openai",
		func(key string) (llm.Client, error) {
			return responses.New(responses.Config{
				APIKey:           key,
				CapabilityPolicy: llm.CapabilityAdjust,
			})
		},
	},
	{
		"anthropic",
		func(key string) (llm.Client, error) {
			return messages.New(messages.Config{
				APIKey:           key,
				CapabilityPolicy: llm.CapabilityAdjust,
			})
		},
	},
}

func liveClient(t *testing.T, name string) (llm.Client, string) {
	t.Helper()
	for _, p := range liveProviders {
		if p.name != name {
			continue
		}
		key, modelName := providertest.AgentCredentials(t, strings.ToUpper(name))
		client, err := p.new(key)
		if err != nil {
			t.Fatal(err)
		}
		return client, modelName
	}
	t.Fatalf("unknown live provider %q", name)
	return nil, ""
}

func TestLiveOpenAIAgentTurn(t *testing.T) {
	client, modelName := liveClient(t, "openai")
	providertest.AgentTurn(t, "openai", client, modelName)
}

func TestLiveAnthropicAgentTurn(t *testing.T) {
	client, modelName := liveClient(t, "anthropic")
	providertest.AgentTurn(t, "anthropic", client, modelName)
}

// These probes verify the shared turn contract over real provider tool calls.
// Each test makes one paid call. Run them separately from the broader agent
// turn probes with DASHI_LIVE_AGENT_TEST=1.
func TestLiveOpenAIStructuredOutput(t *testing.T) {
	client, modelName := liveClient(t, "openai")
	providertest.StructuredOutput(t, client, modelName)
}

func TestLiveAnthropicStructuredOutput(t *testing.T) {
	client, modelName := liveClient(t, "anthropic")
	providertest.StructuredOutput(t, client, modelName)
}

// Each subtest makes one paid call with a recorded reminder.
func TestLiveRecordedReminders(t *testing.T) {
	for _, p := range liveProviders {
		t.Run(p.name, func(t *testing.T) {
			client, modelName := liveClient(t, p.name)
			providertest.RecordedReminder(t, client, modelName)
		})
	}
}

// Each subtest makes one paid streamed model call through Runner.Stream.
func TestLiveIteratorAgentTurns(t *testing.T) {
	for _, p := range liveProviders {
		t.Run(p.name, func(t *testing.T) {
			client, modelName := liveClient(t, p.name)
			providertest.IteratorTurn(t, client, modelName)
		})
	}
}

// Each subtest makes exactly two paid calls. The second call must see the
// recorded tool result while no longer advertising application tools.
func TestLiveReservedFinalReply(t *testing.T) {
	for _, p := range liveProviders {
		t.Run(p.name, func(t *testing.T) {
			client, modelName := liveClient(t, p.name)
			providertest.ReservedFinalReply(t, client, modelName)
		})
	}
}

// Each subtest makes two paid calls: a steer sent while the tool runs must
// follow the tool result in the second call.
func TestLiveSteerAfterToolBatch(t *testing.T) {
	for _, p := range liveProviders {
		t.Run(p.name, func(t *testing.T) {
			client, modelName := liveClient(t, p.name)
			providertest.SteerAfterToolBatch(t, client, modelName)
		})
	}
}

// Each subtest makes two paid calls: an urgent steer cuts a streamed
// reply, and the provider accepts the partial, reminder, and steer.
func TestLiveUrgentSteerCutsStream(t *testing.T) {
	for _, p := range liveProviders {
		t.Run(p.name, func(t *testing.T) {
			client, modelName := liveClient(t, p.name)
			providertest.UrgentSteerCutsStream(t, client, modelName)
		})
	}
}

// Each subtest makes two paid calls: the cut partial ends in whitespace,
// and the provider accepts the next request.
func TestLiveUrgentSteerPartialEndsInWhitespace(t *testing.T) {
	for _, p := range liveProviders {
		t.Run(p.name, func(t *testing.T) {
			client, modelName := liveClient(t, p.name)
			providertest.UrgentSteerPartialEndsInWhitespace(
				t,
				client,
				modelName,
			)
		})
	}
}

// Three paid calls with about 14 web searches, about $1.30 on
// claude-haiku-4-5: an urgent steer arrives after a pause_turn, and the
// provider accepts the request that carries it.
func TestLiveAnthropicUrgentSteerAfterPause(t *testing.T) {
	client, modelName := liveClient(t, "anthropic")
	search, err := messages.NewWebSearchTool(messages.WebSearchToolOptions{
		MaxUses: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	providertest.UrgentSteerAfterPause(t, client, modelName, search)
}
