package dashi_test

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

// heldClient holds model call number at (from 1) at its gate.
type heldClient struct {
	*dashitest.Client
	at   int32
	n    atomic.Int32
	gate *gate
}

func (c *heldClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	if c.n.Add(1) == c.at {
		if err := c.gate.wait(ctx); err != nil {
			return llm.Result{}, err
		}
	}
	return c.Client.Send(ctx, call)
}

func held(at int32, replies ...llm.Result) *heldClient {
	return &heldClient{Client: dashitest.NewClient(replies...), at: at, gate: newGate()}
}

// sendAsync starts a Send and returns its result channel.
func sendAsync(
	ctx context.Context,
	s *dashi.Session,
	text string,
) <-chan result {
	done := make(chan result, 1)
	go func() {
		out, err := s.Send(ctx, text)
		done <- result{out, err}
	}()
	return done
}

type result struct {
	out dashi.Outcome
	err error
}

// awaitSend returns the result, failing the test on an error.
func awaitSend(t *testing.T, done <-chan result) dashi.Outcome {
	t.Helper()
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.out
}

// entered waits until the held call is at its gate.
func entered(t *testing.T, c *heldClient) {
	t.Helper()
	<-c.gate.entered
}

// input returns the text of each message a turn started with.
func input(t *testing.T, rec dashi.Loader, id string) []string {
	t.Helper()
	turn, err := rec.Load(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range turn.Steps[0].Started.Input {
		out = append(out, m.Text())
	}
	return out
}

func queued(t *testing.T, s *dashi.Session) int {
	t.Helper()
	st, err := s.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return st.Queued
}

// Input queued while a turn runs starts a queue turn in the same call once
// the turn ends, and RunQueue reports false when nothing is queued.
func TestSessionDispatchOnIdle(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := held(1, dashitest.Reply("one"), dashitest.Reply("two"))
	s := handle(rec, client)
	s.New = true
	done := sendAsync(t.Context(), s, "first")
	entered(t, client)
	q, err := s.Enqueue(t.Context(), "second")
	if err != nil || q.TurnID != "" || len(q.Warnings) != 0 || q.ID == "" {
		t.Fatalf("queued=%+v err=%v", q, err)
	}
	if n := queued(t, s); n != 1 {
		t.Fatalf("queued=%d", n)
	}
	close(client.gate.release)
	out := awaitSend(t, done)
	if out.Turn.ID != "s-2" || out.Answer != "two" || out.Origin != dashi.StartUser {
		t.Fatalf("turn=%s answer=%q origin=%q", out.Turn.ID, out.Answer, out.Origin)
	}
	if got, want := out.Turn.Steps[0].CommandID, `queue:["`+q.ID+`"]`; got != want {
		t.Fatalf("command ID=%q, want %q", got, want)
	}
	if got := input(t, rec, "s-2"); !slices.Equal(got, []string{"second"}) {
		t.Fatalf("input=%q", got)
	}
	// The queue turn got the first turn's history.
	if n := len(client.Inputs()[1].Messages); n != 3 {
		t.Fatalf("queue turn sent %d messages, want 3", n)
	}
	if n := queued(t, s); n != 0 {
		t.Fatalf("queued=%d", n)
	}
	last, ran, err := s.RunQueue(t.Context())
	if err != nil || ran || last.Turn.ID != "s-2" {
		t.Fatalf("ran=%v turn=%s err=%v", ran, last.Turn.ID, err)
	}
	requireTurns(t, rec, 2)
}

// Items wait while the last turn has stopped without ending; they run once
// a Continue ends it.
func TestSessionQueueWaitsForStop(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	// The held call is canceled before it takes a reply.
	client := held(1, dashitest.Reply("resumed"), dashitest.Reply("two"))
	s := handle(rec, client)
	s.New = true
	ctx, cancel := context.WithCancel(t.Context())
	done := sendAsync(ctx, s, "first")
	entered(t, client)
	if _, err := s.Enqueue(t.Context(), "later"); err != nil {
		t.Fatal(err)
	}
	cancel()
	r := <-done
	if r.err != nil || r.out.Next() != dashi.NextContinue {
		t.Fatalf("next=%s err=%v", r.out.Next(), r.err)
	}
	if n := queued(t, s); n != 1 {
		t.Fatalf("queued=%d", n)
	}
	out, ran, err := s.RunQueue(t.Context())
	if err != nil || ran || out.Turn.ID != "s-1" {
		t.Fatalf("ran=%v turn=%s err=%v", ran, out.Turn.ID, err)
	}
	close(client.gate.release)
	out, err = s.Run(t.Context(), dashi.Continue{})
	if err != nil || out.Turn.ID != "s-2" || out.Answer != "two" {
		t.Fatalf("turn=%s answer=%q err=%v", out.Turn.ID, out.Answer, err)
	}
	if got := input(t, rec, "s-2"); !slices.Equal(got, []string{"later"}) {
		t.Fatalf("input=%q", got)
	}
}

// Steer reaches the running turn; with no turn running it queues.
func TestSessionSteer(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := held(1,
		dashitest.CallTool("c1", "lookup", `{}`),
		dashitest.Reply("done"),
		dashitest.Reply("queued answer"),
	)
	s := handle(rec, client)
	s.New = true
	s.Runner.Tools = (&chargeTool{}).tools()
	done := sendAsync(t.Context(), s, "start")
	entered(t, client)
	q, err := s.Steer(t.Context(), "use plan B")
	if err != nil || q.TurnID != "s-1" {
		t.Fatalf("queued=%+v err=%v", q, err)
	}
	close(client.gate.release)
	out := awaitSend(t, done)
	if out.Turn.ID != "s-1" || out.Answer != "done" {
		t.Fatalf("turn=%s answer=%q", out.Turn.ID, out.Answer)
	}
	if !recordsSteer(out.Turn, q.ID) {
		t.Fatal("the steer was not recorded")
	}
	if !strings.Contains(texts(client.Inputs()[1].Messages), "use plan B") {
		t.Fatal("the model did not see the steer")
	}

	// Idle: the steer goes to the queue.
	q, err = s.Steer(t.Context(), "and then this")
	if err != nil || q.TurnID != "" {
		t.Fatalf("queued=%+v err=%v", q, err)
	}
	out, ran, err := s.RunQueue(t.Context())
	if err != nil || !ran || out.Answer != "queued answer" {
		t.Fatalf("ran=%v answer=%q err=%v", ran, out.Answer, err)
	}

	// Empty text, a repeated ID, and a host source that is not an Inbox.
	if _, err := s.Steer(t.Context(), " "); err == nil {
		t.Fatal("empty steer accepted")
	}
	if _, err := s.EnqueueItem(t.Context(), dashi.ContextItem{
		ID: "x", Message: llm.UserMessage(llm.Text("a")),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueItem(t.Context(), dashi.ContextItem{
		ID: "x", Message: llm.UserMessage(llm.Text("b")),
	}); err == nil {
		t.Fatal("repeated ID accepted")
	}
	host := handle(rec, client)
	host.Steering = hostSource{}
	if _, err := host.Steer(t.Context(), "hi"); err == nil {
		t.Fatal("Steer accepted a source that is not an Inbox")
	}
	inbox := dashi.NewInbox()
	host.Steering = inbox
	if q, err := host.Steer(t.Context(), "hi"); err != nil || q.TurnID != "" {
		t.Fatalf("queued=%+v err=%v", q, err)
	}
}

type hostSource struct{}

func (hostSource) Steers(context.Context, string) ([]dashi.Steer, error) {
	return nil, nil
}

func (hostSource) Delivered(context.Context, string, []string) error {
	return nil
}

func recordsSteer(turn dashi.Turn, id string) bool {
	for _, step := range turn.Steps {
		if step.Context == nil || !step.Context.Steering {
			continue
		}
		for _, item := range step.Context.Items {
			if item.ID == id {
				return true
			}
		}
	}
	return false
}

// A steer that misses its turn is not lost: when the turn ends it moves to
// the queue and the same call runs it; when a new Start closes the turn it
// goes ahead of the new input; while the turn is continuable, it waits for
// its Continue.
func TestSessionMissedSteers(t *testing.T) {
	t.Run("turn ended", func(t *testing.T) {
		rec := dashi.NewMemoryRecorder()
		client := held(1, dashitest.Reply("done"), dashitest.Reply("also done"))
		s := handle(rec, client)
		s.New = true
		// One model call: a steer that arrives during it cannot supersede
		// the answer.
		s.Agent.Limits.ModelAttempts = 1
		done := sendAsync(t.Context(), s, "start")
		entered(t, client)
		q, err := s.Steer(t.Context(), "also this")
		if err != nil || q.TurnID != "s-1" {
			t.Fatalf("queued=%+v err=%v", q, err)
		}
		close(client.gate.release)
		out := awaitSend(t, done)
		if out.Turn.ID != "s-2" || out.Answer != "also done" {
			t.Fatalf("turn=%s answer=%q", out.Turn.ID, out.Answer)
		}
		if got := input(t, rec, "s-2"); !slices.Equal(got, []string{"also this"}) {
			t.Fatalf("input=%q", got)
		}
		if got := out.Turn.Steps[0].CommandID; got != `queue:["`+q.ID+`"]` {
			t.Fatalf("command ID=%q", got)
		}
	})
	t.Run("turn closed", func(t *testing.T) {
		rec := dashi.NewMemoryRecorder()
		client := held(1, dashitest.Reply("fresh"))
		s := handle(rec, client)
		s.New = true
		ctx, cancel := context.WithCancel(t.Context())
		done := sendAsync(ctx, s, "start")
		entered(t, client)
		if _, err := s.Steer(t.Context(), "wait"); err != nil {
			t.Fatal(err)
		}
		cancel()
		if r := <-done; r.err != nil || r.out.Next() != dashi.NextContinue {
			t.Fatalf("next=%s err=%v", r.out.Next(), r.err)
		}
		close(client.gate.release)
		out, err := s.Send(t.Context(), "new")
		if err != nil || out.Turn.ID != "s-2" || out.Answer != "fresh" {
			t.Fatalf("turn=%s answer=%q err=%v", out.Turn.ID, out.Answer, err)
		}
		if !hasWarning(out.SessionWarnings, dashi.WarningTurnClosed) {
			t.Fatalf("warnings=%+v", out.SessionWarnings)
		}
		if got := input(t, rec, "s-2"); !slices.Equal(got, []string{"wait", "new"}) {
			t.Fatalf("input=%q", got)
		}
		closed := load(t, rec, "s-1")
		if closed.Stop.Kind != dashi.StopCanceled || closed.Stop.Reason != dashi.CloseReason {
			t.Fatalf("closed stop=%+v", closed.Stop)
		}
		if n := queued(t, s); n != 0 {
			t.Fatalf("queued=%d", n)
		}
	})
	t.Run("turn continued", func(t *testing.T) {
		rec := dashi.NewMemoryRecorder()
		client := held(1, dashitest.Reply("resumed"))
		s := handle(rec, client)
		s.New = true
		ctx, cancel := context.WithCancel(t.Context())
		done := sendAsync(ctx, s, "start")
		entered(t, client)
		q, err := s.Steer(t.Context(), "wait")
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		<-done
		close(client.gate.release)
		out, err := s.Run(t.Context(), dashi.Continue{})
		if err != nil || out.Turn.ID != "s-1" || out.Answer != "resumed" {
			t.Fatalf("turn=%s answer=%q err=%v", out.Turn.ID, out.Answer, err)
		}
		if !recordsSteer(out.Turn, q.ID) {
			t.Fatal("the continued turn did not record the steer")
		}
		requireTurns(t, rec, 1)
	})
}

// Queued input under a durable recorder warns memory_only; the in-memory
// recorder's queue lives as long as its record, so it does not.
func TestSessionQueueMemoryOnly(t *testing.T) {
	durable := handle(newLiveRecorder(), dashitest.NewClient())
	durable.New = true
	q, err := durable.Enqueue(t.Context(), "later")
	if err != nil || !hasWarning(q.Warnings, dashi.WarningMemoryOnly) {
		t.Fatalf("queued=%+v err=%v", q, err)
	}
	q, err = durable.Steer(t.Context(), "idle steer")
	if err != nil || !hasWarning(q.Warnings, dashi.WarningMemoryOnly) {
		t.Fatalf("queued=%+v err=%v", q, err)
	}
	for _, s := range []*dashi.Session{
		handle(dashi.NewMemoryRecorder(), dashitest.NewClient()),
		{Runner: dashi.Runner{Client: dashitest.NewClient()}},
	} {
		if q, err := s.Enqueue(t.Context(), "later"); err != nil || len(q.Warnings) != 0 {
			t.Fatalf("queued=%+v err=%v", q, err)
		}
	}
}

// A user item queued before a caller's Start goes ahead of its input;
// items of other origins run as a queue turn after it, with origin
// StartScheduled.
func TestSessionStartTakesUserItems(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(
		dashitest.Reply("first"),
		dashitest.Reply("second"),
		dashitest.Reply("notice read"),
	)
	s := handle(rec, client)
	s.New = true
	if _, err := s.Send(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	notice, err := s.EnqueueItem(t.Context(), dashi.ContextItem{
		Origin:  dashi.ContextTool,
		Message: llm.UserMessage(llm.Text("[task done]")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(t.Context(), "typed earlier"); err != nil {
		t.Fatal(err)
	}
	out, err := s.Send(t.Context(), "now")
	if err != nil {
		t.Fatal(err)
	}
	if got := input(t, rec, "s-2"); !slices.Equal(got, []string{"typed earlier", "now"}) {
		t.Fatalf("input=%q", got)
	}
	if out.Turn.ID != "s-3" || out.Origin != dashi.StartScheduled ||
		out.Turn.Steps[0].CommandID != `queue:["`+notice.ID+`"]` {
		t.Fatalf("turn=%s origin=%q key=%q", out.Turn.ID, out.Origin,
			out.Turn.Steps[0].CommandID)
	}
	if got := input(t, rec, "s-3"); !slices.Equal(got, []string{"[task done]"}) {
		t.Fatalf("input=%q", got)
	}
	// The retried Start still finds its turn: its input ends with the
	// retry's input.
	start := dashi.Say("again")
	start.CommandID = "k"
	if _, err := s.Enqueue(t.Context(), "before again"); err != nil {
		t.Fatal(err)
	}
	first, err := s.Run(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.Run(t.Context(), start)
	if err != nil || retry.Turn.ID != first.Turn.ID {
		t.Fatalf("retry turn=%s, want %s; err=%v", retry.Turn.ID, first.Turn.ID, err)
	}
}

// Run's result keeps the session warnings of the turns before its last.
func TestSessionDispatchKeepsWarnings(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := held(1, dashitest.Reply("two"), dashitest.Reply("three"))
	s := handle(rec, client)
	s.New = true
	ctx, cancel := context.WithCancel(t.Context())
	done := sendAsync(ctx, s, "first")
	entered(t, client)
	cancel()
	<-done
	close(client.gate.release)
	if _, err := s.EnqueueItem(t.Context(), dashi.ContextItem{
		Origin:  dashi.ContextTool,
		Message: llm.UserMessage(llm.Text("note")),
	}); err != nil {
		t.Fatal(err)
	}
	out, err := s.Send(t.Context(), "new")
	if err != nil || out.Turn.ID != "s-3" || out.Answer != "three" {
		t.Fatalf("turn=%s answer=%q err=%v", out.Turn.ID, out.Answer, err)
	}
	if !hasWarning(out.SessionWarnings, dashi.WarningTurnClosed) {
		t.Fatalf("warnings=%+v", out.SessionWarnings)
	}
}
