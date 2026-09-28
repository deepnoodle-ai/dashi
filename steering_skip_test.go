package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// batchResult is a model result requesting one call per id, each of the
// tool named by name(id).
func batchResult(name func(id string) string, ids ...string) llm.Result {
	parts := make([]llm.Content, 0, len(ids))
	for _, id := range ids {
		call := tool.Call{ID: id, Name: name(id), Args: []byte(`{}`)}
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

func allWork(string) string { return "work" }

// skipBench runs a batch of "work" calls. Each call runs the function in
// run for its ID, if any, and succeeds. The BeforeTool hook runs onPlan for
// each call it sees; the tool is read-only, so a resumed start re-runs.
type skipBench struct {
	mu     sync.Mutex
	run    map[string]func(ctx context.Context) error
	ran    map[string]int
	onPlan func(id string)
}

func newSkipBench() *skipBench {
	return &skipBench{
		run: map[string]func(context.Context) error{},
		ran: map[string]int{},
	}
}

func (b *skipBench) runs(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ran[id]
}

func (b *skipBench) tool() tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:        "work",
			Schema:      json.RawMessage(`{"type":"object"}`),
			Kind:        tool.KindLocal,
			Annotations: tool.Annotations{ReadOnly: new(true)},
		},
		Executor: execFunc(func(
			ctx context.Context,
			call tool.Call,
		) (tool.Outcome, error) {
			b.mu.Lock()
			b.ran[call.ID]++
			fn := b.run[call.ID]
			b.mu.Unlock()
			if fn != nil {
				if err := fn(ctx); err != nil {
					return tool.Outcome{}, err
				}
			}
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("done " + call.ID)),
			}, nil
		}),
	}
}

func (b *skipBench) hooks() Hooks {
	return ToolHook{
		Name: "watch",
		Func: func(_ context.Context, plan ToolPlan) (ToolDecision, error) {
			if b.onPlan != nil {
				b.onPlan(plan.Call.ID)
			}
			return Allow(""), nil
		},
	}.Hooks()
}

// releaseOn returns a channel closed when the BeforeTool hook first sees
// call id: by then the run goroutine is past that call's start of
// BeforeTool, so a steer sent after the channel closes finds it waiting.
func (b *skipBench) releaseOn(id string) <-chan struct{} {
	ch := make(chan struct{})
	var once sync.Once
	b.onPlan = func(seen string) {
		if seen == id {
			once.Do(func() { close(ch) })
		}
	}
	return ch
}

func skipRunner(
	client llm.Client,
	bench *skipBench,
	concurrency int,
	rec Recorder,
	extra ...tool.Tool,
) Runner {
	agent := testAgent()
	agent.Limits.ToolConcurrency = concurrency
	return Runner{
		Agent:    agent,
		Client:   client,
		Tools:    append(tool.List{bench.tool()}, extra...),
		Hooks:    bench.hooks(),
		Recorder: rec,
	}
}

// requireSkipped checks that call id settled as kept from starting by a
// steer, and whether BeforeTool decided on it first.
func requireSkipped(t *testing.T, turn Turn, id string, decided bool) {
	t.Helper()
	fact := settledFact(t, turn, id)
	if fact.Reason != reasonSteering || fact.Outcome == nil ||
		fact.Outcome.State != tool.StateNotExecuted ||
		modelText(modelToolResult(id, fact.Outcome)) != steerSkipText {
		t.Fatalf("%s settled as %+v", id, fact)
	}
	got := false
	for _, step := range stepsOf(turn, StepToolDecision) {
		got = got || step.Tool.Call.ID == id
	}
	if got != decided {
		t.Fatalf("%s: BeforeTool decided = %t, want %t", id, got, decided)
	}
}

func requireRan(t *testing.T, turn Turn, id string) {
	t.Helper()
	fact := settledFact(t, turn, id)
	if fact.Outcome == nil || fact.Outcome.State != tool.StateSucceeded {
		t.Fatalf("%s settled as %+v", id, fact)
	}
}

