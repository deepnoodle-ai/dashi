package feed

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

// streamClient answers each call with a final message after streaming
// words as deltas. When hold is set, it waits on it after the first delta.
type streamClient struct {
	words []string
	hold  chan struct{}
	// read, when set, holds the reply until the reader has consumed the
	// deltas: a delta the Observer delivers after its settling step is
	// rightly dropped, which would make a test's expected shape racy.
	read chan struct{}
	// stop is the model's stop; zero means finished.
	stop llm.StopKind
}

func (*streamClient) Provider() string                       { return "test" }
func (*streamClient) API() string                            { return "test.chat" }
func (*streamClient) ModelInfo(string) (llm.ModelInfo, bool) { return llm.ModelInfo{}, false }

func (*streamClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	return llm.NewRequest(llm.RequestSnapshot{
		Origin:    content.Origin{Provider: "test", API: "test.chat", Model: in.Model, Scope: "fixture"},
		Input:     in,
		Effective: in.Settings,
		Body:      []byte(`{}`),
	})
}

func (c *streamClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	for i, word := range c.words {
		if call.Sink != nil {
			call.Sink(llm.Delta{Kind: llm.DeltaText, Text: word})
		}
		if i == 0 && c.hold != nil {
			select {
			case <-c.hold:
			case <-ctx.Done():
			}
		}
	}
	if c.read != nil {
		select {
		case <-c.read:
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
	}
	return llm.Result{
		Message:   llm.AssistantMessage(llm.Text(strings.Join(c.words, ""))),
		Stop:      llm.Stop{Kind: cmp.Or(c.stop, llm.StopFinished)},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}, nil
}

func start() dashi.Start {
	return dashi.Start{Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}
}

func runner(client llm.Client, rec dashi.Recorder, observer dashi.Observer) dashi.Runner {
	return dashi.Runner{
		Agent:    dashi.Agent{Model: "fixture"},
		Client:   client,
		Recorder: rec,
		Observer: observer,
	}
}

// readerOnly hides a StepWaiter.
type readerOnly struct{ r dashi.StepReader }

func (r readerOnly) StepsAfter(ctx context.Context, id string, after int64, limit int) ([]dashi.Step, error) {
	return r.r.StepsAfter(ctx, id, after, limit)
}

// collect runs Follow on its own goroutine.
func collect(ctx context.Context, h *Hub, r dashi.StepReader, after int64) func(*testing.T) ([]dashi.Event, error) {
	done := make(chan struct{})
	var events []dashi.Event
	var err error
	go func() {
		defer close(done)
		for e, e2 := range Follow(ctx, h, r, "t1", after) {
			if e2 != nil {
				err = e2
				return
			}
			events = append(events, e)
		}
	}()
	return func(t *testing.T) ([]dashi.Event, error) {
		t.Helper()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("follow did not end")
		}
		return events, err
	}
}

// shape summarizes events as "s<seq>", "d", "p", or "g".
func shape(events []dashi.Event) string {
	var out []string
	for _, e := range events {
		switch {
		case e.Step != nil:
			out = append(out, fmt.Sprintf("s%d", e.Step.Seq))
		case e.Gap:
			out = append(out, "g")
		case e.Delta != nil:
			out = append(out, "d")
		case e.Progress != nil:
			out = append(out, "p")
		}
	}
	return strings.Join(out, " ")
}

// A live reader sees steps with the deltas between model_requested and
// model_settled, and every event carries its place.
func TestFollowMergesPreviewsInPlace(t *testing.T) {
	var hub Hub
	rec := dashi.NewMemoryRecorder()
	client := &streamClient{words: []string{"a", "b", "c"}, read: make(chan struct{})}
	done := make(chan struct{})
	var events []dashi.Event
	var err error
	go func() {
		defer close(done)
		deltas := 0
		for e, e2 := range Follow(t.Context(), &hub, rec, "t1", 0) {
			if e2 != nil {
				err = e2
				return
			}
			events = append(events, e)
			if e.Delta != nil {
				if deltas++; deltas == 3 {
					close(client.read)
				}
			}
		}
	}()
	waitReaders(t, &hub, 1)
	out, runErr := runner(client, rec, hub.Observe).Run(t.Context(), dashi.Turn{ID: "t1"}, start())
	if runErr != nil {
		t.Fatal(runErr)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("follow did not end")
	}
	if err != nil {
		t.Fatal(err)
	}
	if got, want := shape(events), "s1 s2 d d d s3 s4"; got != want {
		t.Fatalf("events %q, want %q", got, want)
	}
	for _, e := range events {
		if e.Delta != nil && e.Seq != 2 {
			t.Fatalf("delta at seq %d; want its model_requested (2)", e.Seq)
		}
	}
	if len(out.Turn.Steps) != 4 {
		t.Fatalf("steps=%d", len(out.Turn.Steps))
	}
	if hub.readers("t1") != 0 {
		t.Fatal("reader left in the hub")
	}
}

