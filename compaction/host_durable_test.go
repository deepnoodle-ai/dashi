package compaction_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

// durableHost keeps a conversation's turns in a recorder, as a host with a
// database does, and admits compaction turns under the command ID
// StartIfNeeded names, which is also their turn ID.
type durableHost struct {
	t         *testing.T
	client    *dashitest.Client
	rec       *dashi.MemoryRecorder
	inbox     *dashi.MemoryInbox
	runner    dashi.Runner
	compactor *compaction.Compactor
	ids       []string // the conversation's turn IDs, oldest first
}

func newDurableHost(t *testing.T, window int) *durableHost {
	client := dashitest.NewClient()
	rec := dashi.NewMemoryRecorder()
	inbox := dashi.NewMemoryInbox()
	policy := compaction.Policy{}
	policy.Model.Capabilities.ContextWindow = window
	return &durableHost{
		t:         t,
		client:    client,
		rec:       rec,
		inbox:     inbox,
		runner:    dashi.Runner{Client: client, Recorder: rec, Inbox: inbox},
		compactor: &compaction.Compactor{Policy: policy, Model: dashitest.Model},
	}
}

func (h *durableHost) turns() []dashi.Turn {
	h.t.Helper()
	var out []dashi.Turn
	for _, id := range h.ids {
		turn, err := h.rec.Load(h.t.Context(), id)
		if err != nil {
			h.t.Fatal(err)
		}
		out = append(out, turn)
	}
	return out
}

