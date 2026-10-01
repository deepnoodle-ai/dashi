//go:build live

package compaction

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/internal/providertest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
)

// TestLiveCompactionContinuity makes about six paid calls. A fact enters the
// history through a tool result. After compaction, the fact is only in the
// summary, and the agent must still answer from it. Set
// DASHI_LIVE_AGENT_TEST=1, OPENAI_API_KEY, and OPENAI_MODEL.
func TestLiveCompactionContinuity(t *testing.T) {
	key, model := providertest.AgentCredentials(t, "OPENAI")
	client, err := responses.New(responses.Config{
		APIKey:           key,
		CapabilityPolicy: llm.CapabilityAdjust,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	note, err := tool.Func(
		"read_note",
		"Reads the release note.",
		func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
			return tool.Succeeded("Release codename: PELICAN-4471."), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	agent := dashi.Agent{
		Model:    model,
		Settings: llm.Settings{MaxTokens: 2_000},
	}
	runner := dashi.Runner{
		Client: client,
		Tools:  tool.List{note},
	}
	var turns []dashi.Turn
	run := func(start dashi.Start) dashi.RunResult {
		t.Helper()
		id := fmt.Sprintf("live-%d", len(turns)+1)
		start.CommandID = "run:" + id
		out, err := runner.Run(ctx, dashi.NewTurn(id), start)
		if err != nil {
			t.Fatal(err)
		}
		if out.Stop.Kind != dashi.StopCompleted {
			t.Fatalf("turn stopped %s: %s", out.Stop.Kind, out.Stop.Reason)
		}
		turns = append(turns, out.Turn)
		return out
	}
	ask := func(prompt string) dashi.RunResult {
		t.Helper()
		history, warnings := dashi.ProjectHistory(turns)
		if len(warnings) > 0 {
			t.Fatalf("warnings: %+v", warnings)
		}
		return run(dashi.Start{
			Agent:   agent,
			History: history,
			Input:   []llm.Message{llm.UserMessage(llm.Text(prompt))},
		})
	}
	ask("Call read_note, then tell me in one sentence what it says.")
	ask("Name one prime number greater than 100. Answer in one sentence.")
	ask("Name one planet with rings. Answer in one sentence.")

	// Keep no turns, so only the summary holds the fact.
	c := &Compactor{Model: model, TailFraction: -1}
	history, _ := dashi.ProjectHistory(turns)
	start, err := c.Start(history)
	if err != nil {
		t.Fatal(err)
	}
	sum := run(start)
	t.Logf("summary (%d output tokens, cost %v):\n%s",
		sum.Spend.Usage.OutputTokens, sum.Spend.Cost, sum.Reply)
	view, _ := dashi.ProjectHistory(turns)
	if len(view) != 1 {
		t.Fatalf("view has %d messages, want the summary alone", len(view))
	}
	out := ask("What was the release codename? Answer with the codename only.")
	if !strings.Contains(out.Reply, "PELICAN-4471") {
		t.Fatalf("answer %q lacks the codename", out.Reply)
	}
}