// Previews reach a reader promptly even when the record's reader has no
// StepWaiter: steps the hub observes wake the read.
func TestFollowWakesOnObservedSteps(t *testing.T) {
	var hub Hub
	rec := dashi.NewMemoryRecorder()
	client := &streamClient{words: []string{"a", "b"}, hold: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	firstDelta := make(chan time.Duration, 1)
	began := time.Now()
	go func() {
		for e, err := range Follow(ctx, &hub, readerOnly{rec}, "t1", 0) {
			if err != nil {
				return
			}
			if e.Delta != nil {
				firstDelta <- time.Since(began)
				close(client.hold)
				return
			}
		}
	}()
	waitReaders(t, &hub, 1)
	go runner(client, rec, hub.Observe).Run(t.Context(), dashi.Turn{ID: "t1"}, start())
	select {
	case d := <-firstDelta:
		if d > 500*time.Millisecond {
			t.Fatalf("first delta took %v; it waited on the poll", d)
		}
	case <-time.After(5 * time.Second):
		close(client.hold)
		t.Fatal("no delta")
	}
}

// The review's regression: the Observer delivers a message's deltas late,
// after a reader resumed from the step that settled it. Nothing is sent.
func TestFollowDropsPreviewsSettledBeforeCursor(t *testing.T) {
	var hub Hub
	rec := dashi.NewMemoryRecorder()
	var mu sync.Mutex
	var held []dashi.Event
	observer := func(e dashi.Event) {
		mu.Lock()
		defer mu.Unlock()
		held = append(held, e)
	}
	// An output limit stops the turn without ending it, so the reader
	// stays connected.
	client := &streamClient{words: []string{"a", "b"}, stop: llm.StopOutputLimit}
	out, err := runner(client, rec, observer).Run(
		t.Context(), dashi.Turn{ID: "t1"}, start(),
	)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(held) > 0 && held[len(held)-1].Step != nil &&
			held[len(held)-1].Step.Kind == dashi.StepStopped
	})
	settled := int64(3) // model_settled
	if out.Turn.Steps[settled-1].Kind != dashi.StepModelSettled {
		t.Fatalf("seq 3 is %s", out.Turn.Steps[settled-1].Kind)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	follow := collect(ctx, &hub, rec, settled)
	waitReaders(t, &hub, 1)
	for _, e := range held {
		hub.Observe(e)
	}
	events, err := follow(t)
	if err != nil {
		t.Fatal(err)
	}
	if got := shape(events); got != "s4" {
		t.Fatalf("events %q; want only the stop", got)
	}
}

// Steps from overlapping runs may reach the hub out of order; placement
// uses each event's own Seq, so an older preview stays out.
func TestFollowPlacesPreviewsDespiteOutOfOrderSteps(t *testing.T) {
	m := merger{cursor: 4, lost: map[string]bool{}, heldLost: map[string]bool{}}
	var got []dashi.Event
	m.yield = func(e dashi.Event, _ error) bool { got = append(got, e); return true }
	delta := dashi.Event{TurnID: "t1", MessageID: "m", Seq: 3, Delta: &llm.Delta{Text: "old"}}
	m.place([]dashi.Event{delta})
	if len(got) != 0 {
		t.Fatalf("yielded a preview placed before the cursor: %+v", got)
	}
	var hub Hub
	hub.Observe(dashi.Event{TurnID: "t1", Seq: 5, Step: &dashi.Step{TurnID: "t1", Seq: 5}})
	rd := hub.subscribe("t1")
	defer hub.unsubscribe("t1", rd)
	hub.Observe(delta)
	m.place(hub.take("t1", rd))
	if len(got) != 0 {
		t.Fatalf("yielded a preview placed before the cursor: %+v", got)
	}
}