// work runs a work turn whose model call answers with reply.
func (h *durableHost) work(text string, reply llm.Result) dashi.RunResult {
	h.t.Helper()
	h.client.Add(reply)
	history, _ := dashi.BuildHistory(h.turns())
	id := fmt.Sprintf("conv-%d", len(h.ids)+1)
	out, err := h.runner.Run(h.t.Context(), dashi.NewTurn(id), dashi.Start{
		CommandID: "run:" + id,
		Agent:     dashi.Agent{Model: dashitest.Model},
		History:   history,
		Input:     []llm.Message{llm.UserMessage(llm.Text(text))},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.ids = append(h.ids, id)
	return out
}

// admit records the compaction turn StartIfNeeded asks for, under its
// command ID, and returns it, or "" when none is needed.
func (h *durableHost) admit(turns []dashi.Turn) string {
	h.t.Helper()
	start, needed, err := h.compactor.StartIfNeeded(h.t.Context(), turns)
	if err != nil {
		h.t.Fatal(err)
	}
	if !needed {
		return ""
	}
	id := start.CommandID
	if _, err := h.runner.Record(h.t.Context(), dashi.NewTurn(id), start); err != nil {
		h.t.Fatal(err)
	}
	if !slices.Contains(h.ids, id) {
		h.ids = append(h.ids, id)
	}
	return id
}

// usedText is a reply whose model call used tokens of context.
func usedText(text string, tokens int) llm.Result {
	r := dashitest.Reply(text)
	r.Usage = &llm.Usage{InputTokens: tokens}
	return r
}

// pad is a user message of about n tokens.
func pad(label string, n int) string {
	return label + " " + strings.Repeat("x", 4*n)
}

func TestStartIfNeededRules(t *testing.T) {
	h := newDurableHost(t, 10_000) // due at 2,000 tokens
	need := func(want bool) dashi.Start {
		t.Helper()
		start, needed, err := h.compactor.StartIfNeeded(t.Context(), h.turns())
		if err != nil || needed != want {
			t.Fatalf("needed=%v err=%v; want %v", needed, err, want)
		}
		return start
	}
	need(false) // no turns
	h.work(pad("one", 700), usedText("first answer", 750))
	need(false) // below the due count
	h.work(pad("two", 700), usedText("second answer", 2_000))
	calls := len(h.client.Inputs())
	start := need(true)
	if start.CommandID != "compact:conv-2" || start.Origin != dashi.StartCompaction ||
		start.Compaction.SummaryFallback == "" || len(h.client.Inputs()) != calls {
		t.Fatalf("start: id=%q origin=%q fallback=%q calls=%d",
			start.CommandID, start.Origin, start.Compaction.SummaryFallback, len(h.client.Inputs()))
	}
	again := need(true)
	if a, _ := json.Marshal(start); string(a) != mustJSON(t, again) {
		t.Fatal("a second call built another Start")
	}

	// A compaction turn that has not ended is the last turn: not needed.
	id := h.admit(h.turns())
	need(false)
	h.client.Add(dashitest.Reply("<summary>S</summary>"))
	if out, err := h.runner.Advance(t.Context(), id); err != nil || !out.Stop.Ended {
		t.Fatalf("advance: %+v %v", out.Stop, err)
	}
	need(false) // after a compaction turn

	// The last call's usage is far above the history: the tool
	// definitions alone would make the replacement due.
	h.work(pad("three", 700), usedText("third answer", 9_000))
	need(false)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Failure 4: a crash at any point leaves either no compaction turn, and
// the next admission builds the same one, or one whose replay is already
// decided. A second worker meets the same key, and the first record wins.
func TestDurableCompactionCrashAndSecondWorker(t *testing.T) {
	h := newDurableHost(t, 10_000)
	h.work(pad("one", 700), usedText("first answer", 750))
	h.work(pad("two", 700), usedText("second answer", 2_000))
	before := h.turns()

	// Admission records the Start; the worker crashes before driving it.
	id := h.admit(before)
	recorded := h.turns()[2]

	// A second worker, which read the turns before the first recorded,
	// admits the same key: the record is unchanged.
	if again := h.admit(before); again != id {
		t.Fatalf("second worker admitted %q, want %q", again, id)
	}
	if got := h.turns(); len(got) != 3 || mustJSON(t, got[2]) != mustJSON(t, recorded) {
		t.Fatalf("the second admission changed the record: %d turns", len(got))
	}

	// The worker crashes in the middle of the summary call.
	release := make(chan struct{})
	h.client.Hold(release)
	h.client.Add(dashitest.Reply("<summary>Two prompts were answered.</summary>"))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := h.runner.Drive(ctx, id)
		done <- err
	}()
	for !sent(h.rec, t, id) {
		runtime.Gosched() // wait until the summary call is in flight
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("drive: %v", err)
	}
	if _, needed, _ := h.compactor.StartIfNeeded(t.Context(), h.turns()); needed {
		t.Fatal("needed while the compaction turn has not ended")
	}

	// A restarted worker resumes it.
	close(release)
	out, err := h.runner.Drive(t.Context(), id)
	if err != nil || !out.Stop.Ended {
		t.Fatalf("resume: %+v %v", out.Stop, err)
	}
	history, warnings := dashi.BuildHistory(h.turns())
	if len(warnings) != 0 || len(history) != 1 ||
		!strings.Contains(history[0].Text(), "Two prompts were answered.") {
		t.Fatalf("history=%d warnings=%v", len(history), warnings)
	}
}

// sent reports whether turn id has a model call started.
func sent(rec *dashi.MemoryRecorder, t *testing.T, id string) bool {
	turn, err := rec.Load(t.Context(), id)
	if err != nil {
		return false
	}
	for _, s := range turn.Steps {
		if s.Kind == dashi.StepModelCallStarted {
			return true
		}
	}
	return false
}

// Failure 1: the summary call fails. The host closes the stopped turn with
// Cancel, and replay uses its summary fallback: the user's messages, the
// tools called, and the last reply, shorter than the history, with a
// warning. The next turn is not due again.
func TestDurableCompactionFailureUsesFallback(t *testing.T) {
	h := newDurableHost(t, 100_000) // due at 20,000 tokens
	h.runner.Tools = tool.List{tool.MustFunc("send_email", "Send an email.",
		func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
			return tool.Succeeded("sent"), nil
		})}
	h.client.Add(dashitest.CallTool("c1", "send_email", `{}`))
	h.work(pad("Email Ana the report.", 7_000), usedText("I emailed Ana.", 7_500))
	h.work(pad("Now summarize it.", 7_000), usedText("The report says 42.", 20_000))
	full, _ := dashi.BuildHistory(h.turns())

	id := h.admit(h.turns())
	// No scripted reply is left, so the summary call fails.
	out, err := h.runner.Advance(t.Context(), id)
	if err != nil || out.Stop.Ended || out.Next() != dashi.NextFix {
		t.Fatalf("advance: %+v next=%s err=%v", out.Stop, out.Next(), err)
	}
	if _, needed, _ := h.compactor.StartIfNeeded(t.Context(), h.turns()); needed {
		t.Fatal("needed while the failed compaction turn has not ended")
	}
	if err := h.inbox.Put(id, dashi.Cancel{CommandID: "close:" + id}); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runner.Advance(t.Context(), id); err != nil || !out.Stop.Ended {
		t.Fatalf("close: %+v %v", out.Stop, err)
	}

	history, warnings := dashi.BuildHistory(h.turns())
	if len(warnings) != 1 || warnings[0].Code != dashi.WarningSummaryFallback ||
		warnings[0].TurnID != id {
		t.Fatalf("warnings=%v", warnings)
	}
	if len(history) != 1 || !compaction.IsSummary(history[0]) {
		t.Fatalf("history=%d messages", len(history))
	}
	text := history[0].Text()
	for _, want := range []string{"Email Ana the report.", "send_email", "Now summarize it.", "The report says 42."} {
		if !strings.Contains(text, want) {
			t.Fatalf("fallback lacks %q:\n%s", want, text)
		}
	}
	if len(text) >= len(mustJSON(t, full))/2 {
		t.Fatalf("fallback is %d bytes; history was %d", len(text), len(mustJSON(t, full)))
	}
	h.work("And now?", usedText("Done.", 4_000))
	if _, needed, _ := h.compactor.StartIfNeeded(t.Context(), h.turns()); needed {
		t.Fatal("compaction repeats after the fallback")
	}
}

// StartIfNeeded and a Session follow one rule: over the same turns, the
// Session runs a compaction turn exactly when StartIfNeeded says one is
// needed.
func TestStartIfNeededMatchesSession(t *testing.T) {
	lookup := tool.MustFunc("lookup", "Look something up.",
		func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
			return tool.Succeeded("found"), nil
		})
	call := dashitest.CallTool("c1", "lookup", `{}`)
	call.Usage = &llm.Usage{InputTokens: 2_000}
	for _, tc := range []struct {
		name    string
		replies []llm.Result
		limits  dashi.Limits
		compact bool // end with a manual compaction turn
		want    bool
	}{
		{name: "below the due count",
			replies: []llm.Result{usedText("a", 750), usedText("b", 1_500)}},
		{name: "at the due count", want: true,
			replies: []llm.Result{usedText("a", 750), usedText("b", 2_000)}},
		{name: "after a compaction turn", compact: true,
			replies: []llm.Result{usedText("a", 750), usedText("b", 2_000), dashitest.Reply("<summary>S</summary>")}},
		{name: "replacement would be due",
			replies: []llm.Result{usedText("a", 750), usedText("b", 9_000)}},
		{name: "ended at a limit", want: true, limits: dashi.Limits{ModelCalls: 1, NoRaise: true},
			replies: []llm.Result{usedText("a", 750), call}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := compaction.Policy{}
			policy.Model.Capabilities.ContextWindow = 10_000
			rec := dashi.NewMemoryRecorder()
			client := dashitest.NewClient(tc.replies...)
			s := &session.Session{
				ID:     "s",
				New:    true,
				Runner: dashi.Runner{Client: client, Recorder: rec, Tools: tool.List{lookup}},
				Agent:  dashi.Agent{Model: dashitest.Model, Limits: tc.limits},
			}
			for _, text := range []string{pad("one", 700), pad("two", 700)} {
				if _, err := s.Send(t.Context(), text); err != nil {
					t.Fatal(err)
				}
			}
			n := 2
			if tc.compact {
				s.Compactor = compaction.New(policy)
				if _, err := s.Compact(t.Context(), nil); err != nil {
					t.Fatal(err)
				}
				n = 3
			}
			var turns []dashi.Turn
			for i := 1; i <= n; i++ {
				turn, err := rec.Load(t.Context(), session.TurnID("s", i))
				if err != nil {
					t.Fatal(err)
				}
				turns = append(turns, turn)
			}
			c := compaction.New(policy)
			_, needed, err := c.StartIfNeeded(t.Context(), turns)
			if err != nil || needed != tc.want {
				t.Fatalf("StartIfNeeded: needed=%v err=%v; want %v", needed, err, tc.want)
			}

			client.Add(dashitest.Reply("<summary>S</summary>"), dashitest.Reply("next"))
			reader := &session.Session{
				ID:        "s",
				Runner:    dashi.Runner{Client: client, Recorder: rec},
				Agent:     dashi.Agent{Model: dashitest.Model},
				Compactor: c,
			}
			if _, err := reader.Send(t.Context(), "next"); err != nil {
				t.Fatal(err)
			}
			turn, err := rec.Load(t.Context(), session.TurnID("s", n+1))
			if err != nil {
				t.Fatal(err)
			}
			ran := turn.Steps[0].Started.Origin == dashi.StartCompaction
			if ran != needed {
				t.Fatalf("session compacted=%v, StartIfNeeded needed=%v", ran, needed)
			}
		})
	}
}