// requireResultsThenSteer checks that the request carries each call's
// result, then the steer after them.
func requireResultsThenSteer(
	t *testing.T,
	in llm.Input,
	steer string,
	skipped []string,
	ran ...string,
) {
	t.Helper()
	last := -1
	for i, m := range in.Messages {
		for _, part := range m.Content {
			if part.ToolResult != nil {
				last = i
			}
		}
	}
	for _, id := range skipped {
		result := toolResultFor(in, id)
		if result == nil || resultText(result) != steerSkipText {
			t.Fatalf("skipped %s: result %+v", id, result)
		}
	}
	for _, id := range ran {
		result := toolResultFor(in, id)
		if result == nil || resultText(result) != "done "+id {
			t.Fatalf("ran %s: result %+v", id, result)
		}
	}
	at := hasText(in.Messages, steer)
	if at < 0 || at <= last {
		t.Fatalf("steer at %d, last tool result at %d", at, last)
	}
}

func TestUrgentSteerSkipsLaterCallsInSequentialBatch(t *testing.T) {
	inbox := newWatchedInbox()
	bench := newSkipBench()
	release := bench.releaseOn("b")
	bench.run["a"] = func(context.Context) error {
		<-release
		inbox.sendUrgent(t, "stop, wrong file")
		return nil
	}
	client := newUrgentClient(
		urgentCall{result: batchResult(allWork, "a", "b", "c")},
		urgentCall{result: final("ok")},
	)
	runner := skipRunner(client, bench, 1, nil)
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		startInput(),
		WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	requireRan(t, out.Turn, "a")
	// b was allowed while a ran; c had not reached BeforeTool.
	requireSkipped(t, out.Turn, "b", true)
	requireSkipped(t, out.Turn, "c", false)
	if bench.runs("b")+bench.runs("c") != 0 {
		t.Fatal("a skipped call ran")
	}
	if len(out.Warnings) != 0 {
		t.Fatalf("warnings: %+v", out.Warnings)
	}
	if out.ModelAttempts != 2 {
		t.Fatalf("attempts = %d", out.ModelAttempts)
	}
	requireResultsThenSteer(
		t,
		client.inputs()[1],
		"stop, wrong file",
		[]string{"b", "c"},
		"a",
	)
	// The record replays to the same view.
	view, err := Replay(cloneTurn(out.Turn))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"b", "c"} {
		v := view.Tools[id]
		if v.Status != ToolSettled || modelText(v.Result()) != steerSkipText {
			t.Fatalf("replayed %s: %+v", id, v)
		}
	}
}

