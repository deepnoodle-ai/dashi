package dashi_test

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

func sendAll(t *testing.T, s *dashi.Session, texts ...string) dashi.Outcome {
	t.Helper()
	var out dashi.Outcome
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
	if out.Turn.ID != "s-4" || out.Answer != "three" || len(out.SessionWarnings) != 0 {
		t.Fatalf("turn=%s answer=%q warnings=%v", out.Turn.ID, out.Answer, out.SessionWarnings)
	}

	c := load(t, rec, "s-3")
	start := loadTurn(t, rec, "s-3").Steps[0]
	if c.Origin != dashi.StartCompaction || start.CommandID != "compact:s-3" ||
		!c.Agent.NoTools || c.Compaction == nil || c.Compaction.Through != 2 ||
		c.Compaction.Digest == "" {
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
	projected, err := s.ProjectedHistory(t.Context())
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
	again, err := fresh.ProjectedHistory(t.Context())
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
	want, err := s.ProjectedHistory(t.Context())
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
		[]dashi.WarningCode{dashi.WarningCompactionFailed}) {
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
			[]dashi.WarningCode{dashi.WarningTurnClosed}) {
			t.Fatalf("turn=%s warnings=%v", out.Turn.ID, out.SessionWarnings)
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
			[]dashi.WarningCode{dashi.WarningCompactionFailed}) ||
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
	var busy *dashi.TurnBusyError
	if !errors.As(err, &busy) || busy.Origin != dashi.StartCompaction ||
		busy.TurnID != "s-2" || !errors.Is(err, dashi.ErrTurnBusy) {
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
		[]dashi.WarningCode{dashi.WarningCompactionFailed}) {
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
		[]dashi.WarningCode{dashi.WarningCompactionFailed},
	) {
		t.Fatalf("ran=%v turn=%s warnings=%v err=%v", ran, out.Turn.ID, out.SessionWarnings, err)
	}
	if got := textsOf(client.Inputs()[2].Messages); !slices.Equal(got,
		[]string{"first", "one", "task done"}) {
		t.Fatalf("queue turn sent %q", got)
	}
}

// Stream yields the compaction turn's events, ending with its Outcome
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
		if ev.Outcome != nil {
			outcomes = append(outcomes, ev.TurnID+" "+ev.Outcome.Answer)
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
	if err != nil || out.Turn.ID != "s-2" || out.Answer != "summary" {
		t.Fatalf("turn=%s answer=%q err=%v", out.Turn.ID, out.Answer, err)
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