// Previews held for a step come out right after it, before the next step of
// the same page.
func TestFollowReleasesHeldPreviewsWithinAPage(t *testing.T) {
	var got []dashi.Event
	m := merger{cursor: 1, lost: map[string]bool{}, heldLost: map[string]bool{}}
	m.yield = func(e dashi.Event, _ error) bool { got = append(got, e); return true }
	m.place([]dashi.Event{
		{MessageID: "m", Seq: 2, Delta: &llm.Delta{Text: "x"}},
		{MessageID: "m", Seq: 2, Delta: &llm.Delta{Text: "y"}},
		{MessageID: "n", Seq: 3, Delta: &llm.Delta{Text: "z"}},
	})
	if len(got) != 0 {
		t.Fatal("yielded before the step")
	}
	for seq := int64(2); seq <= 4; seq++ {
		got = append(got, dashi.Event{Step: &dashi.Step{Seq: seq}})
		m.cursor = seq
		m.release()
	}
	if s := shape(got); s != "s2 d d s3 d s4" {
		t.Fatalf("events %q", s)
	}
	if len(m.held) != 0 {
		t.Fatalf("held %d after the cursor passed them", len(m.held))
	}
}

// A reader that joins while a message streams gets a gap for it, never a
// message missing its start; one resuming at model_requested gets the same.
func TestFollowGapsAMessageJoinedMidStream(t *testing.T) {
	for name, after := range map[string]int64{"page load": 0, "reconnect": 2} {
		t.Run(name, func(t *testing.T) {
			var hub Hub
			rec := dashi.NewMemoryRecorder()
			client := &streamClient{words: []string{"a", "b", "c"}, hold: make(chan struct{})}
			sent := make(chan struct{})
			observe := func(e dashi.Event) {
				hub.Observe(e)
				if e.Delta != nil && e.Delta.Text == "a" {
					close(sent)
				}
			}
			done := make(chan error, 1)
			go func() {
				_, err := runner(client, rec, observe).Run(t.Context(), dashi.Turn{ID: "t1"}, start())
				done <- err
			}()
			<-sent // the first delta reached the hub before the reader joined
			follow := collect(t.Context(), &hub, rec, after)
			waitReaders(t, &hub, 1)
			close(client.hold)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			events, err := follow(t)
			if err != nil {
				t.Fatal(err)
			}
			want := "s1 s2 g s3 s4"
			if after == 2 {
				want = "g s3 s4"
			}
			if got := shape(events); got != want {
				t.Fatalf("events %q; want %q", got, want)
			}
		})
	}
}

// A reader that falls behind gets one gap per message and no later deltas
// for it; progress drops silently; another reader is unaffected.
func TestHubOverflow(t *testing.T) {
	var hub Hub
	slow := hub.subscribe("t1")
	fast := hub.subscribe("t1")
	defer hub.unsubscribe("t1", slow)
	defer hub.unsubscribe("t1", fast)
	var fastGot int
	for i := range queueLimit + 50 {
		e := dashi.Event{TurnID: "t1", MessageID: "m", Seq: 2, Delta: &llm.Delta{Text: "x"}}
		hub.Observe(e)
		if i%10 == 0 {
			fastGot += len(hub.take("t1", fast))
		}
	}
	hub.Observe(dashi.Event{TurnID: "t1", Seq: 2, Progress: &dashi.ToolProgress{CallID: "c"}})
	fastGot += len(hub.take("t1", fast))
	events := hub.take("t1", slow)
	if got := len(events); got != queueLimit+1 {
		t.Fatalf("slow reader queued %d; want %d previews and one gap", got, queueLimit+1)
	}
	gaps := 0
	for _, e := range events {
		if e.Gap {
			gaps++
		}
	}
	if gaps != 1 || !events[len(events)-1].Gap {
		t.Fatalf("gaps=%d, last=%+v", gaps, events[len(events)-1])
	}
	// Later deltas for the message stay out; progress has room again.
	hub.Observe(dashi.Event{TurnID: "t1", MessageID: "m", Seq: 2, Delta: &llm.Delta{Text: "y"}})
	if n := len(hub.take("t1", slow)); n != 0 {
		t.Fatalf("delta after a gap queued: %d", n)
	}
	if fastGot != queueLimit+51 {
		t.Fatalf("fast reader got %d", fastGot)
	}
}

