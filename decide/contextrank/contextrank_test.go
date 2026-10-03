package contextrank_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/contextrank"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

// judge starts a fake Decide server that answers each question with the
// P(yes) of a marker in scores found in the question's item text, or
// leaves it unanswered when that P(yes) is negative.
// Items with no marker get 0.05.
func judge(t *testing.T, scores map[string]float64) (*decidetest.Server, *decide.Client) {
	t.Helper()
	srv := decidetest.NewServer(t)
	srv.Respond(func(req *decide.Request) (*decide.Response, error) {
		resp := &decide.Response{
			Answers:   map[string]decide.Answer{},
			RequestID: "req_1",
			Usage:     decide.Usage{InputTokens: 100, OutputTokens: 2 * len(req.Questions)},
		}
		for key, q := range req.Questions {
			instructions, _ := q.(*decide.NoulQuestion).Instructions.(string)
			_, text, _ := strings.Cut(instructions, "The item:\n")
			p := 0.05
			for marker, s := range scores {
				if strings.Contains(text, marker) {
					p = s
				}
			}
			if p < 0 {
				continue // leave the question unanswered
			}
			resp.Answers[key] = decidetest.NoulAnswer(p)
		}
		return resp, nil
	})
	return srv, srv.Client(t)
}

func fixed(cs ...contextrank.Candidate) func(context.Context, string) ([]contextrank.Candidate, error) {
	return func(context.Context, string) ([]contextrank.Candidate, error) {
		return cs, nil
	}
}

// record collects every Selection.
type record struct {
	mu   sync.Mutex
	sels []contextrank.Selection
}

func (r *record) add(s contextrank.Selection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sels = append(r.sels, s)
}

func (r *record) all() []contextrank.Selection {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.sels)
}

func causes(sel contextrank.Selection) map[string]contextrank.Cause {
	out := map[string]contextrank.Cause{}
	for _, j := range sel.Candidates {
		out[j.ID] = j.Cause
	}
	return out
}

// sent returns the host IDs of the context items in a model input, in order.
func sent(in llm.Input) []string {
	var ids []string
	for _, m := range in.Messages {
		for _, c := range m.Content {
			if c.Kind != llm.ContentReminder || !strings.HasPrefix(c.Reminder.Name, "context-") {
				continue
			}
			id, _, _ := strings.Cut(strings.TrimPrefix(c.Reminder.Content, "Context item "), ",")
			ids = append(ids, id)
		}
	}
	return ids
}

func start(text string) dashi.Start {
	return dashi.Start{
		Agent: dashi.Agent{Model: dashitest.Model},
		Input: []llm.Message{llm.UserMessage(llm.Text(text))},
	}
}

func hookResult(turn dashi.Turn) (dashi.HookResult, bool) {
	for _, s := range turn.Steps {
		for _, h := range s.HookResults {
			if h.Name == contextrank.HookName {
				return h, true
			}
		}
	}
	return dashi.HookResult{}, false
}

func hasWarning(ws []dashi.Warning, code dashi.WarningCode) bool {
	return slices.ContainsFunc(ws, func(w dashi.Warning) bool { return w.Code == code })
}

// note is a candidate whose content is padded to size bytes, so budgets
// are easy to reason about.
func note(id string, size int) contextrank.Candidate {
	text := id + " text"
	return contextrank.Candidate{ID: id, Text: text, Content: text + strings.Repeat(".", size-len(text))}
}

