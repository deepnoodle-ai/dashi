package selective_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/selective"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

const testError = "FAIL: unexpected token ) at offset 16"

func textTool(t *testing.T, name, text string) tool.Tool {
	t.Helper()
	tl, err := tool.Func(name, "Returns fixed text.",
		func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
			return tool.Succeeded(text), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

// A session with the selective Compactor records a compaction turn whose
// replacement history keeps the needed tool exchange word for word, and
// every reader, a fresh handle with no compactor or BuildHistory over the
// stored turns, builds the same history.
func TestSessionCompaction(t *testing.T) {
	ctx := context.Background()
	logText := strings.Repeat("2026-10-02T10:00:00Z INFO worker heartbeat ok\n", 70)
	docs := strings.Repeat("The grammar allows trailing commas in calls. ", 26)

	full := dashitest.Reply("Read the docs.")
	full.Usage = &llm.Usage{InputTokens: 3_500} // over 80% of the window
	client := dashitest.NewClient(
		dashitest.CallTool("c1", "read_log", `{}`),
		dashitest.Reply("Read the log."),
		dashitest.CallTool("c2", "run_tests", `{}`),
		dashitest.Reply("The tests fail at offset 16."),
		dashitest.CallTool("c3", "read_docs", `{}`),
		full,
		dashitest.Reply("<summary>The log showed only heartbeats.</summary>"),
		dashitest.Reply("Fixed the parser."),
	)
	// The last turn, from "Read the grammar docs." on, is larger than the
	// 400-token tail share, but the stock tail keeps it, so it stays whole
	// as the tail. The segments, in order: s0 = call c1 and the log, s1 =
	// "Read the log.", s2 = call c2 and the failure, s3 = "The tests fail
	// at offset 16.". Unanswered questions get 0.5, which is not above 0.5,
	// so those segments are dropped.
	srv := decidetest.NewServer(t)
	srv.Answer("s2.needed", decidetest.NoulAnswer(0.95))
	srv.Answer("s0.needed", decidetest.NoulAnswer(0.05))
	var selections []selective.Selection
	compactor := &selective.Compactor{
		Client:      srv.Client(t, decide.WithMaxRetries(0)),
		OnSelection: func(s selective.Selection) { selections = append(selections, s) },
	}
	compactor.Base.Policy.Model.Capabilities.ContextWindow = 4_000

	rec := dashi.NewMemoryRecorder()
	runner := dashi.Runner{
		Client:   client,
		Recorder: rec,
		Tools: tool.List{
			textTool(t, "read_log", logText),
			textTool(t, "run_tests", testError),
			textTool(t, "read_docs", docs),
		},
	}
	s := &session.Session{
		ID:        "fix",
		New:       true,
		Runner:    runner,
		Agent:     dashi.Agent{Model: dashitest.Model},
		Compactor: compactor,
	}
	for _, text := range []string{
		"Fix the parser tests. Do not change exported signatures.",
		"Run the tests.",
		"Read the grammar docs.",
		"Now fix it.",
	} {
		out, err := s.Send(ctx, text)
		if err != nil {
			t.Fatal(err)
		}
		if len(out.SessionWarnings) > 0 {
			t.Fatalf("warnings: %+v", out.SessionWarnings)
		}
	}

	var turns []dashi.Turn
	var compactionTurn *dashi.TurnView
	for turn, err := range s.Turns(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		turns = append(turns, turn)
		if view, _ := turn.View(); view.Origin == dashi.StartCompaction {
			compactionTurn = &view
		}
	}
	if compactionTurn == nil {
		t.Fatal("no compaction turn was recorded")
	}
	if len(selections) != 1 || selections[0].Fallback != "" {
		t.Fatalf("selections = %+v", selections)
	}
	var acts []selective.Action
	for _, seg := range selections[0].Segments {
		acts = append(acts, seg.Action)
	}
	drop, keep := selective.ActionDrop, selective.ActionKeep
	want := []selective.Action{drop, drop, keep, drop}
	if !slices.Equal(acts, want) {
		t.Fatalf("actions = %v, want %v", acts, want)
	}
	if selections[0].Hash != compactionTurn.Compaction.Hash {
		t.Fatal("the selection's hash does not match the compaction turn's")
	}

	next, err := s.NextHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	text := allText(next)
	if !strings.Contains(text, testError) {
		t.Fatalf("the needed failure is not kept word for word:\n%s", text)
	}
	if !strings.Contains(text, docs) {
		t.Fatal("the last turn's docs are not kept word for word")
	}
	if strings.Contains(text, "heartbeat ok\n2026") {
		t.Fatal("the dropped log is still in the history")
	}
	summaries := 0
	for _, m := range next {
		if compaction.IsSummary(m) {
			summaries++
		}
	}
	if summaries != 1 {
		t.Fatalf("history has %d summaries", summaries)
	}

	reader := &session.Session{ID: "fix", Runner: dashi.Runner{Client: client, Recorder: rec}}
	again, err := reader.NextHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	built, warnings := dashi.BuildHistory(turns)
	if len(warnings) > 0 {
		t.Fatalf("BuildHistory warnings: %+v", warnings)
	}
	a, _ := json.Marshal(next)
	for name, other := range map[string][]llm.Message{"fresh handle": again, "BuildHistory": built} {
		// BuildHistory includes the last turn, which NextHistory also does.
		if b, _ := json.Marshal(other); string(a) != string(b) {
			t.Fatalf("%s builds a different history", name)
		}
	}
}

func allText(msgs []llm.Message) string {
	b, _ := json.Marshal(msgs)
	var s string
	for _, m := range msgs {
		for _, c := range m.Content {
			if c.ToolResult != nil {
				for _, bl := range c.ToolResult.Output.Content {
					if bl.Text != nil {
						s += bl.Text.Text + "\n"
					}
				}
			}
		}
		s += m.Text() + "\n"
	}
	return s + string(b)
}
