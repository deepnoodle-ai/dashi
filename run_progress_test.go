package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func progressTool(
	name string,
	fn func(context.Context, tool.Call) (tool.Outcome, error),
) tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:   name,
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(fn),
	}
}

// namedCallsResult requests one call per name, with IDs c0, c1, and so on.
func namedCallsResult(names ...string) llm.Result {
	var parts []llm.Content
	for i, name := range names {
		call := tool.Call{
			ID:   fmt.Sprintf("c%d", i),
			Name: name,
			Args: []byte(`{}`),
		}
		parts = append(
			parts,
			llm.Content{Kind: llm.ContentToolCall, ToolCall: &call},
		)
	}
	return llm.Result{
		Message:   llm.AssistantMessage(parts...),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
}

// oneCall scripts a model that calls name once, then replies.
func oneCall(name string) []llm.Result {
	return []llm.Result{callResult("call", name), final("ok")}
}

func progressRunner(results []llm.Result, tools ...tool.Tool) Runner {
	return Runner{
		Client:   &scriptedClient{results: results},
		Tools:    tool.List(tools),
		Recorder: NewMemoryRecorder(),
	}
}

// progressStart starts turn "t1" with the given tool concurrency.
func progressStart(limit int) Start {
	start := streamStart()
	start.Agent.Limits.ToolConcurrency = limit
	return start
}

// progressOrder checks that every progress event for a call falls between
// the call's tool_started step and its finish step, and returns the progress
// events and final outcome.
type progressOrder struct {
	t        *testing.T
	started  map[string]bool
	finished map[string]bool
	progress []ToolProgress
	outcome  *RunResult
}

func newProgressOrder(t *testing.T) *progressOrder {
	return &progressOrder{
		t:        t,
		started:  map[string]bool{},
		finished: map[string]bool{},
	}
}

func (o *progressOrder) add(event Event) {
	o.t.Helper()
	switch {
	case event.Step != nil && event.Step.Tool != nil:
		id := event.Step.Tool.Call.ID
		switch event.Step.Kind {
		case StepToolStarted:
			o.started[id] = true
		case StepToolFinished, StepToolWaiting:
			o.finished[id] = true
		}
	case event.Progress != nil:
		p := event.Progress
		if event.TurnID != "t1" {
			o.t.Fatalf("progress turn=%q", event.TurnID)
		}
		if !o.started[p.CallID] || o.finished[p.CallID] {
			o.t.Fatalf(
				"progress for %s outside its run: started=%v finished=%v",
				p.CallID,
				o.started[p.CallID],
				o.finished[p.CallID],
			)
		}
		o.progress = append(o.progress, *p)
	case event.Result != nil:
		o.outcome = event.Result
	}
}

func (o *progressOrder) stream(r Runner, start Start) {
	o.t.Helper()
	events := r.Stream(o.t.Context(), Turn{ID: "t1"}, start)
	for event, err := range events {
		if err != nil {
			o.t.Fatal(err)
		}
		o.add(event)
	}
}

func TestStreamDeliversToolProgress(t *testing.T) {
	seen := make(chan float64)
	work := progressTool("work", func(
		ctx context.Context,
		_ tool.Call,
	) (tool.Outcome, error) {
		for i := 1; i <= 3; i++ {
			tool.Report(ctx, tool.Progress{
				Message: fmt.Sprintf("%d/3", i),
				Done:    float64(i),
				Total:   3,
				Data:    json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
			})
			select {
			case <-seen:
			case <-time.After(5 * time.Second):
				return tool.Outcome{}, errors.New("progress not delivered")
			}
		}
		tool.Report(ctx, tool.Progress{Message: "done", Done: 3, Total: 3})
		return tool.Succeeded("ok"), nil
	})
	order := newProgressOrder(t)
	runner := progressRunner(oneCall("work"), work)
	events := runner.Stream(t.Context(), Turn{ID: "t1"}, progressStart(1))
	for event, err := range events {
		if err != nil {
			t.Fatal(err)
		}
		order.add(event)
		if p := event.Progress; p != nil && p.Message != "done" {
			seen <- p.Done
		}
	}
	var got []string
	for _, p := range order.progress {
		if p.CallID != "call" || p.Name != "work" || p.Total != 3 {
			t.Fatalf("progress=%+v", p)
		}
		got = append(got, p.Message+string(p.Data))
	}
	want := `1/3{"i":1},2/3{"i":2},3/3{"i":3},done`
	if strings.Join(got, ",") != want {
		t.Fatalf("progress=%v", got)
	}
	if order.outcome == nil || order.outcome.Stop.Kind != StopCompleted {
		t.Fatalf("outcome=%+v", order.outcome)
	}
	record, err := json.Marshal(order.outcome.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(record), "1/3") {
		t.Fatal("progress was recorded")
	}
}

func TestSlowStreamConsumerDoesNotBlockReporting(t *testing.T) {
	const reports = 10000
	returned := make(chan struct{})
	work := progressTool("work", func(
		ctx context.Context,
		_ tool.Call,
	) (tool.Outcome, error) {
		defer close(returned)
		for i := 1; i <= reports; i++ {
			tool.Report(ctx, tool.Progress{Done: float64(i), Total: reports})
		}
		return tool.Succeeded("ok"), nil
	})
	order := newProgressOrder(t)
	runner := progressRunner(oneCall("work"), work)
	blocked := false
	events := runner.Stream(t.Context(), Turn{ID: "t1"}, progressStart(1))
	for event, err := range events {
		if err != nil {
			t.Fatal(err)
		}
		order.add(event)
		if event.Progress != nil && !blocked {
			blocked = true
			select {
			case <-returned:
			case <-time.After(5 * time.Second):
				t.Fatal("a blocked consumer stalled the tool")
			}
		}
	}
	n := len(order.progress)
	if n == 0 || n > reports {
		t.Fatalf("events=%d", n)
	}
	if last := order.progress[n-1]; last.Done != reports {
		t.Fatalf("last=%+v", last)
	}
}

func TestParallelToolProgressUnderRace(t *testing.T) {
	report := func(ctx context.Context, call tool.Call) (tool.Outcome, error) {
		var wg sync.WaitGroup
		for g := range 3 {
			wg.Go(func() {
				for i := range 500 {
					tool.Report(ctx, tool.Progress{
						Message: call.ID,
						Done:    float64(i),
						Data:    json.RawMessage(fmt.Sprintf(`{"g":%d}`, g)),
					})
				}
			})
		}
		wg.Wait()
		return tool.Succeeded("ok"), nil
	}
	names := []string{"a", "b", "c", "d", "e"}
	var tools []tool.Tool
	for _, name := range names {
		tools = append(tools, progressTool(name, report))
	}
	for _, limit := range []int{1, 4} {
		t.Run(fmt.Sprintf("concurrency %d", limit), func(t *testing.T) {
			order := newProgressOrder(t)
			order.stream(progressRunner(
				[]llm.Result{namedCallsResult(names...), final("ok")},
				tools...,
			), progressStart(limit))
			for _, p := range order.progress {
				if p.Message != p.CallID ||
					names[p.CallID[1]-'0'] != p.Name {
					t.Fatalf("misattributed progress %+v", p)
				}
			}
			if order.outcome == nil || order.outcome.Stop.Kind != StopCompleted {
				t.Fatalf("outcome=%+v", order.outcome)
			}
			if len(order.finished) != len(names) {
				t.Fatalf("finished=%v", order.finished)
			}
		})
	}
}

func TestProgressBeforePanicIsDelivered(t *testing.T) {
	work := progressTool("work", func(
		ctx context.Context,
		_ tool.Call,
	) (tool.Outcome, error) {
		tool.Report(ctx, tool.Progress{Message: "about to fail"})
		panic("boom")
	})
	order := newProgressOrder(t)
	order.stream(progressRunner([]llm.Result{callResult("call", "work")}, work), progressStart(1))
	if len(order.progress) != 1 || order.progress[0].Message != "about to fail" {
		t.Fatalf("progress=%+v", order.progress)
	}
	if order.outcome == nil || order.outcome.Stop.Kind != StopResultUnknown {
		t.Fatalf("outcome=%+v", order.outcome)
	}
}

// A call that hands off work keeps a leaked goroutine reporting while a
// second call holds the runner in its drain loop, so a late report would be
// delivered if the finished call's slot still accepted or forwarded reports.
func TestReportsAfterExecuteReturnsAreDropped(t *testing.T) {
	for _, state := range []tool.State{tool.StateWaiting, tool.StateDetached} {
		t.Run(string(state), func(t *testing.T) {
			stop := make(chan struct{})
			var leaked sync.WaitGroup
			defer func() {
				close(stop)
				leaked.Wait()
			}()
			handedOff := make(chan struct{})
			var late atomic.Int64
			work := progressTool("work", func(
				ctx context.Context,
				_ tool.Call,
			) (tool.Outcome, error) {
				tool.Report(ctx, tool.Progress{Message: "handing off"})
				leaked.Go(func() {
					<-handedOff
					for {
						select {
						case <-stop:
							return
						default:
							tool.Report(ctx, tool.Progress{Message: "late"})
							late.Add(1)
							time.Sleep(100 * time.Microsecond)
						}
					}
				})
				out := tool.Outcome{State: state}
				if state == tool.StateWaiting {
					out.Wait = &tool.Wait{Reason: "later"}
				} else {
					out.Handle = &tool.Handle{ID: "job"}
				}
				return out, nil
			})
			hold := progressTool("hold", func(
				ctx context.Context,
				_ tool.Call,
			) (tool.Outcome, error) {
				select {
				case <-handedOff:
				case <-time.After(5 * time.Second):
					return tool.Outcome{}, errors.New("work never waited")
				}
				for late.Load() < 50 {
					time.Sleep(100 * time.Microsecond)
				}
				return tool.Succeeded("ok"), nil
			})
			runner := progressRunner(
				[]llm.Result{namedCallsResult("work", "hold")},
				work,
				hold,
			)
			runner.Recorder = notifyingRecorder{
				Recorder: runner.Recorder,
				onCommit: func(step Step) {
					if step.Kind == StepToolWaiting {
						close(handedOff)
					}
				},
			}
			order := newProgressOrder(t)
			// progressOrder fails on any report delivered after tool_waiting.
			order.stream(runner, progressStart(2))
			if len(order.progress) == 0 ||
				order.progress[0].Message != "handing off" {
				t.Fatalf("progress=%+v", order.progress)
			}
			if order.outcome == nil || order.outcome.Stop.Kind != StopWaiting {
				t.Fatalf("outcome=%+v", order.outcome)
			}
		})
	}
}

func TestBreakingStreamWhileToolReports(t *testing.T) {
	exited := make(chan struct{})
	work := progressTool("work", func(
		ctx context.Context,
		_ tool.Call,
	) (tool.Outcome, error) {
		defer close(exited)
		for ctx.Err() == nil {
			tool.Report(ctx, tool.Progress{Message: "working"})
			time.Sleep(time.Millisecond)
		}
		return tool.Outcome{}, ctx.Err()
	})
	runner := progressRunner(
		[]llm.Result{callResult("call", "work")},
		work,
	)
	recorder := runner.Recorder.(*MemoryRecorder)
	events := runner.Stream(t.Context(), Turn{ID: "t1"}, progressStart(1))
	for event, err := range events {
		if err != nil {
			t.Fatal(err)
		}
		if event.Progress != nil {
			break
		}
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("tool kept running after the stream ended")
	}
	turn, err := recorder.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := turn.View()
	if err != nil || view.Stop.Kind != StopInterrupted {
		t.Fatalf("stop=%+v err=%v", view.Stop, err)
	}
}

func TestNestedRunnerProgressStaysInItsTurn(t *testing.T) {
	inner := progressTool("inner", func(
		ctx context.Context,
		_ tool.Call,
	) (tool.Outcome, error) {
		tool.Report(ctx, tool.Progress{Message: "inner"})
		return tool.Succeeded("ok"), nil
	})
	outer := progressTool("outer", func(
		ctx context.Context,
		_ tool.Call,
	) (tool.Outcome, error) {
		tool.Report(ctx, tool.Progress{Message: "outer"})
		child := progressRunner(
			[]llm.Result{callResult("child-call", "inner"), final("ok")},
			inner,
		)
		out, err := child.Run(ctx, Turn{ID: "child"}, progressStart(1))
		if err != nil || out.Stop.Kind != StopCompleted {
			return tool.Outcome{}, fmt.Errorf("child: %v %+v", err, out.Stop)
		}
		return tool.Succeeded("ok"), nil
	})
	order := newProgressOrder(t)
	order.stream(progressRunner(
		[]llm.Result{callResult("call", "outer"), final("ok")},
		outer,
	), progressStart(1))
	if len(order.progress) != 1 || order.progress[0].Message != "outer" {
		t.Fatalf("progress=%+v", order.progress)
	}
}

func TestRunWithoutListenerShadowsOuterReporter(t *testing.T) {
	outerReports := 0
	ctx := tool.WithReporter(t.Context(), func(tool.Progress) {
		outerReports++
	})
	work := progressTool("work", func(
		ctx context.Context,
		_ tool.Call,
	) (tool.Outcome, error) {
		tool.Report(ctx, tool.Progress{Message: "nobody listens"})
		return tool.Succeeded("ok"), nil
	})
	runner := progressRunner(oneCall("work"), work)
	out, err := runner.Run(ctx, Turn{ID: "t1"}, progressStart(1))
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if outerReports != 0 {
		t.Fatalf("outer reporter received %d reports", outerReports)
	}
}

func TestObserverAndStreamGetSeparateProgressCopies(t *testing.T) {
	work := progressTool("work", func(
		ctx context.Context,
		_ tool.Call,
	) (tool.Outcome, error) {
		tool.Report(ctx, tool.Progress{
			Message: "m",
			Data:    json.RawMessage(`{"n":1}`),
		})
		return tool.Succeeded("ok"), nil
	})
	var mu sync.Mutex
	var observed []ToolProgress
	stopped := make(chan struct{})
	runner := progressRunner(oneCall("work"), work)
	runner.Observer = func(event Event) {
		mu.Lock()
		defer mu.Unlock()
		if event.Progress != nil {
			observed = append(observed, *event.Progress)
		}
		if event.Step != nil && event.Step.Kind == StepTurnStopped {
			close(stopped)
		}
	}
	order := newProgressOrder(t)
	events := runner.Stream(t.Context(), Turn{ID: "t1"}, progressStart(1))
	for event, err := range events {
		if err != nil {
			t.Fatal(err)
		}
		order.add(event)
		if event.Progress != nil {
			event.Progress.Data[0] = 'X'
		}
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not see the stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order.progress) != 1 || len(observed) != 1 ||
		string(observed[0].Data) != `{"n":1}` {
		t.Fatalf("stream=%+v observed=%+v", order.progress, observed)
	}
}

func TestRunWithObserverDeliversProgress(t *testing.T) {
	work := progressTool("work", func(
		ctx context.Context,
		_ tool.Call,
	) (tool.Outcome, error) {
		tool.Report(ctx, tool.Progress{Message: "observed"})
		return tool.Succeeded("ok"), nil
	})
	got := make(chan string, 1)
	runner := progressRunner(oneCall("work"), work)
	runner.Observer = func(event Event) {
		if event.Progress != nil {
			got <- event.Progress.Message
		}
	}
	_, err := runner.Run(t.Context(), Turn{ID: "t1"}, progressStart(1))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m != "observed" {
			t.Fatalf("message=%q", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("observer received no progress")
	}
}

// Progress fills at most half of a parked Observer's queue, and deltas then
// evict it, so every delta that fits the full queue still arrives.
func TestObserverDeltasEvictQueuedProgress(t *testing.T) {
	e, drain := parkedEmitter(t)
	for range 1000 {
		e.sendProgress(Event{Progress: &ToolProgress{CallID: "c"}})
	}
	if n := e.len(); n != observerQueue/2 {
		t.Fatalf("progress queued=%d, want %d", n, observerQueue/2)
	}
	for i := range observerQueue {
		e.send(delta(i))
	}
	got := kinds(drain())
	if want := slices.Repeat([]string{"d"}, observerQueue); !slices.Equal(got, want) {
		t.Fatalf("events=%v, want only the %d deltas", got, observerQueue)
	}
}

// Breaking a Stream on the first progress event cancels the turn; snapshots
// still pending in other calls' slots must not reach the Observer.
func TestStreamBreakStopsObserverProgress(t *testing.T) {
	const reporters = 3
	var reported sync.WaitGroup
	reported.Add(reporters)
	report := progressTool("report", func(
		ctx context.Context,
		call tool.Call,
	) (tool.Outcome, error) {
		tool.Report(ctx, tool.Progress{Message: call.ID})
		reported.Done()
		<-ctx.Done()
		return tool.Outcome{}, ctx.Err()
	})
	hold := progressTool("hold", func(
		ctx context.Context,
		_ tool.Call,
	) (tool.Outcome, error) {
		<-ctx.Done()
		return tool.Outcome{}, ctx.Err()
	})
	runner := progressRunner(
		[]llm.Result{namedCallsResult("report", "report", "report", "hold")},
		report,
		hold,
	)
	// Hold the runner before it starts the last call, so every reporter's
	// snapshot is pending when it next drains.
	runner.Recorder = notifyingRecorder{
		Recorder: runner.Recorder,
		onCommit: func(step Step) {
			if step.Kind == StepToolStarted &&
				step.Tool.Call.ID == fmt.Sprintf("c%d", reporters) {
				reported.Wait()
			}
		},
	}
	var mu sync.Mutex
	observed := 0
	stopped := make(chan struct{})
	runner.Observer = func(event Event) {
		mu.Lock()
		defer mu.Unlock()
		if event.Progress != nil {
			observed++
		}
		if event.Step != nil && event.Step.Kind == StepTurnStopped {
			close(stopped)
		}
	}
	events := runner.Stream(t.Context(), Turn{ID: "t1"}, progressStart(reporters+1))
	for event, err := range events {
		if err != nil {
			t.Fatal(err)
		}
		if event.Progress != nil {
			break
		}
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not see the stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if observed != 1 {
		t.Fatalf("observer received %d progress events, want 1", observed)
	}
}

// A parked Observer must still receive the turn's later steps while a tool
// floods progress, because progress uses at most half of its queue.
func TestProgressFloodKeepsObserverSteps(t *testing.T) {
	const reports = 200
	ack := make(chan struct{})
	work := progressTool("work", func(
		ctx context.Context,
		_ tool.Call,
	) (tool.Outcome, error) {
		for i := 1; i <= reports; i++ {
			tool.Report(ctx, tool.Progress{Done: float64(i)})
			select {
			case <-ack:
			case <-time.After(5 * time.Second):
				return tool.Outcome{}, errors.New("progress not delivered")
			}
		}
		return tool.Succeeded("ok"), nil
	})
	release := make(chan struct{})
	stopped := make(chan struct{})
	runner := progressRunner(oneCall("work"), work)
	runner.Observer = func(event Event) {
		<-release
		if event.Step != nil && event.Step.Kind == StepTurnStopped {
			close(stopped)
		}
	}
	order := newProgressOrder(t)
	events := runner.Stream(t.Context(), Turn{ID: "t1"}, progressStart(1))
	for event, err := range events {
		if err != nil {
			t.Fatal(err)
		}
		order.add(event)
		if event.Progress != nil {
			ack <- struct{}{}
		}
	}
	if len(order.progress) != reports {
		t.Fatalf("stream progress=%d", len(order.progress))
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("observer lost the stop step to progress")
	}
}
