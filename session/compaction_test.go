package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
)

// keepLast is a Compactor that compacts once the last call used at least
// limit tokens, keeping the last keep messages after the summary.
type keepLast struct {
	limit int
	keep  int
	err   error
}

func (c keepLast) Needed(_ []llm.Message, last llm.Usage) bool {
	return last.InputTokens+last.OutputTokens >= c.limit
}

func (c keepLast) Start(projected []llm.Message) (dashi.Start, error) {
	if c.err != nil {
		return dashi.Start{}, c.err
	}
	return dashi.Start{
		Input: []llm.Message{llm.UserMessage(llm.Text("Summarize."))},
		Compaction: &dashi.CompactionFact{
			Keep:  slices.Clone(projected[len(projected)-c.keep:]),
			Frame: "Summary:",
		},
	}, nil
}

// used is a reply whose call used tokens of context.
func used(text string, tokens int) llm.Result {
	r := dashitest.Reply(text)
	r.Usage = &llm.Usage{InputTokens: tokens}
	return r
}

func textsOf(msgs []llm.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Text()
	}
	return out
}

func sendAll(t *testing.T, s *session.Session, texts ...string) dashi.RunResult {
	t.Helper()
	var out dashi.RunResult
	for _, text := range texts {
		var err error
		if out, err = s.Send(t.Context(), text); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func loadTurn(t *testing.T, rec dashi.Loader, id string) dashi.Turn {
	t.Helper()
	turn, err := rec.Load(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

func codes(ws []dashi.Warning) []dashi.WarningCode {
	var out []dashi.WarningCode
	for _, w := range ws {
		out = append(out, w.Code)
	}
	return out
}

// countingRecorder records the turn IDs loaded through it.
type countingRecorder struct {
	*dashi.MemoryRecorder
	mu     sync.Mutex
	loaded []string
}

func (r *countingRecorder) Load(ctx context.Context, id string) (dashi.Turn, error) {
	r.mu.Lock()
	r.loaded = append(r.loaded, id)
	r.mu.Unlock()
	return r.MemoryRecorder.Load(ctx, id)
}

// A turn whose last call used the threshold makes the next Send run a
// compaction turn first. The caller's turn sends the replacement history;
// History keeps the conversation; a fresh handle with no Compactor loads
// back to the compaction turn only and sends the same bytes.
func TestSessionCompactionTurn(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(
		dashitest.Reply("one"),
		used("two", 100),
		dashitest.Reply("Heron on port 8443."),
		dashitest.Reply("three"),
		dashitest.Reply("four"),
	)
	s := handle(rec, client)
	s.New = true
	s.Compactor = keepLast{limit: 100, keep: 1}
	out := sendAll(t, s, "first", "second", "third")
	if out.Turn.ID != "s-4" || out.Reply != "three" || len(out.SessionWarnings) != 0 {
		t.Fatalf("turn=%s answer=%q warnings=%v", out.Turn.ID, out.Reply, out.SessionWarnings)
	}

	c := load(t, rec, "s-3")
	start := loadTurn(t, rec, "s-3").Steps[0]
	if c.Origin != dashi.StartCompaction || start.CommandID != "compact:s-3" ||
		!c.Agent.NoTools || c.Compaction == nil || c.Compaction.Through != 2 ||
		c.Compaction.Hash == "" {
		t.Fatalf("origin=%q key=%q noTools=%v fact=%+v",
			c.Origin, start.CommandID, c.Agent.NoTools, c.Compaction)
	}
	// The summary request carries no history.
	if got := textsOf(client.Inputs()[2].Messages); !slices.Equal(got, []string{"Summarize."}) {
		t.Fatalf("summary request=%q", got)
	}
	replaced := []string{"Summary:\n\nHeron on port 8443.", "two"}
	if got := textsOf(client.Inputs()[3].Messages); !slices.Equal(got, append(replaced, "third")) {
		t.Fatalf("turn 4 sent %q", got)
	}
	history, err := s.History(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := textsOf(history); !slices.Equal(got,
		[]string{"first", "one", "second", "two", "third", "three"}) {
		t.Fatalf("history=%q", got)
	}
	projected, err := s.NextHistory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := textsOf(projected); !slices.Equal(got, append(replaced, "third", "three")) {
		t.Fatalf("projected=%q", got)
	}
	n := 0
	for turn, err := range s.Turns(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		_ = turn
	}
	if n != 4 {
		t.Fatalf("Turns yielded %d, want 4", n)
	}

	counting := &countingRecorder{MemoryRecorder: rec}
	fresh := handle(counting, client)
	again, err := fresh.NextHistory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(projected)
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Fatalf("fresh handle projected\n%s\nwant\n%s", b, a)
	}
	if slices.Contains(counting.loaded, "s-1") || slices.Contains(counting.loaded, "s-2") {
		t.Fatalf("fresh handle loaded %q", counting.loaded)
	}
	out, err = fresh.Send(t.Context(), "fourth")
	if err != nil || out.Turn.ID != "s-5" {
		t.Fatalf("turn=%s err=%v", out.Turn.ID, err)
	}
	sent := client.Inputs()[4].Messages
	b, _ = json.Marshal(sent[:len(sent)-1])
	if string(a) != string(b) {
		t.Fatalf("fresh handle sent\n%s\nwant\n%s", b, a)
	}
}

// tampered changes turn s-1's input as it loads, as a bug that rewrote an
// ended turn would.
type tampered struct {
	*dashi.MemoryRecorder
}

func (r tampered) Load(ctx context.Context, id string) (dashi.Turn, error) {
	turn, err := r.MemoryRecorder.Load(ctx, id)
	if err == nil && id == "s-1" {
		turn.Steps[0].Started.Input = []llm.Message{llm.UserMessage(llm.Text("changed"))}
	}
	return turn, err
}

// A digest mismatch warns only on a handle that holds the covered turns,
// and changes nothing else: the replacement history is still sent.
func TestSessionCompactionMismatch(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(
		used("one", 100),
		dashitest.Reply("summary"),
		dashitest.Reply("two"),
		dashitest.Reply("three"),
		dashitest.Reply("four"),
	)
	s := handle(rec, client)
	s.New = true
	s.Compactor = keepLast{limit: 100, keep: 0}
	sendAll(t, s, "first", "second")
	want, err := s.NextHistory(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// A handle that never loaded the covered turn does not check.
	short := handle(tampered{rec}, client)
	out := sendAll(t, short, "third")
	if len(out.SessionWarnings) != 0 {
		t.Fatalf("warnings=%v", out.SessionWarnings)
	}

	// History loads turn 1, so the next turn checks the digest.
	full := handle(tampered{rec}, client)
	if _, err := full.History(t.Context()); err != nil {
		t.Fatal(err)
	}
	out = sendAll(t, full, "fourth")
	if got := codes(out.SessionWarnings); !slices.Equal(got,
		[]dashi.WarningCode{dashi.WarningCompactionMismatch}) {
		t.Fatalf("warnings=%v", out.SessionWarnings)
	}
	sent := textsOf(client.Inputs()[4].Messages)
	if got := sent[:len(want)]; !slices.Equal(got, textsOf(want)) {
		t.Fatalf("sent %q, want the replacement %q first", sent, textsOf(want))
	}
}

// A summary that fails closes the compaction turn, keeps the previous
// projected history, and warns; the next completed turn tries again.
func TestSessionCompactionFailed(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	refusal := llm.Result{
		Message: llm.AssistantMessage(llm.Text("I cannot.")),
		Stop:    llm.Stop{Kind: llm.StopRefusal},
	}
	client := dashitest.NewClient(
		used("one", 100),
		refusal,
		used("two", 100),
		dashitest.Reply("summary"),
		dashitest.Reply("three"),
	)
	s := handle(rec, client)
	s.New = true
	s.Compactor = keepLast{limit: 100, keep: 0}
	sendAll(t, s, "first")
	out := sendAll(t, s, "second")
	if out.Turn.ID != "s-3" || !slices.Equal(codes(out.SessionWarnings),
		[]dashi.WarningCode{session.WarningCompactionFailed}) {
		t.Fatalf("turn=%s warnings=%v", out.Turn.ID, out.SessionWarnings)
	}
	c := load(t, rec, "s-2")
	if c.Origin != dashi.StartCompaction || c.Stop.Kind != dashi.StopCanceled {
		t.Fatalf("s-2 origin=%q stop=%s", c.Origin, c.Stop.Kind)
	}
	if got := loadTurn(t, rec, "s-2").Steps; got[len(got)-1].CommandID != "close:s-2" {
		t.Fatalf("closed by %q", got[len(got)-1].CommandID)
	}
	if got := textsOf(client.Inputs()[2].Messages); !slices.Equal(got,
		[]string{"first", "one", "second"}) {
		t.Fatalf("turn 3 sent %q", got)
	}
	// Turn 3 completed, so the next Send compacts.
	out = sendAll(t, s, "third")
	if out.Turn.ID != "s-5" || len(out.SessionWarnings) != 0 ||
		load(t, rec, "s-4").Origin != dashi.StartCompaction {
		t.Fatalf("turn=%s warnings=%v", out.Turn.ID, out.SessionWarnings)
	}
}

// No compaction runs right after a turn that failed or was canceled, nor
// when the Compactor cannot build its Start, which warns.
func TestSessionCompactionSkipped(t *testing.T) {
	always := keepLast{limit: 0}
	t.Run("after a closed turn", func(t *testing.T) {
		rec := dashi.NewMemoryRecorder()
		client := held(1, dashitest.Reply("two"))
		s := handle(rec, client)
		s.New = true
		ctx, cancel := context.WithCancel(t.Context())
		done := sendAsync(ctx, s, "first")
		entered(t, client)
		cancel()
		<-done
		s.Compactor = always
		out := sendAll(t, s, "second")
		if out.Turn.ID != "s-2" || !slices.Equal(codes(out.SessionWarnings),
			[]dashi.WarningCode{session.WarningTurnClosed}) {
			t.Fatalf("turn=%s warnings=%v", out.Turn.ID, out.SessionWarnings)
		}
	})
	t.Run("after a turn closed with structured output", func(t *testing.T) {
		// Closing completes a turn that recorded structured output, and
		// still no compaction follows it.
		rec := dashi.NewMemoryRecorder()
		client := dashitest.NewClient(
			dashitest.CallTool("call-1", "lookup", `{}`),
			dashitest.Reply(`{"answer":"42"}`),
			dashitest.Reply("two"),
		)
		s := handle(rec, client)
		s.New = true
		s.Runner.Tools = (&chargeTool{}).tools()
		s.Agent.Limits = dashi.Limits{ModelCalls: 2}
		start := dashi.Say("first")
		start.OutputSchema = json.RawMessage(`{"type":"object"}`)
		if out, err := s.Run(t.Context(), start); err != nil || out.Output == nil {
			t.Fatalf("output: %v, %v", err, out.Output)
		}
		s.Compactor = always
		var outcomes []string
		var closing bool
		for ev, err := range s.Stream(t.Context(), dashi.Say("second")) {
			if err != nil {
				t.Fatal(err)
			}
			if ev.TurnID == "s-1" && ev.Step != nil {
				closing = true
			}
			if ev.Result != nil {
				outcomes = append(outcomes, ev.TurnID)
				if ev.TurnID == "s-2" && !slices.Equal(codes(ev.Result.SessionWarnings),
					[]dashi.WarningCode{session.WarningTurnClosed}) {
					t.Fatalf("warnings=%v", ev.Result.SessionWarnings)
				}
			}
		}
		if !closing || !slices.Equal(outcomes, []string{"s-1", "s-2"}) {
			t.Fatalf("closing events=%v outcomes=%q", closing, outcomes)
		}
		if v := load(t, rec, "s-1"); v.Stop.Kind != dashi.StopCompleted {
			t.Fatalf("s-1 stop=%s", v.Stop.Kind)
		}
		if v := load(t, rec, "s-2"); v.Origin == dashi.StartCompaction {
			t.Fatal("s-2 is a compaction turn")
		}
	})
	t.Run("start fails", func(t *testing.T) {
		rec := dashi.NewMemoryRecorder()
		client := dashitest.NewClient(dashitest.Reply("one"), dashitest.Reply("two"))
		s := handle(rec, client)
		s.New = true
		s.Compactor = keepLast{err: errors.New("no cut")}
		out := sendAll(t, s, "first", "second")
		if out.Turn.ID != "s-2" || !slices.Equal(codes(out.SessionWarnings),
			[]dashi.WarningCode{session.WarningCompactionFailed}) ||
			!strings.Contains(out.SessionWarnings[0].Message, "no cut") {
			t.Fatalf("turn=%s warnings=%v", out.Turn.ID, out.SessionWarnings)
		}
	})
}

// A compaction turn cut short is closed by the next Start with
// WarningCompactionFailed, and Steer refuses while one runs.
func TestSessionCompactionInterrupted(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := held(2, used("one", 100), dashitest.Reply("two"))
	s := handle(rec, client)
	s.New = true
	s.Compactor = keepLast{limit: 100}
	sendAll(t, s, "first")
	ctx, cancel := context.WithCancel(t.Context())
	done := sendAsync(ctx, s, "second")
	entered(t, client)
	_, err := s.Steer(t.Context(), "hurry")
	var busy *session.TurnBusyError
	if !errors.As(err, &busy) || busy.Origin != dashi.StartCompaction ||
		busy.TurnID != "s-2" || !errors.Is(err, session.ErrTurnBusy) {
		t.Fatalf("steer err=%v", err)
	}
	if _, err := s.Enqueue(t.Context(), "later"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if r := <-done; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err=%v", r.err)
	}
	if v := load(t, rec, "s-2"); v.Next() != dashi.NextContinue {
		t.Fatalf("s-2 next=%s", v.Next())
	}
	out := sendAll(t, s, "second")
	if out.Turn.ID != "s-3" || !slices.Equal(codes(out.SessionWarnings),
		[]dashi.WarningCode{session.WarningCompactionFailed}) {
		t.Fatalf("turn=%s warnings=%v", out.Turn.ID, out.SessionWarnings)
	}
	if got := input(t, rec, "s-3"); !slices.Equal(got, []string{"later", "second"}) {
		t.Fatalf("input=%q", got)
	}
}

// The compaction check runs before a queue turn.
func TestSessionCompactionBeforeQueueTurn(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(
		used("one", 100),
		dashitest.Reply("summary"),
		dashitest.Reply("two"),
	)
	s := handle(rec, client)
	s.New = true
	s.Compactor = keepLast{limit: 100}
	sendAll(t, s, "first")
	if _, err := s.EnqueueItem(t.Context(), dashi.ContextItem{
		Origin:  dashi.ContextTool,
		Message: llm.UserMessage(llm.Text("task done")),
	}); err != nil {
		t.Fatal(err)
	}
	out, ran, err := s.RunQueue(t.Context())
	if err != nil || !ran || out.Turn.ID != "s-3" {
		t.Fatalf("ran=%v turn=%s err=%v", ran, out.Turn.ID, err)
	}
	if load(t, rec, "s-2").Origin != dashi.StartCompaction {
		t.Fatal("s-2 is not a compaction turn")
	}
	if got := textsOf(client.Inputs()[2].Messages); !slices.Equal(got,
		[]string{"Summary:\n\nsummary", "task done"}) {
		t.Fatalf("queue turn sent %q", got)
	}
}

// A failed compaction turn is closed at once, so the queue turn after it
// runs.
func TestSessionCompactionFailsBeforeQueueTurn(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(
		used("one", 100),
		llm.Result{
			Message: llm.AssistantMessage(llm.Text("I cannot.")),
			Stop:    llm.Stop{Kind: llm.StopRefusal},
		},
		dashitest.Reply("two"),
	)
	s := handle(rec, client)
	s.New = true
	s.Compactor = keepLast{limit: 100}
	sendAll(t, s, "first")
	if _, err := s.EnqueueItem(t.Context(), dashi.ContextItem{
		Origin:  dashi.ContextTool,
		Message: llm.UserMessage(llm.Text("task done")),
	}); err != nil {
		t.Fatal(err)
	}
	out, ran, err := s.RunQueue(t.Context())
	if err != nil || !ran || out.Turn.ID != "s-3" || !slices.Equal(
		codes(out.SessionWarnings),
		[]dashi.WarningCode{session.WarningCompactionFailed},
	) {
		t.Fatalf("ran=%v turn=%s warnings=%v err=%v", ran, out.Turn.ID, out.SessionWarnings, err)
	}
	if got := textsOf(client.Inputs()[2].Messages); !slices.Equal(got,
		[]string{"first", "one", "task done"}) {
		t.Fatalf("queue turn sent %q", got)
	}
}

// Stream yields the compaction turn's events, ending with its RunResult
// event, ahead of the caller's turn.
func TestSessionStreamCompaction(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(
		used("one", 100),
		dashitest.Reply("summary"),
		dashitest.Reply("two"),
	)
	s := handle(rec, client)
	s.New = true
	s.Compactor = keepLast{limit: 100}
	sendAll(t, s, "first")
	var outcomes []string
	for ev, err := range s.Stream(t.Context(), dashi.Say("second")) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.TurnID != "s-2" && ev.TurnID != "s-3" {
			t.Fatalf("event of turn %q", ev.TurnID)
		}
		if ev.Result != nil {
			outcomes = append(outcomes, ev.TurnID+" "+ev.Result.Reply)
		}
	}
	if !slices.Equal(outcomes, []string{"s-2 summary", "s-3 two"}) {
		t.Fatalf("outcomes=%q", outcomes)
	}
}

// The engine accepts a compaction fact only with origin compaction, and
// requires it there.
func TestStartCompactionOrigin(t *testing.T) {
	r := dashi.Runner{Client: dashitest.NewClient(dashitest.Reply("x"))}
	agent := dashi.Agent{Model: dashitest.Model}
	in := []llm.Message{llm.UserMessage(llm.Text("hi"))}
	for _, c := range []dashi.Start{
		{Agent: agent, Input: in, Compaction: &dashi.CompactionFact{}},
		{Agent: agent, Input: in, Origin: dashi.StartCompaction},
		{Agent: agent, Input: in, Origin: dashi.StartCompaction,
			Compaction: &dashi.CompactionFact{SummaryAt: 1}},
	} {
		if _, err := r.Run(t.Context(), dashi.NewTurn("t"), c); err == nil {
			t.Fatalf("Start %+v ran", c)
		}
	}
}

// A Continue on an interrupted compaction turn sends no history, as its
// Start did, and its summary then stands for the turns it covers.
func TestSessionContinueCompaction(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := held(2, used("one", 100), dashitest.Reply("summary"), dashitest.Reply("two"))
	s := handle(rec, client)
	s.New = true
	s.Compactor = keepLast{limit: 100}
	sendAll(t, s, "first")
	ctx, cancel := context.WithCancel(t.Context())
	done := sendAsync(ctx, s, "second")
	entered(t, client)
	cancel()
	<-done
	out, err := s.Run(t.Context(), dashi.Continue{})
	if err != nil || out.Turn.ID != "s-2" || out.Reply != "summary" {
		t.Fatalf("turn=%s answer=%q err=%v", out.Turn.ID, out.Reply, err)
	}
	for _, w := range out.Warnings {
		if w.Code == dashi.WarningHistoryMismatch {
			t.Fatal(w.Message)
		}
	}
	out = sendAll(t, s, "second")
	if got := textsOf(client.Inputs()[2].Messages); out.Turn.ID != "s-3" ||
		!slices.Equal(got, []string{"Summary:\n\nsummary", "second"}) {
		t.Fatalf("turn=%s sent %q", out.Turn.ID, got)
	}
}

// Two workers race for the compaction slot: worker B runs the compaction
// turn and its own turn while worker A's turn_started commit waits. A
// then finds the slot taken, plans again, and sends B's replacement
// history, with no compaction_failed warning and no second summary call.
func TestSessionCompactionRace(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(
		used("one", 100),
		dashitest.Reply("summary"),
		dashitest.Reply("b"),
		dashitest.Reply("a"),
	)
	b := handle(rec, client)
	b.New = true
	b.Compactor = keepLast{limit: 100}
	sendAll(t, b, "first")

	var bOut dashi.RunResult
	var bErr error
	a := handle(&raceRecorder{MemoryRecorder: rec, before: func() {
		bOut, bErr = b.Send(t.Context(), "from b")
	}}, client)
	a.Compactor = keepLast{limit: 100}
	out, err := a.Send(t.Context(), "from a")
	if bErr != nil || bOut.Turn.ID != "s-3" {
		t.Fatalf("b: turn=%s err=%v", bOut.Turn.ID, bErr)
	}
	if err != nil || out.Turn.ID != "s-4" || out.Reply != "a" ||
		len(out.SessionWarnings) != 0 {
		t.Fatalf("a: turn=%s answer=%q warnings=%v err=%v",
			out.Turn.ID, out.Reply, out.SessionWarnings, err)
	}
	if n := len(client.Inputs()); n != 4 {
		t.Fatalf("%d model calls; want 4", n)
	}
	var compactions []string
	for turn, err := range a.Turns(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		if turn.Steps[0].Started.Origin == dashi.StartCompaction {
			compactions = append(compactions, turn.ID)
		}
	}
	if !slices.Equal(compactions, []string{"s-2"}) {
		t.Fatalf("compaction turns %q", compactions)
	}
	if got := textsOf(client.Inputs()[3].Messages); !slices.Equal(got,
		[]string{"Summary:\n\nsummary", "from b", "b", "from a"}) {
		t.Fatalf("a sent %q", got)
	}
}

// The caller's Start.Budget caps the compaction turn that runs before it:
// the summary call's output allowance is clamped to it, and a summary
// that spends past it fails the compaction, not the caller's turn.
func TestSessionCompactionBudget(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	summary := dashitest.Reply("summary")
	summary.Usage = &llm.Usage{OutputTokens: 10}
	client := dashitest.NewClient(used("one", 100), summary, dashitest.Reply("two"))
	s := handle(rec, client)
	s.New = true
	s.Compactor = keepLast{limit: 100}
	sendAll(t, s, "first")
	start := dashi.Say("second")
	start.Budget = &dashi.Budget{MaxOutputTokens: 5}
	out, err := s.Run(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	c := loadTurn(t, rec, "s-2").Steps[0].Started
	if c.Origin != dashi.StartCompaction || c.Budget == nil ||
		c.Budget.MaxOutputTokens != 5 {
		t.Fatalf("s-2 origin=%q budget=%+v", c.Origin, c.Budget)
	}
	if got := client.Inputs()[1].Settings.MaxTokens; got != 5 {
		t.Fatalf("summary max tokens=%d; want 5", got)
	}
	if v := load(t, rec, "s-2"); v.Stop.Kind != dashi.StopCanceled {
		t.Fatalf("s-2 stop=%s", v.Stop.Kind)
	}
	if out.Turn.ID != "s-3" || out.Reply != "two" || !slices.Equal(
		codes(out.SessionWarnings),
		[]dashi.WarningCode{session.WarningCompactionFailed},
	) {
		t.Fatalf("turn=%s answer=%q warnings=%v", out.Turn.ID, out.Reply, out.SessionWarnings)
	}
}

// The summary is the text inside the answer's <summary> tags when it has
// them, and the whole answer otherwise; a handle with no Compactor reads
// the same. Empty tags replace nothing.
func TestSessionCompactionSummaryTags(t *testing.T) {
	for _, tc := range []struct{ answer, want string }{
		{"Here it is:\n<summary>\nHeron, port 8443.\n</summary>\nDone.", "Heron, port 8443."},
		{"Heron, port 8443.", "Heron, port 8443."},
		{"<summary> </summary>", ""},
	} {
		rec := dashi.NewMemoryRecorder()
		client := dashitest.NewClient(
			used("one", 100),
			dashitest.Reply(tc.answer),
			dashitest.Reply("two"),
		)
		s := handle(rec, client)
		s.New = true
		s.Compactor = keepLast{limit: 100}
		sendAll(t, s, "first", "second")
		want := []string{"Summary:\n\n" + tc.want, "second", "two"}
		if tc.want == "" {
			want = []string{"first", "one", "second", "two"}
		}
		for _, h := range []*session.Session{s, handle(rec, client)} {
			projected, err := h.NextHistory(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if got := textsOf(projected); !slices.Equal(got, want) {
				t.Fatalf("answer %q: projected=%q", tc.answer, got)
			}
		}
	}
}

// Two calls on one handle plan the same compaction slot. The loser's
// finish must not clear the handle's compacting mark while the winner's
// compaction turn runs, so a Steer still refuses.
func TestSessionCompactionSameHandleRace(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(
		used("one", 100),
		dashitest.Reply("summary"),
		dashitest.Reply("b"),
	)
	first := handle(rec, client)
	first.New = true
	sendAll(t, first, "first")

	g := newGate()
	bDone := make(chan error, 1)
	var s *session.Session
	race := &raceRecorder{MemoryRecorder: rec, before: func() {
		// A has planned the compaction slot; B plans it too and wins.
		go func() {
			_, err := s.Send(context.Background(), "from b")
			bDone <- err
		}()
		g.requireEntered(t, bDone)
	}}
	s = handle(race, gatedClient{Client: client, gate: g})
	s.Compactor = keepLast{limit: 100}
	s.Send(t.Context(), "from a") // loses the slot; its result does not matter

	_, err := s.Steer(t.Context(), "hurry")
	var busy *session.TurnBusyError
	if !errors.As(err, &busy) || busy.Origin != dashi.StartCompaction {
		t.Fatalf("steer during the compaction: %v; want a TurnBusyError", err)
	}
	close(g.release)
	if err := <-bDone; err != nil {
		t.Fatal(err)
	}
}

// Only the session's Compactor starts compaction turns: the projection
// trusts them, so a caller's Start cannot claim the origin or the fact.
func TestSessionRefusesCallerCompaction(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	s := handle(rec, dashitest.NewClient())
	s.New = true
	byOrigin := dashi.Say("hello")
	byOrigin.Origin = dashi.StartCompaction
	byFact := dashi.Say("hello")
	byFact.Compaction = &dashi.CompactionFact{Frame: "Summary:"}
	both := byFact
	both.Origin = dashi.StartCompaction
	for name, start := range map[string]dashi.Start{
		"origin": byOrigin,
		"fact":   byFact,
		"both":   both,
	} {
		if _, err := s.Run(t.Context(), start); err == nil ||
			!strings.Contains(err.Error(), "Compactor") {
			t.Fatalf("Run with %s: err = %v; want a refusal", name, err)
		}
		var err error
		for _, e := range s.Stream(t.Context(), start) {
			err = e
		}
		if err == nil || !strings.Contains(err.Error(), "Compactor") {
			t.Fatalf("Stream with %s: err = %v; want a refusal", name, err)
		}
	}
	if _, err := rec.Load(t.Context(), session.TurnID("s", 1)); !errors.Is(err, dashi.ErrTurnNotFound) {
		t.Fatalf("turn 1: %v; want none", err)
	}
}