func TestUrgentSteerSkipsOnlyUnstartedCallsInParallelBatch(t *testing.T) {
	inbox := newWatchedInbox()
	bench := newSkipBench()
	release := bench.releaseOn("c")
	sent := make(chan struct{})
	bench.run["a"] = func(context.Context) error {
		<-release
		inbox.sendUrgent(t, "stop")
		close(sent)
		return nil
	}
	// b is running when the steer arrives and is not interrupted.
	bench.run["b"] = func(context.Context) error {
		<-sent
		return nil
	}
	client := newUrgentClient(
		urgentCall{result: batchResult(allWork, "a", "b", "c", "d")},
		urgentCall{result: final("ok")},
	)
	runner := skipRunner(client, bench, 2, nil)
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		startInput(),
		WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	requireRan(t, out.Turn, "a")
	requireRan(t, out.Turn, "b")
	// c was allowed and waiting for a slot: allowed to not executed.
	requireSkipped(t, out.Turn, "c", true)
	requireSkipped(t, out.Turn, "d", false)
	requireResultsThenSteer(
		t,
		client.inputs()[1],
		"stop",
		[]string{"c", "d"},
		"a", "b",
	)
	data, err := json.Marshal(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Turn
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	view, err := Replay(decoded)
	if err != nil {
		t.Fatal(err)
	}
	for id, v := range out.Tools {
		if !sameJSONValue(view.Tools[id].Result(), v.Result()) ||
			view.Tools[id].Status != v.Status {
			t.Fatalf("replay differs for %s", id)
		}
	}
	projected, err := Project(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if hasText(projected, "stop") < 0 {
		t.Fatal("the steer is not projected from the decoded record")
	}
}

func TestNonUrgentSteerSkipsNoToolCall(t *testing.T) {
	inbox := newWatchedInbox()
	bench := newSkipBench()
	bench.run["a"] = func(context.Context) error {
		_, err := inbox.SendText("also check the tests")
		return err
	}
	client := newUrgentClient(
		urgentCall{result: batchResult(allWork, "a", "b", "c")},
		urgentCall{result: final("ok")},
	)
	runner := skipRunner(client, bench, 1, nil)
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		startInput(),
		WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	for _, id := range []string{"a", "b", "c"} {
		requireRan(t, out.Turn, id)
	}
	requireResultsThenSteer(
		t,
		client.inputs()[1],
		"also check the tests",
		nil,
		"a", "b", "c",
	)
}

func TestUrgentSteerLeavesApprovalsAndExternalWaitsPending(t *testing.T) {
	inbox := newWatchedInbox()
	bench := newSkipBench()
	release := make(chan struct{})
	var once sync.Once
	approvals := ToolHook{
		Name: "approve",
		Func: func(_ context.Context, plan ToolPlan) (ToolDecision, error) {
			switch plan.Call.ID {
			case "p":
				return Defer("needs a person"), nil
			case "b":
				once.Do(func() { close(release) })
			}
			return Allow(""), nil
		},
	}
	bench.run["a"] = func(context.Context) error {
		<-release
		inbox.sendUrgent(t, "stop")
		return nil
	}
	external := tool.Tool{Definition: tool.Definition{
		Name:   "ext",
		Schema: json.RawMessage(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	names := func(id string) string {
		if id == "e" {
			return "ext"
		}
		return "work"
	}
	client := newUrgentClient(
		urgentCall{result: batchResult(names, "p", "e", "a", "b")},
		urgentCall{result: final("ok")},
	)
	runner := skipRunner(client, bench, 1, nil, external)
	runner.Hooks = approvals.Hooks()
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		startInput(),
		WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopWaiting {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if out.Tools["p"].Status != ToolApprovalPending ||
		out.Tools["e"].Status != ToolWaiting {
		t.Fatalf("p=%s e=%s", out.Tools["p"].Status, out.Tools["e"].Status)
	}
	requireRan(t, out.Turn, "a")
	requireSkipped(t, out.Turn, "b", true)
	for _, step := range stepsOf(out.Turn, StepDelivered) {
		if step.Context.Steering {
			t.Fatal("the steer was recorded with no model call to read it")
		}
	}
	// The host answers both. The approved call runs: it came before the
	// skip, and the steer reaches the next model call after its result.
	for _, pending := range out.Pending {
		var settle Settle
		switch pending.ID {
		case "p":
			settle = pending.Approve()
		case "e":
			settle = pending.Result(tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("done e")),
			})
		}
		out, err = runner.Run(t.Context(), out.Turn, settle, WithSteering(inbox))
		if err != nil {
			t.Fatal(err)
		}
	}
	if out.Stop.Kind != StopCompleted {
		out, err = runner.Run(
			t.Context(),
			out.Turn,
			Continue{CommandID: "go"},
			WithSteering(inbox),
		)
		if err != nil || out.Stop.Kind != StopCompleted {
			t.Fatalf("stop=%+v err=%v", out.Stop, err)
		}
	}
	requireRan(t, out.Turn, "p")
	requireResultsThenSteer(
		t,
		client.inputs()[1],
		"stop",
		[]string{"b"},
		"p", "e", "a",
	)
}

// crashOnSkip fails the commit of call id's steering settlement, as when a
// worker dies right after an earlier skip was written.
type crashOnSkip struct {
	*MemoryRecorder
	id    string
	fired atomic.Bool
}

func (c *crashOnSkip) Commit(ctx context.Context, req CommitRequest) error {
	if req.Step.Kind == StepToolSettled && req.Step.Tool.Call.ID == c.id &&
		req.Step.Tool.Reason == reasonSteering && c.fired.CompareAndSwap(false, true) {
		return errors.New("injected: worker crashed")
	}
	return c.MemoryRecorder.Commit(ctx, req)
}

func TestCrashAfterSteeringSkipResumesFromTheRecord(t *testing.T) {
	inbox := newWatchedInbox()
	bench := newSkipBench()
	release := bench.releaseOn("c")
	bench.run["a"] = func(context.Context) error {
		<-release
		inbox.sendUrgent(t, "stop")
		return nil
	}
	// b is still running when the worker dies; its start is recorded, so
	// the resumed run starts it again instead of skipping it.
	bench.run["b"] = func(ctx context.Context) error {
		if bench.runs("b") == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	client := newUrgentClient(
		urgentCall{result: batchResult(allWork, "a", "b", "c", "d")},
		urgentCall{result: final("ok")},
	)
	rec := &crashOnSkip{MemoryRecorder: NewMemoryRecorder(), id: "d"}
	runner := skipRunner(client, bench, 2, rec)
	if _, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		startInput(),
		WithSteering(inbox),
	); err == nil {
		t.Fatal("the lost settlement was not reported")
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := Replay(saved)
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["b"].Status != ToolStarted ||
		view.Tools["c"].Status != ToolSettled ||
		view.Tools["d"].Status != ToolRequested {
		t.Fatalf(
			"saved b=%s c=%s d=%s",
			view.Tools["b"].Status,
			view.Tools["c"].Status,
			view.Tools["d"].Status,
		)
	}
	// The resumed run has no urgent steer in hand; the record alone keeps
	// d from starting.
	out, err := runner.Run(
		t.Context(),
		saved,
		Continue{CommandID: "resume"},
		WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	requireRan(t, out.Turn, "a")
	requireRan(t, out.Turn, "b")
	requireSkipped(t, out.Turn, "c", true)
	requireSkipped(t, out.Turn, "d", false)
	if bench.runs("b") != 2 || bench.runs("d") != 0 {
		t.Fatalf("b ran %d times, d %d", bench.runs("b"), bench.runs("d"))
	}
	requireResultsThenSteer(
		t,
		client.inputs()[1],
		"stop",
		[]string{"c", "d"},
		"a", "b",
	)
}

func TestReplayAcceptsAllowedToNotExecutedOnlyForSteering(t *testing.T) {
	inbox := newWatchedInbox()
	bench := newSkipBench()
	release := bench.releaseOn("b")
	bench.run["a"] = func(context.Context) error {
		<-release
		inbox.sendUrgent(t, "stop")
		return nil
	}
	client := newUrgentClient(
		urgentCall{result: batchResult(allWork, "a", "b")},
		urgentCall{result: final("ok")},
	)
	runner := skipRunner(client, bench, 1, nil)
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		startInput(),
		WithSteering(inbox),
	)
	if err != nil {
		t.Fatal(err)
	}
	requireSkipped(t, out.Turn, "b", true)
	for _, reason := range []string{"", "tool unavailable", "not accepted: steering"} {
		forged := cloneTurn(out.Turn)
		for i, step := range forged.Steps {
			if step.Kind == StepToolSettled && step.Tool.Call.ID == "b" {
				forged.Steps[i].Tool.Reason = reason
			}
		}
		if _, err := Replay(forged); err == nil ||
			!strings.Contains(err.Error(), "only a steer") {
			t.Fatalf("reason %q: replay err = %v", reason, err)
		}
	}
}

// A started call may have taken effect, so neither the record nor a steer
// settles it as not run; a resumed start re-runs a call safe to repeat.
func TestSteerSkipsOnlyUnstartedCalls(t *testing.T) {
	r := &runtime{}
	for status, want := range map[ToolStatus]bool{
		ToolRequested:       true,
		ToolAllowed:         true,
		ToolStarted:         false,
		ToolWaiting:         false,
		ToolApprovalPending: false,
		ToolDenied:          false,
		ToolUncertain:       false,
	} {
		s := &replayState{
			callOrder:    []string{"a", "b"},
			callAttempts: map[string]string{"a": "m1", "b": "m1"},
		}
		s.Tools = map[string]ToolView{
			"a": {Status: ToolSettled, steered: true},
			"b": {Status: status},
		}
		if got := r.steerSkips(s, "b"); got != want {
			t.Errorf("%s: skips = %t, want %t", status, got, want)
		}
		// A call before the skip is not held back by the record.
		s.callOrder = []string{"b", "a"}
		if r.steerSkips(s, "b") {
			t.Errorf("%s: an earlier call was skipped", status)
		}
	}
}
