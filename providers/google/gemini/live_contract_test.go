//go:build live

package gemini_test

import (
	"os"
	"testing"

	"github.com/deepnoodle-ai/dashi/internal/providertest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/google/gemini"
)

// These are the Gemini cases of the root module's live provider and agent
// probes. Each test names its gate variable and the calls it makes.

func newLiveClient(key string) (llm.Client, error) {
	return gemini.New(gemini.Config{
		APIKey:           key,
		CapabilityPolicy: llm.CapabilityAdjust,
	})
}

func liveAgentClient(t *testing.T) (llm.Client, string) {
	t.Helper()
	key, modelName := providertest.AgentCredentials(t, "GEMINI")
	client, err := newLiveClient(key)
	if err != nil {
		t.Fatal(err)
	}
	return client, modelName
}

// DASHI_LIVE_TEST=1 with GEMINI_API_KEY and GEMINI_MODEL; two paid requests.
func TestLiveAgentFinalFacts(t *testing.T) {
	providertest.LiveFinalFacts(
		t,
		os.Getenv("GEMINI_API_KEY"),
		os.Getenv("GEMINI_MODEL"),
		newLiveClient,
	)
}

// DASHI_LIVE_TEST=1 with GEMINI_API_KEY. Media probes send a few bytes; the
// context probe is rejected before inference.
func TestLiveErrorReasons(t *testing.T) {
	providertest.LiveErrorReasons(t, providertest.ErrorProbe{
		KeyEnv: "GEMINI_API_KEY",
		Model:  "gemini-2.5-flash-lite",
		Window: 1_048_576,
		Media:  true,
		New: func() (llm.Client, error) {
			return gemini.New(gemini.Config{CapabilityPolicy: llm.CapabilityAdjust})
		},
	})
}

// The agent probes below need DASHI_LIVE_AGENT_TEST=1 with GEMINI_API_KEY
// and GEMINI_MODEL.

// Three paid calls.
func TestLiveAgentTurn(t *testing.T) {
	client, modelName := liveAgentClient(t)
	providertest.AgentTurn(t, "gemini", client, modelName)
}

// One paid call.
func TestLiveStructuredOutput(t *testing.T) {
	client, modelName := liveAgentClient(t)
	providertest.StructuredOutput(t, client, modelName)
}

// One paid call.
func TestLiveRecordedReminder(t *testing.T) {
	client, modelName := liveAgentClient(t)
	providertest.RecordedReminder(t, client, modelName)
}

// One paid call.
func TestLiveIteratorAgentTurn(t *testing.T) {
	client, modelName := liveAgentClient(t)
	providertest.IteratorTurn(t, client, modelName)
}

// Two paid calls.
func TestLiveReservedFinalAnswer(t *testing.T) {
	client, modelName := liveAgentClient(t)
	providertest.ReservedFinalAnswer(t, client, modelName)
}

// Two paid calls.
func TestLiveSteerAfterToolBatch(t *testing.T) {
	client, modelName := liveAgentClient(t)
	providertest.SteerAfterToolBatch(t, client, modelName)
}