func TestSelectionUnderBudget(t *testing.T) {
	srv, client := judge(t, map[string]float64{
		"seat": 0.91, "passport": 0.84, "big": 0.80, "hotel": 0.62, "gym": 0.30,
	})
	// With its header line, each 100-byte note is about 200 bytes. The
	// budget fits seat and passport, not big, and then still fits hotel.
	var rec record
	ranker := &contextrank.Ranker{
		Client: client,
		Source: fixed(
			note("gym", 100), note("hotel", 100), note("big", 400),
			note("seat", 100), note("passport", 100),
		),
		MaxBytes:    3*200 + 30,
		OnSelection: rec.add,
	}
	model := dashitest.NewClient(dashitest.Reply("Booked."))
	runner := dashi.Runner{Client: model, Hooks: ranker.Hook().Hooks()}
	out, err := runner.Run(t.Context(), dashi.Turn{}, start("Book my flight to Tokyo"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sent(model.Inputs()[0]), []string{"seat", "passport", "hotel"}; !slices.Equal(got, want) {
		t.Fatalf("model saw %v; want %v", got, want)
	}
	last := model.Inputs()[0].Messages
	if last[0].Text() != "Book my flight to Tokyo" {
		t.Fatalf("first message = %q; items must follow the user's message", last[0].Text())
	}

	sels := rec.all()
	if len(sels) != 1 {
		t.Fatalf("selections = %d; want 1", len(sels))
	}
	sel := sels[0]
	want := map[string]contextrank.Cause{
		"seat": contextrank.CauseChosen, "passport": contextrank.CauseChosen,
		"hotel": contextrank.CauseChosen, "big": contextrank.CauseOverBudget,
		"gym": contextrank.CauseBelowMinimum,
	}
	if got := causes(sel); !mapsEqual(got, want) {
		t.Fatalf("causes = %v; want %v", got, want)
	}
	if sel.TurnID != out.ID || sel.Model != "jev-1.13.0" || sel.Err != nil {
		t.Fatalf("selection = %+v", sel)
	}
	if !slices.Equal(sel.RequestIDs, []string{"req_1"}) || sel.Usage.InputTokens == 0 {
		t.Fatalf("request IDs %v, usage %+v", sel.RequestIDs, sel.Usage)
	}
	if len(sel.Added) != 3 || sel.Added[0].Name != contextrank.ItemName("seat") ||
		sel.Added[0].Origin != dashi.ContextRuntime || !sel.Added[0].Snapshot {
		t.Fatalf("added = %+v", sel.Added)
	}

	h, ok := hookResult(out.Turn)
	if !ok {
		t.Fatal("no hook result recorded")
	}
	wantReason := "chose 3 of 5 (seat 0.91, passport 0.84, hotel 0.62); request req_1"
	if h.Verdict != dashi.VerdictChange || h.Reason != wantReason || h.RulesVersion != "jev-1.13.0" {
		t.Fatalf("hook result = %+v; want reason %q", h, wantReason)
	}
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("Decide requests = %d; want 1", n)
	}
}

func mapsEqual[K comparable, V comparable](a, b map[K]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func TestThreshold(t *testing.T) {
	cases := []struct {
		name string
		min  float64
		want []string
	}{
		{"zero means above 0.5", 0, []string{"high"}},
		{"negative means no floor", -1, []string{"high", "even", "low"}},
		{"positive is inclusive", 0.5, []string{"high", "even"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, client := judge(t, map[string]float64{"high": 0.7, "even": 0.5, "low": 0.2})
			var rec record
			ranker := &contextrank.Ranker{
				Client:         client,
				Source:         fixed(note("low", 50), note("even", 50), note("high", 50)),
				MinProbability: tc.min,
				OnSelection:    rec.add,
			}
			model := dashitest.NewClient(dashitest.Reply("ok"))
			runner := dashi.Runner{Client: model, Hooks: ranker.Hook().Hooks()}
			if _, err := runner.Run(t.Context(), dashi.Turn{}, start("hi")); err != nil {
				t.Fatal(err)
			}
			if got := sent(model.Inputs()[0]); !slices.Equal(got, tc.want) {
				t.Fatalf("model saw %v; want %v", got, tc.want)
			}
		})
	}
}

func TestUnansweredDropsOnlyThatCandidate(t *testing.T) {
	_, client := judge(t, map[string]float64{"a": 0.9, "b": -1})
	var rec record
	ranker := &contextrank.Ranker{
		Client:      client,
		Source:      fixed(note("a", 50), note("b", 50)),
		OnSelection: rec.add,
	}
	model := dashitest.NewClient(dashitest.Reply("ok"))
	runner := dashi.Runner{Client: model, Hooks: ranker.Hook().Hooks()}
	out, err := runner.Run(t.Context(), dashi.Turn{}, start("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if got := sent(model.Inputs()[0]); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("model saw %v", got)
	}
	if c := causes(rec.all()[0])["b"]; c != contextrank.CauseUnanswered {
		t.Fatalf("b = %q", c)
	}
	if hasWarning(out.Warnings, dashi.WarningHookFailed) {
		t.Fatal("an unanswered question failed the hook")
	}
}

func TestDuplicatesAcrossTurns(t *testing.T) {
	srv, client := judge(t, map[string]float64{"seat": 0.9, "passport": 0.8})
	cands := []contextrank.Candidate{note("seat", 60), note("passport", 60)}
	var rec record
	ranker := &contextrank.Ranker{
		Client: client,
		Source: func(context.Context, string) ([]contextrank.Candidate, error) {
			return slices.Clone(cands), nil
		},
		OnSelection: rec.add,
	}
	model := dashitest.NewClient(
		dashitest.Reply("Booked."), dashitest.Reply("Window seat."), dashitest.Reply("Updated."))
	s := &session.Session{
		Runner: dashi.Runner{Client: model, Hooks: ranker.Hook().Hooks()},
		Agent:  dashi.Agent{Model: dashitest.Model},
	}
	if _, err := s.Send(t.Context(), "Book my flight to Tokyo"); err != nil {
		t.Fatal(err)
	}
	// Turn 2: both items are still in view with the same content.
	out, err := s.Send(t.Context(), "Which seat?")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("Decide requests = %d; want 1, since every item is present", n)
	}
	if got := sent(model.Inputs()[1]); !slices.Equal(got, []string{"seat", "passport"}) {
		t.Fatalf("turn 2 model saw %v; want each item once", got)
	}
	sel := rec.all()[1]
	if c := causes(sel); c["seat"] != contextrank.CausePresent || c["passport"] != contextrank.CausePresent {
		t.Fatalf("turn 2 causes = %v", c)
	}
	if _, ok := hookResult(out.Turn); ok {
		t.Fatal("turn 2 recorded a change")
	}
	// The previous reply is part of the exchange.
	if st, _ := srv.Requests()[0].Request.State.(string); st != "User: Book my flight to Tokyo" {
		t.Fatalf("turn 1 state = %q", st)
	}

	// Turn 3: the seat note changed, so it is judged and added again.
	cands[0].Content = "seat text: aisle now"
	if _, err := s.Send(t.Context(), "Change my seat"); err != nil {
		t.Fatal(err)
	}
	reqs := srv.Requests()
	if len(reqs) != 2 || len(reqs[1].Request.Questions) != 1 {
		t.Fatalf("turn 3 asked %d requests; want one question about the changed note", len(reqs))
	}
	st, _ := reqs[1].Request.State.(string)
	if st != "Assistant: Window seat.\n\nUser: Change my seat" {
		t.Fatalf("turn 3 state = %q", st)
	}
	if got := sent(model.Inputs()[2]); !slices.Equal(got, []string{"seat", "passport", "seat"}) {
		t.Fatalf("turn 3 model saw %v; want the changed seat note appended", got)
	}
}

func TestStateKeepsTheEndOfLongMessages(t *testing.T) {
	srv, client := judge(t, map[string]float64{"owner": 0.9})
	ranker := &contextrank.Ranker{Client: client, Source: fixed(note("owner", 50))}
	model := dashitest.NewClient(dashitest.Reply("ok"))
	runner := dashi.Runner{Client: model, Hooks: ranker.Hook().Hooks()}
	trace := strings.Repeat("ERROR db timeout at handler.go:42\n", 300) // about 10 KB
	if _, err := runner.Run(t.Context(), dashi.Turn{}, start(trace+"Which service owns this?")); err != nil {
		t.Fatal(err)
	}
	st, _ := srv.Requests()[0].Request.State.(string)
	if len(st) > 5<<10 || !strings.HasSuffix(st, "Which service owns this?") {
		t.Fatalf("state is %d bytes and ends %q; want the question kept", len(st), st[max(0, len(st)-40):])
	}
}

func TestSkipsCompactionTurns(t *testing.T) {
	srv, client := judge(t, map[string]float64{"port": 0.9})
	ranker := &contextrank.Ranker{Client: client, Source: fixed(note("port", 60))}
	full := dashitest.Reply("Noted.")
	full.Usage = &llm.Usage{InputTokens: 900}
	model := dashitest.NewClient(
		full,
		dashitest.Reply("<summary>The project is Heron.</summary>"),
		dashitest.Reply("Port 8443."),
	)
	policy := compaction.Policy{}
	policy.Model.Capabilities.ContextWindow = 1_000
	compactor := compaction.New(policy)
	compactor.TailFraction = -1 // summarize everything, the item too
	s := &session.Session{
		Runner:    dashi.Runner{Client: model, Hooks: ranker.Hook().Hooks()},
		Agent:     dashi.Agent{Model: dashitest.Model},
		Compactor: compactor,
	}
	if _, err := s.Send(t.Context(), "My project Heron deploys on port 8443."); err != nil {
		t.Fatal(err)
	}
	out, err := s.Send(t.Context(), "Which port?")
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != session.TurnID(s.ID, 3) {
		t.Fatalf("turn = %s; want a compaction turn before it", out.ID)
	}
	inputs := model.Inputs()
	if len(inputs) != 3 {
		t.Fatalf("model calls = %d", len(inputs))
	}
	if got := sent(inputs[1]); len(got) != 0 {
		t.Fatalf("compaction call saw %v", got)
	}
	// Turn 1 and turn 3 rank; the compaction turn does not. The item from
	// turn 1 was summarized away, so turn 3 adds it again.
	if n := len(srv.Requests()); n != 2 {
		t.Fatalf("Decide requests = %d; want 2", n)
	}
	if got := sent(inputs[2]); !slices.Equal(got, []string{"port"}) {
		t.Fatalf("turn 3 saw %v", got)
	}
}

func TestChildTurns(t *testing.T) {
	for _, rank := range []bool{false, true} {
		srv, client := judge(t, map[string]float64{"a": 0.9})
		ranker := &contextrank.Ranker{Client: client, Source: fixed(note("a", 50)), RankChildTurns: rank}
		model := dashitest.NewClient(dashitest.Reply("done"))
		runner := dashi.Runner{Client: model, Hooks: ranker.Hook().Hooks()}
		st := start("Research flights")
		st.Parent = &dashi.CallRef{TurnID: "parent", CallID: "call_1"}
		if _, err := runner.Run(t.Context(), dashi.Turn{}, st); err != nil {
			t.Fatal(err)
		}
		want := 0
		if rank {
			want = 1
		}
		if n := len(srv.Requests()); n != want {
			t.Fatalf("RankChildTurns=%v: Decide requests = %d; want %d", rank, n, want)
		}
	}
}

func TestResumedTurnDoesNotRankAgain(t *testing.T) {
	srv, client := judge(t, map[string]float64{"a": 0.9})
	ranker := &contextrank.Ranker{Client: client, Source: fixed(note("a", 50))}
	model := dashitest.NewClient(dashitest.Reply("done"))
	// The worker loses the acknowledgement of the step that starts the
	// model call, after it committed with the added item: a crash.
	store := dashi.NewMemoryRecorder()
	rec := &dashitest.FaultRecorder{
		Recorder: store,
		Kind:     dashi.StepModelCallStarted,
		Fault:    dashitest.LoseAck,
	}
	runner := dashi.Runner{Client: model, Recorder: rec, Hooks: ranker.Hook().Hooks()}
	cmd := start("hi")
	cmd.CommandID = "start-1"
	if _, err := runner.Run(t.Context(), dashi.Turn{ID: "t1"}, cmd); err == nil {
		t.Fatal("the injected fault returned no error")
	}
	saved, err := store.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runner.Run(t.Context(), saved, dashi.Continue{CommandID: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Reply != "done" {
		t.Fatalf("reply = %q; stop %+v", out.Reply, out.Stop)
	}
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("Decide requests = %d; want 1", n)
	}
	for i, in := range model.Inputs() {
		if got := sent(in); !slices.Equal(got, []string{"a"}) {
			t.Fatalf("model call %d saw %v; want the item once", i+1, got)
		}
	}
}

func TestFailureAddsNothing(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*decidetest.Server, *contextrank.Ranker)
		want  string
	}{
		{"decide fails", func(srv *decidetest.Server, _ *contextrank.Ranker) {
			srv.FailNext(400) // not retried
		}, "contextrank: decide"},
		{"source fails", func(_ *decidetest.Server, r *contextrank.Ranker) {
			r.Source = func(context.Context, string) ([]contextrank.Candidate, error) {
				return nil, errors.New("index offline")
			}
		}, "index offline"},
		{"timeout", func(srv *decidetest.Server, r *contextrank.Ranker) {
			srv.SetLatency(time.Second)
			r.Timeout = 20 * time.Millisecond
		}, "deadline exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, client := judge(t, map[string]float64{"a": 0.9})
			var rec record
			ranker := &contextrank.Ranker{Client: client, Source: fixed(note("a", 50)), OnSelection: rec.add}
			tc.setup(srv, ranker)
			model := dashitest.NewClient(dashitest.Reply("done"))
			runner := dashi.Runner{Client: model, Hooks: ranker.Hook().Hooks()}
			began := time.Now()
			out, err := runner.Run(t.Context(), dashi.Turn{}, start("hi"))
			if err != nil {
				t.Fatal(err)
			}
			if out.Reply != "done" {
				t.Fatalf("the turn did not run: %+v", out.Stop)
			}
			if got := sent(model.Inputs()[0]); len(got) != 0 {
				t.Fatalf("model saw %v; want nothing added", got)
			}
			if !hasWarning(out.Warnings, dashi.WarningHookFailed) {
				t.Fatalf("warnings = %+v; want %s", out.Warnings, dashi.WarningHookFailed)
			}
			sel := rec.all()[0]
			if sel.Err == nil || !strings.Contains(sel.Err.Error(), tc.want) || len(sel.Added) != 0 {
				t.Fatalf("selection error = %v; want %q", sel.Err, tc.want)
			}
			if time.Since(began) > 500*time.Millisecond {
				t.Fatalf("the turn waited %v", time.Since(began))
			}
		})
	}
}

