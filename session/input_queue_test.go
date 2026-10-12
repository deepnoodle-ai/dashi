package session_test

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
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
	s *session.Session,
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
	out dashi.RunResult
	err error
}

// awaitSend returns the result, failing the test on an error.
func awaitSend(t *testing.T, done <-chan result) dashi.RunResult {
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

func queued(t *testing.T, s *session.Session) int {
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
	if out.Turn.ID != "s-2" || out.Reply != "two" || out.Origin != dashi.StartUser {
		t.Fatalf("turn=%s answer=%q origin=%q", out.Turn.ID, out.Reply, out.Origin)
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
	if err != nil || out.Turn.ID != "s-2" || out.Reply != "two" {
		t.Fatalf("turn=%s answer=%q err=%v", out.Turn.ID, out.Reply, err)
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
	if out.Turn.ID != "s-1" || out.Reply != "done" {
		t.Fatalf("turn=%s answer=%q", out.Turn.ID, out.Reply)
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
	if err != nil || !ran || out.Reply != "queued answer" {
		t.Fatalf("ran=%v answer=%q err=%v", ran, out.Reply, err)
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
	host.Inbox = hostSource{}
	if _, err := host.Steer(t.Context(), "hi"); err == nil {
		t.Fatal("Steer accepted a source that is not an Inbox")
	}
	inbox := dashi.NewMemoryInbox()
	host.Inbox = inbox
	if q, err := host.Steer(t.Context(), "hi"); err != nil || q.TurnID != "" {
		t.Fatalf("queued=%+v err=%v", q, err)
	}
}

type hostSource struct{}

func (hostSource) Waiting(context.Context, string) ([]dashi.Command, error) {
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
		// the reply.
		s.Agent.Limits.ModelCalls = 1
		done := sendAsync(t.Context(), s, "start")
		entered(t, client)
		q, err := s.Steer(t.Context(), "also this")
		if err != nil || q.TurnID != "s-1" {
			t.Fatalf("queued=%+v err=%v", q, err)
		}
		close(client.gate.release)
		out := awaitSend(t, done)
		if out.Turn.ID != "s-2" || out.Reply != "also done" {
			t.Fatalf("turn=%s answer=%q", out.Turn.ID, out.Reply)
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
		if err != nil || out.Turn.ID != "s-2" || out.Reply != "fresh" {
			t.Fatalf("turn=%s answer=%q err=%v", out.Turn.ID, out.Reply, err)
		}
		if !hasWarning(out.SessionWarnings, session.WarningTurnClosed) {
			t.Fatalf("warnings=%+v", out.SessionWarnings)
		}
		if got := input(t, rec, "s-2"); !slices.Equal(got, []string{"wait", "new"}) {
			t.Fatalf("input=%q", got)
		}
		closed := load(t, rec, "s-1")
		if closed.Stop.Kind != dashi.StopCanceled || closed.Stop.Reason != session.CloseReason {
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
		if err != nil || out.Turn.ID != "s-1" || out.Reply != "resumed" {
			t.Fatalf("turn=%s answer=%q err=%v", out.Turn.ID, out.Reply, err)
		}
		if !recordsSteer(out.Turn, q.ID) {
			t.Fatal("the continued turn did not record the steer")
		}
		requireTurns(t, rec, 1)
	})
}

// Delivered drops the queued items it names just before a queue turn:
// alone, they start no turn; beside others, the turn carries only those.
func TestSessionDeliveredDropsQueuedItems(t *testing.T) {
	client := dashitest.NewClient(dashitest.Reply("hi"), dashitest.Reply("read it"))
	s := handle(dashi.NewMemoryRecorder(), client)
	s.New = true
	seen := 0
	s.Delivered = func(turns []dashi.Turn) map[string]bool {
		seen = len(turns)
		return map[string]bool{"read": true}
	}
	if _, err := s.Send(t.Context(), "hi"); err != nil {
		t.Fatal(err)
	}
	item := func(id string) dashi.ContextItem {
		return dashi.ContextItem{ID: id, Message: llm.UserMessage(llm.Text("result " + id))}
	}
	if _, err := s.EnqueueItem(t.Context(), item("read")); err != nil {
		t.Fatal(err)
	}
	if _, ran, err := s.RunQueue(t.Context()); err != nil || ran || seen != 1 {
		t.Fatalf("ran=%v err=%v turns seen=%d", ran, err, seen)
	}
	if st, err := s.Open(t.Context()); err != nil || st.Turns != 1 || st.Queued != 0 {
		t.Fatalf("state=%+v err=%v", st, err)
	}
	for _, id := range []string{"read", "new"} {
		if _, err := s.EnqueueItem(t.Context(), item(id)); err != nil {
			t.Fatal(err)
		}
	}
	out, ran, err := s.RunQueue(t.Context())
	if err != nil || !ran || out.Turn.Steps[0].CommandID != `queue:["new"]` {
		t.Fatalf("ran=%v err=%v turn=%+v", ran, err, out.Turn.Steps[0])
	}
}

// State.QueuedUser counts the queued items from the user, so a host can
// tell them from a background task's notice.
func TestSessionQueuedUserCountsUserItems(t *testing.T) {
	s := handle(dashi.NewMemoryRecorder(), dashitest.NewClient())
	s.New = true
	if _, err := s.Enqueue(t.Context(), "later"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueItem(t.Context(), dashi.ContextItem{
		Origin:  dashi.ContextRuntime,
		Message: llm.UserMessage(llm.Text("task result")),
	}); err != nil {
		t.Fatal(err)
	}
	if st, err := s.Open(t.Context()); err != nil || st.Queued != 2 || st.QueuedUser != 1 {
		t.Fatalf("state=%+v err=%v", st, err)
	}
}

// runtimeItem is a queued item that did not come from the user, such as a
// background task's notice.
func runtimeItem(text string) dashi.ContextItem {
	return dashi.ContextItem{
		Origin:  dashi.ContextRuntime,
		Message: llm.UserMessage(llm.Text(text)),
	}
}

// RunQueueUnlessUserQueued runs nothing while a user item is queued, also
// one that arrives during its queue turn, and never takes a user item
// enqueued at the same time as the call.
func TestSessionRunQueueUnlessUserQueued(t *testing.T) {
	t.Run("user item queued", func(t *testing.T) {
		s := handle(dashi.NewMemoryRecorder(), dashitest.NewClient(dashitest.Reply("no")))
		s.New = true
		if _, err := s.EnqueueItem(t.Context(), runtimeItem("notice")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Enqueue(t.Context(), "later"); err != nil {
			t.Fatal(err)
		}
		if _, ran, err := s.RunQueueUnlessUserQueued(t.Context()); err != nil || ran {
			t.Fatalf("ran=%v err=%v", ran, err)
		}
		if st, err := s.Open(t.Context()); err != nil || st.Turns != 0 || st.Queued != 2 {
			t.Fatalf("state=%+v err=%v", st, err)
		}
	})
	t.Run("user item arrives during the turn", func(t *testing.T) {
		rec := dashi.NewMemoryRecorder()
		client := held(1, dashitest.Reply("one"), dashitest.Reply("two"))
		s := handle(rec, client)
		s.New = true
		if _, err := s.EnqueueItem(t.Context(), runtimeItem("notice")); err != nil {
			t.Fatal(err)
		}
		done := make(chan result, 1)
		go func() {
			out, _, err := s.RunQueueUnlessUserQueued(t.Context())
			done <- result{out, err}
		}()
		entered(t, client)
		if _, err := s.Enqueue(t.Context(), "later"); err != nil {
			t.Fatal(err)
		}
		close(client.gate.release)
		if out := awaitSend(t, done); out.Turn.ID != "s-1" {
			t.Fatalf("last turn=%s, want s-1 only", out.Turn.ID)
		}
		if st, err := s.Open(t.Context()); err != nil || st.Turns != 1 || st.QueuedUser != 1 {
			t.Fatalf("state=%+v err=%v", st, err)
		}
	})
	t.Run("user item enqueued concurrently", func(t *testing.T) {
		for range 50 {
			rec := dashi.NewMemoryRecorder()
			s := handle(rec, dashitest.NewClient(dashitest.Reply("ok"), dashitest.Reply("again")))
			s.New = true
			if _, err := s.EnqueueItem(t.Context(), runtimeItem("notice")); err != nil {
				t.Fatal(err)
			}
			enqueued := make(chan error, 1)
			go func() {
				_, err := s.Enqueue(t.Context(), "later")
				enqueued <- err
			}()
			_, ran, err := s.RunQueueUnlessUserQueued(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := <-enqueued; err != nil {
				t.Fatal(err)
			}
			if ran && slices.Contains(input(t, rec, "s-1"), "later") {
				t.Fatal("the guarded run took the user item")
			}
			st, err := s.Open(t.Context())
			if err != nil || st.QueuedUser != 1 || st.Turns > 1 {
				t.Fatalf("ran=%v state=%+v err=%v", ran, st, err)
			}
		}
	})
}

// Queued input under a durable recorder warns memory_only; the in-memory
// recorder's queue lives as long as its record, so it does not.
func TestSessionQueueMemoryOnly(t *testing.T) {
	durable := handle(newLiveRecorder(), dashitest.NewClient())
	durable.New = true
	q, err := durable.Enqueue(t.Context(), "later")
	if err != nil || !hasWarning(q.Warnings, session.WarningMemoryOnly) {
		t.Fatalf("queued=%+v err=%v", q, err)
	}
	q, err = durable.Steer(t.Context(), "idle steer")
	if err != nil || !hasWarning(q.Warnings, session.WarningMemoryOnly) {
		t.Fatalf("queued=%+v err=%v", q, err)
	}
	for _, s := range []*session.Session{
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
	if err != nil || out.Turn.ID != "s-3" || out.Reply != "three" {
		t.Fatalf("turn=%s answer=%q err=%v", out.Turn.ID, out.Reply, err)
	}
	if !hasWarning(out.SessionWarnings, session.WarningTurnClosed) {
		t.Fatalf("warnings=%+v", out.SessionWarnings)
	}
}

// A queue turn whose queue: key a turn in the idempotency window already
// started is a duplicate: its items were already added, so they are consumed
// and no turn starts.
func TestSessionQueueUsedKey(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	s := handle(rec, dashitest.NewClient(dashitest.Reply("ok")))
	s.New = true
	note := dashi.ContextItem{ID: "n1", Message: llm.UserMessage(llm.Text("note"))}
	if _, err := s.EnqueueItem(t.Context(), note); err != nil {
		t.Fatal(err)
	}
	if _, ran, err := s.RunQueue(t.Context()); err != nil || !ran {
		t.Fatalf("run queue: %v, %v", ran, err)
	}
	if _, err := s.EnqueueItem(t.Context(), note); err != nil {
		t.Fatal(err)
	}
	if _, ran, err := s.RunQueue(t.Context()); err != nil || ran {
		t.Fatalf("second run queue: %v, %v", ran, err)
	}
	if n := queued(t, s); n != 0 {
		t.Fatalf("queued=%d", n)
	}
	requireTurns(t, rec, 1)
}

// An unreadable last turn may hold a queue key, so under Exclusive a
// repeated item fails RunQueue and starts no turn, rather than risk
// adding the items twice.
func TestSessionQueueUsedKeyUnreadable(t *testing.T) {
	rec := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(dashitest.Reply("ok"), dashitest.Reply("again"))
	s := handle(rec, client)
	s.New = true
	note := dashi.ContextItem{ID: "n1", Message: llm.UserMessage(llm.Text("note"))}
	if _, err := s.EnqueueItem(t.Context(), note); err != nil {
		t.Fatal(err)
	}
	if _, ran, err := s.RunQueue(t.Context()); err != nil || !ran {
		t.Fatalf("run queue: %v, %v", ran, err)
	}
	broken := &brokenLoader{MemoryRecorder: rec, id: session.TurnID("s", 1)}
	other := handle(broken, client)
	other.Exclusive = true
	if _, err := other.EnqueueItem(t.Context(), note); err != nil {
		t.Fatal(err)
	}
	if _, ran, err := other.RunQueue(t.Context()); err == nil || ran {
		t.Fatalf("second run queue: %v, %v", ran, err)
	}
	requireTurns(t, rec, 1)
}