// An Observer gap marks the message lost: later deltas for it are dropped,
// and a gap placed before the cursor is dropped too.
func TestObserverGapSuppressesLaterDeltas(t *testing.T) {
	var hub Hub
	rd := hub.subscribe("t1")
	defer hub.unsubscribe("t1", rd)
	hub.Observe(dashi.Event{TurnID: "t1", MessageID: "m", Seq: 2, Gap: true})
	hub.Observe(dashi.Event{TurnID: "t1", MessageID: "m", Seq: 2, Delta: &llm.Delta{Text: "x"}})
	events := hub.take("t1", rd)
	if s := shape(events); s != "g" {
		t.Fatalf("queued %q", s)
	}
	var got []dashi.Event
	m := merger{cursor: 3, lost: map[string]bool{}, heldLost: map[string]bool{}}
	m.yield = func(e dashi.Event, _ error) bool { got = append(got, e); return true }
	m.place(events)
	if len(got) != 0 {
		t.Fatal("yielded a gap placed before the cursor")
	}
}

func TestNilHub(t *testing.T) {
	var hub *Hub
	hub.Observe(dashi.Event{TurnID: "t1", Seq: 2, Delta: &llm.Delta{Text: "x"}})
	rec := dashi.NewMemoryRecorder()
	if _, err := runner(&streamClient{words: []string{"a"}}, rec, hub.Observe).Run(
		t.Context(), dashi.Turn{ID: "t1"}, start(),
	); err != nil {
		t.Fatal(err)
	}
	events, err := collect(t.Context(), nil, rec, 0)(t)
	if err != nil || shape(events) != "s1 s2 s3 s4" {
		t.Fatalf("events %q err=%v", shape(events), err)
	}
}

// Every way out of the loop unsubscribes and stops the record's reader.
func TestFollowCleansUpOnExit(t *testing.T) {
	var hub Hub
	rec := dashi.NewMemoryRecorder()
	if _, err := runner(&streamClient{words: []string{"a"}}, rec, hub.Observe).Run(
		t.Context(), dashi.Turn{ID: "t1"}, start(),
	); err != nil {
		t.Fatal(err)
	}
	base := runtime.NumGoroutine()
	// Break after the first step.
	for range Follow(t.Context(), &hub, rec, "t1", 0) {
		break
	}
	// Context ends while waiting on a turn with no steps.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	for range Follow(ctx, &hub, rec, "other", 0) {
	}
	cancel()
	// A terminal stop while previews are held.
	rd := &hub
	go func() {
		time.Sleep(10 * time.Millisecond)
		rd.Observe(dashi.Event{TurnID: "t1", MessageID: "m", Seq: 9, Delta: &llm.Delta{Text: "x"}})
	}()
	for range Follow(t.Context(), &hub, rec, "t1", 0) {
	}
	// Cleanup is synchronous: Follow has returned, so the hub is empty.
	if n := hub.entries(); n != 0 {
		t.Fatalf("hub holds %d turns after every reader left", n)
	}
	waitFor(t, func() bool { return runtime.NumGoroutine() <= base })
}

// Readers leaving a resting turn in quick succession leave no hub entry
// behind, even when the record's reader is mid-wait as they leave.
func TestManyDisconnectsLeaveNoEntries(t *testing.T) {
	var hub Hub
	rec := dashi.NewMemoryRecorder()
	client := &streamClient{words: []string{"a"}, stop: llm.StopOutputLimit}
	// The run is not observed, so the hub has nothing of its own to keep
	// for the turn (it remembers settled messages of observed runs).
	if _, err := runner(client, rec, nil).Run(
		t.Context(), dashi.Turn{ID: "t1"}, start(),
	); err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		for range Follow(t.Context(), &hub, readerOnly{rec}, "t1", int64(i%4)) {
			break
		}
		if n := hub.entries(); n != 0 {
			t.Fatalf("after disconnect %d the hub holds %d turns", i, n)
		}
	}
}

