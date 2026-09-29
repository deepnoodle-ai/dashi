//go:build live

package compaction

import (
	"context"
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
	runner := dashi.Runner{
		Agent: dashi.Agent{
			Model:    model,
			Settings: llm.Settings{MaxTokens: 2_000},
		},
		Client: client,
		Tools:  tool.List{note},
	}
	var history []llm.Message
	var starts []int
	var last llm.Usage
	ask := func(prompt string) dashi.Outcome {
		t.Helper()
		out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
			History: history,
			Input:   []llm.Message{llm.UserMessage(llm.Text(prompt))},
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.Stop.Kind != dashi.StopCompleted {
			t.Fatalf("turn stopped %s: %s", out.Stop.Kind, out.Stop.Reason)
		}
		msgs, err := dashi.Project(out.Turn)
		if err != nil {
			t.Fatal(err)
		}
		starts = append(starts, len(history))
		history = append(history, msgs...)
		last = out.Spend.Usage
		return out
	}
	ask("Call read_note, then tell me in one sentence what it says.")
	ask("Name one prime number greater than 100. Answer in one sentence.")
	ask("Name one planet with rings. Answer in one sentence.")

	sum := NewSummarizer(client, SummarizerConfig{Model: model})
	rec, err := Compact(ctx, sum, history, nil, Options{
		Policy:       Policy{},
		TurnStarts:   starts,
		Last:         last,
		TailFraction: 1e-9, // keep no Turns, so only the summary holds the fact
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("summary (%s, %d output tokens, cost %v):\n%s",
		rec.Model, rec.Usage.OutputTokens, rec.Cost, rec.Summary)
	view, err := Project(history, rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 1 {
		t.Fatalf("view has %d messages, want the summary alone", len(view))
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		History: view,
		Input: []llm.Message{llm.UserMessage(llm.Text(
			"What was the release codename? Answer with the codename only.",
		))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Answer, "PELICAN-4471") {
		t.Fatalf("answer %q lacks the codename", out.Answer)
	}
}
