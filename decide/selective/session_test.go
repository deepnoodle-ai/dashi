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
// stored turns, builds the same history. A second compaction summarizes
// the first summary again and judges the segments the first one kept.
func TestSessionCompaction(t *testing.T) {
	ctx := context.Background()
	logText := strings.Repeat("2026-10-02T10:00:00Z INFO worker heartbeat ok\n", 70)
	docs := strings.Repeat("The grammar allows trailing commas in calls. ", 26)

	full := dashitest.Reply("Read the docs.")
	full.Usage = &llm.Usage{InputTokens: 3_500} // over 80% of the window
	full2 := dashitest.Reply("The tree is clean.")
	full2.Usage = &llm.Usage{InputTokens: 3_500}
	client := dashitest.NewClient(
		dashitest.CallTool("c1", "read_log", `{}`),
		dashitest.Reply("Read the log."),
		dashitest.CallTool("c2", "run_tests", `{}`),
		dashitest.Reply("The tests fail at offset 16."),
		dashitest.CallTool("c3", "read_docs", `{}`),
		full,
		dashitest.Reply("<summary>The log showed only heartbeats.</summary>"),
		dashitest.Reply("Fixed the parser."),
		dashitest.CallTool("c4", "git_status", `{}`),
		full2,
		dashitest.Reply("<summary>Second notes.</summary>"),
		dashitest.Reply("Done."),
	)
	// A segment that holds the failure is needed; any other is not.
	srv := decidetest.NewServer(t)
	srv.Respond(func(req *decide.Request) (*decide.Response, error) {
		resp := &decide.Response{Answers: map[string]decide.Answer{}, Model: "jev-1.13.0"}
		for key, q := range req.Questions {
			b, _ := json.Marshal(q)
			p := 0.05
			if strings.Contains(string(b), "unexpected token") {
				p = 0.95
			}
			resp.Answers[key] = decidetest.NoulAnswer(p)
		}
		return resp, nil
	})
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
			textTool(t, "git_status", "clean"),
		},
	}
	s := &session.Session{
		ID:     "fix",
		New:    true,
		Runner: runner,
		Agent:  dashi.Agent{Model: dashitest.Model},
	}
	send := func(texts ...string) {
		t.Helper()
		for _, text := range texts {
			out, err := s.Send(ctx, text)
			if err != nil {
				t.Fatal(err)
			}
			if len(out.SessionWarnings) > 0 {
				t.Fatalf("warnings: %+v", out.SessionWarnings)
			}
		}
	}
	send(
		"Fix the parser tests. Do not change exported signatures.",
		"Run the tests.",
		"Read the grammar docs.",
	)
	// Seed the intended comparison before enabling automatic compaction.
	// Projected-history sizing can otherwise compact these long fixtures early.
	s.Compactor = compactor
	send("Now fix it.")

	// The last turn, from "Read the grammar docs." on, is larger than the
	// 400-token tail share, but the stock tail keeps it, so it stays whole
	// as the tail. The segments, in order: s0 = call c1 and the log, s1 =
	// "Read the log.", s2 = call c2 and the failure, s3 = "The tests fail
	// at offset 16.".
	if len(selections) != 1 || selections[0].Fallback != "" {
		t.Fatalf("selections = %+v", selections)
	}
	drop, keep := selective.ActionDrop, selective.ActionKeep
	if got, want := actions(selections[0]), []selective.Action{drop, drop, keep, drop}; !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
	compactions := compactionTurns(t, s)
	if len(compactions) != 1 || selections[0].Hash != compactions[0].Compaction.Hash {
		t.Fatal("the selection's hash does not match the compaction turn's")
	}
	next := sameEverywhere(t, s, client, rec)
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
	if n := summaries(next); n != 1 {
		t.Fatalf("history has %d summaries", n)
	}

	// The second compaction judges the failure, kept the first time, again,
	// and the docs turn, now older, too, and summarizes the first summary.
	s.Compactor = nil
	send("Check the tree.")
	s.Compactor = compactor
	send("Wrap up.")
	if len(selections) != 2 || selections[1].Fallback != "" {
		t.Fatalf("selections = %+v", selections)
	}
	var judgedFailure, judgedDocs bool
	for _, seg := range selections[1].Segments {
		for _, i := range seg.MessageIndexes {
			if i >= len(next) { // the second compaction's input extends next
				continue
			}
			for _, c := range next[i].Content {
				if c.ToolCall != nil && c.ToolCall.ID == "c2" {
					judgedFailure = seg.Action == keep
				}
				if c.ToolCall != nil && c.ToolCall.ID == "c3" {
					judgedDocs = seg.Action == drop
				}
			}
		}
	}
	if !judgedFailure || !judgedDocs {
		t.Fatalf("second selection = %+v", selections[1].Segments)
	}
	compactions = compactionTurns(t, s)
	if len(compactions) != 2 {
		t.Fatalf("%d compaction turns", len(compactions))
	}
	request := compactions[1].Input[0].Text()
	if !strings.Contains(request, "The log showed only heartbeats.") || !strings.Contains(request, "trailing commas") {
		t.Fatalf("the second summary request lacks the first summary or the docs:\n%.2000s", request)
	}
	if strings.Contains(request, testError) {
		t.Fatal("the second summary request covers the kept failure")
	}
	last := sameEverywhere(t, s, client, rec)
	text = allText(last)
	if !strings.Contains(text, testError) || !strings.Contains(text, "Second notes.") ||
		strings.Contains(text, "The log showed only heartbeats.") {
		t.Fatalf("history after the second compaction:\n%.3000s", text)
	}
	if n := summaries(last); n != 1 {
		t.Fatalf("history has %d summaries", n)
	}
}

func actions(sel selective.Selection) []selective.Action {
	var out []selective.Action
	for _, seg := range sel.Segments {
		out = append(out, seg.Action)
	}
	return out
}

func summaries(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		if compaction.IsSummary(m) {
			n++
		}
	}
	return n
}

// compactionTurns returns the views of the session's compaction turns.
func compactionTurns(t *testing.T, s *session.Session) []dashi.TurnView {
	t.Helper()
	var out []dashi.TurnView
	for turn, err := range s.Turns(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
		if view, _ := turn.View(); view.Origin == dashi.StartCompaction {
			out = append(out, view)
		}
	}
	return out
}

// sameEverywhere returns the session's next history and checks that a
// fresh handle with no compactor and BuildHistory over the stored turns
// build the same one.
func sameEverywhere(t *testing.T, s *session.Session, client llm.Client, rec dashi.Recorder) []llm.Message {
	t.Helper()
	ctx := context.Background()
	next, err := s.NextHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var turns []dashi.Turn
	for turn, err := range s.Turns(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		turns = append(turns, turn)
	}
	reader := &session.Session{ID: s.ID, Runner: dashi.Runner{Client: client, Recorder: rec}}
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
	return next
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