// A turn resting at a stop is read about as often as dashi.Follow alone
// would read it, not every poll interval.
func TestRestingTurnIsNotPolled(t *testing.T) {
	old := followPoll
	followPoll = 50 * time.Millisecond
	t.Cleanup(func() { followPoll = old })
	var hub Hub
	rec := dashi.NewMemoryRecorder()
	client := &streamClient{words: []string{"a"}, stop: llm.StopOutputLimit}
	if _, err := runner(client, rec, hub.Observe).Run(
		t.Context(), dashi.Turn{ID: "t1"}, start(),
	); err != nil {
		t.Fatal(err)
	}
	counter := &countingReader{r: rec}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for range Follow(ctx, &hub, counter, "t1", 0) {
	}
	if n := counter.reads.Load(); n > 3 {
		t.Fatalf("%d reads of a resting turn in 1s at a %v poll", n, followPoll)
	}
}

type countingReader struct {
	r     dashi.StepReader
	reads atomic.Int64
}

func (c *countingReader) StepsAfter(ctx context.Context, id string, after int64, limit int) ([]dashi.Step, error) {
	c.reads.Add(1)
	return c.r.StepsAfter(ctx, id, after, limit)
}

// Held previews stay bounded when a reader catching up holds a long message:
// one gap stands for all the deltas past the bound.
func TestHeldGapsAreBounded(t *testing.T) {
	m := merger{cursor: 1, lost: map[string]bool{}, heldLost: map[string]bool{}}
	m.yield = func(dashi.Event, error) bool { return true }
	for range 10_300 {
		m.place([]dashi.Event{{MessageID: "m", Seq: 2, Delta: &llm.Delta{Text: "x"}}})
	}
	m.place([]dashi.Event{{MessageID: "m", Seq: 2, Gap: true}})
	if n := len(m.held); n != queueLimit+1 {
		t.Fatalf("held %d events; want %d deltas and one gap", n, queueLimit+1)
	}
}

// A late delta from an older run does not replace the message streaming
// now, so a reader that joins still gets a gap for the current message.
func TestOpenMessageIgnoresOlderRuns(t *testing.T) {
	var hub Hub
	hub.Observe(dashi.Event{TurnID: "t1", MessageID: "m2", Seq: 8, Delta: &llm.Delta{Text: "x"}})
	hub.Observe(dashi.Event{TurnID: "t1", MessageID: "m1", Seq: 2, Delta: &llm.Delta{Text: "y"}})
	hub.Observe(dashi.Event{TurnID: "t1", Seq: 3, Step: &dashi.Step{
		TurnID: "t1", Seq: 3, Kind: dashi.StepModelSettled,
		Model: &dashi.ModelFact{MessageID: "m1"},
	}})
	rd := hub.subscribe("t1")
	defer hub.unsubscribe("t1", rd)
	events := hub.take("t1", rd)
	if len(events) != 1 || !events[0].Gap || events[0].MessageID != "m2" || events[0].Seq != 8 {
		t.Fatalf("joined with %+v; want a gap for m2 at 8", events)
	}
}

// An Observer gap for a turn nobody follows yet still marks its message
// streaming, so a reader that joins before the next delta gets a gap.
// Once the hub observes a message's settling step, a delayed delta or gap
// from an overlapping run cannot reopen it: readers do not get it, and a
// reader that joins gets no gap for it. A terminal stop forgets the turn.
func TestSettledMessageStaysClosed(t *testing.T) {
	var hub Hub
	rd := hub.subscribe("t1")
	delta := dashi.Event{TurnID: "t1", MessageID: "m", Seq: 2, Delta: &llm.Delta{Text: "x"}}
	hub.Observe(delta)
	hub.Observe(dashi.Event{TurnID: "t1", Seq: 3, Step: &dashi.Step{
		TurnID: "t1", Seq: 3, Kind: dashi.StepModelSettled,
		Model: &dashi.ModelFact{MessageID: "m"},
	}})
	hub.take("t1", rd)
	hub.Observe(delta)
	hub.Observe(dashi.Event{TurnID: "t1", MessageID: "m", Seq: 2, Gap: true})
	if events := hub.take("t1", rd); len(events) != 0 {
		t.Fatalf("a settled message reopened for a reader: %q", shape(events))
	}
	hub.unsubscribe("t1", rd)
	late := hub.subscribe("t1")
	if events := hub.take("t1", late); len(events) != 0 {
		t.Fatalf("a new reader joined with %q; want nothing", shape(events))
	}
	hub.unsubscribe("t1", late)
	hub.Observe(dashi.Event{TurnID: "t1", Seq: 4, Step: &dashi.Step{
		TurnID: "t1", Seq: 4, Kind: dashi.StepStopped,
		Stop: &dashi.Stop{Kind: dashi.StopCompleted, Terminal: true},
	}})
	if n := hub.entries(); n != 0 {
		t.Fatalf("hub holds %d turns after a terminal stop", n)
	}
}