func TestTooLargeAndMaxCandidates(t *testing.T) {
	srv, client := judge(t, map[string]float64{"a": 0.9, "b": 0.9, "c": 0.9})
	var rec record
	ranker := &contextrank.Ranker{
		Client:        client,
		Source:        fixed(note("huge", 2000), note("a", 50), note("b", 50), note("c", 50)),
		MaxBytes:      1000,
		MaxCandidates: 2,
		OnSelection:   rec.add,
	}
	model := dashitest.NewClient(dashitest.Reply("ok"))
	runner := dashi.Runner{Client: model, Hooks: ranker.Hook().Hooks()}
	if _, err := runner.Run(t.Context(), dashi.Turn{}, start("hi")); err != nil {
		t.Fatal(err)
	}
	want := map[string]contextrank.Cause{
		"huge": contextrank.CauseTooLarge, "a": contextrank.CauseChosen,
		"b": contextrank.CauseChosen, "c": contextrank.CauseOverMaxCandidates,
	}
	if got := causes(rec.all()[0]); !mapsEqual(got, want) {
		t.Fatalf("causes = %v; want %v", got, want)
	}
	if n := len(srv.Requests()[0].Request.Questions); n != 2 {
		t.Fatalf("questions = %d; want 2", n)
	}
}

func TestManyCandidatesSplitAcrossRequests(t *testing.T) {
	srv, client := judge(t, map[string]float64{"n99": 0.9})
	var cands []contextrank.Candidate
	for i := range 100 {
		cands = append(cands, note(fmt.Sprintf("n%02d", i), 40))
	}
	var rec record
	ranker := &contextrank.Ranker{Client: client, Source: fixed(cands...), MaxCandidates: -1, OnSelection: rec.add}
	model := dashitest.NewClient(dashitest.Reply("ok"))
	runner := dashi.Runner{Client: model, Hooks: ranker.Hook().Hooks()}
	if _, err := runner.Run(t.Context(), dashi.Turn{}, start("hi")); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.Requests()); n != 2 {
		t.Fatalf("Decide requests = %d; want 2", n)
	}
	if got := sent(model.Inputs()[0]); !slices.Equal(got, []string{"n99"}) {
		t.Fatalf("model saw %v", got)
	}
	if ids := rec.all()[0].RequestIDs; len(ids) != 2 {
		t.Fatalf("request IDs = %v", ids)
	}
}