// A reader of a resting turn with no StepWaiter still sees a step committed
// by another process, which no hub observes, within the resting poll.
func TestRestingTurnSeesOtherProcessCommits(t *testing.T) {
	old := followResting
	followResting = 50 * time.Millisecond
	t.Cleanup(func() { followResting = old })
	var hub Hub
	rec := dashi.NewMemoryRecorder()
	resting := &streamClient{words: []string{"a"}, stop: llm.StopOutputLimit}
	first, err := runner(resting, rec, hub.Observe).Run(t.Context(), dashi.Turn{ID: "t1"}, start())
	if err != nil {
		t.Fatal(err)
	}
	follow := collect(t.Context(), &hub, readerOnly{rec}, 0)
	waitReaders(t, &hub, 1)
	time.Sleep(20 * time.Millisecond)
	// Another process resumes the turn: this runner has no Observer.
	began := time.Now()
	out, err := runner(&streamClient{words: []string{"b"}}, rec, nil).Run(
		t.Context(), first.Turn, dashi.Continue{CommandID: "resume"},
	)
	if err != nil || !out.Stop.Terminal {
		t.Fatalf("resume: stop=%+v err=%v", out.Stop, err)
	}
	events, err := follow(t)
	if err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(began); waited > 2*time.Second {
		t.Fatalf("the reader saw the other process's steps after %v", waited)
	}
	last := events[len(events)-1]
	if last.Step == nil || last.Step.Stop == nil || !last.Step.Stop.Terminal {
		t.Fatalf("follow ended at %q", shape(events))
	}
}

func TestObserverGapOpensTheTurn(t *testing.T) {
	var hub Hub
	hub.Observe(dashi.Event{TurnID: "t1", MessageID: "m", Seq: 2, Gap: true})
	rd := hub.subscribe("t1")
	defer hub.unsubscribe("t1", rd)
	events := hub.take("t1", rd)
	if len(events) != 1 || !events[0].Gap || events[0].MessageID != "m" {
		t.Fatalf("joined with %+v; want a gap for m", events)
	}
}

// Many readers join and leave while a run streams.
func TestHubReadersComeAndGo(t *testing.T) {
	var hub Hub
	rec := dashi.NewMemoryRecorder()
	client := &streamClient{words: slices.Repeat([]string{"x"}, 200)}
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for i := range 20 {
		wg.Go(func() {
			for ctx.Err() == nil {
				c, stop := context.WithTimeout(ctx, time.Duration(i%5+1)*time.Millisecond)
				for range Follow(c, &hub, rec, "t1", int64(i%3)) {
				}
				stop()
			}
		})
	}
	if _, err := runner(client, rec, hub.Observe).Run(t.Context(), dashi.Turn{ID: "t1"}, start()); err != nil {
		t.Fatal(err)
	}
	cancel()
	wg.Wait()
	waitFor(t, func() bool { return hub.readers("t1") == 0 })
}

// When the settling step's commit fails, no settle and no gap arrive; the
// reader keeps the deltas it has and ends with its context.
func TestFollowAfterAFailedCommit(t *testing.T) {
	var hub Hub
	rec := dashi.NewMemoryRecorder()
	faulty := &dashitest.FaultRecorder{
		Recorder: rec,
		Kind:     dashi.StepModelSettled,
		Fault:    dashitest.LoseWrite,
	}
	client := &streamClient{words: []string{"a", "b"}, hold: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	follow := collect(ctx, &hub, rec, 0)
	waitReaders(t, &hub, 1)
	close(client.hold)
	_, err := runner(client, faulty, hub.Observe).Run(t.Context(), dashi.Turn{ID: "t1"}, start())
	var commitErr *dashi.CommitError
	if !errors.As(err, &commitErr) {
		t.Fatalf("err=%v; want a commit error", err)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	events, err := follow(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Gap || e.Step != nil && e.Step.Seq > 2 {
			t.Fatalf("events %q", shape(events))
		}
	}
	waitFor(t, func() bool { return hub.readers("t1") == 0 })
}

func waitReaders(t *testing.T, hub *Hub, n int) {
	t.Helper()
	waitFor(t, func() bool { return hub.readers("t1") >= n })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
