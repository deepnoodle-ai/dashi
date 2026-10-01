package dashi

import (
	"context"
	"encoding/json"
	"errors"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
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
	rec Recorder,
	extra ...tool.Tool,
) Runner {
	return Runner{
		Client:   client,
		Tools:    append(tool.List{bench.tool()}, extra...),
		Hooks:    bench.hooks(),
		Recorder: rec,
	}
}

// skipStart starts the turn with the given tool concurrency.
func skipStart(concurrency int) Start {
	agent := testAgent()
	agent.Limits.ToolConcurrency = concurrency
	return startAgent(agent)
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
	runner := skipRunner(client, bench, nil)
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		skipStart(1),
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
	if out.ModelCalls != 2 {
		t.Fatalf("attempts = %d", out.ModelCalls)
	}
	requireResultsThenSteer(
		t,
		client.inputs()[1],
		"stop, wrong file",
		[]string{"b", "c"},
		"a",
	)
	// The record replays to the same view.
	view, err := cloneTurn(out.Turn).View()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"b", "c"} {
		v := view.Tools[id]
		if v.Status != ToolFinished || modelText(v.Result()) != steerSkipText {
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
	runner := skipRunner(client, bench, nil)
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		skipStart(2),
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
	view, err := decoded.View()
	if err != nil {
		t.Fatal(err)
	}
	for id, v := range out.Tools {
		if !canonjson.SameValue(view.Tools[id].Result(), v.Result()) ||
			view.Tools[id].Status != v.Status {
			t.Fatalf("replay differs for %s", id)
		}
	}
	projected, err := TurnMessagesFull(decoded)
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
	runner := skipRunner(client, bench, nil)
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		skipStart(1),
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
				return NeedsApproval("needs a person"), nil
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
	runner := skipRunner(client, bench, nil, external)
	runner.Hooks = approvals.Hooks()
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		skipStart(1),
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
	for _, step := range stepsOf(out.Turn, StepMessagesAdded) {
		if step.Context.Steering {
			t.Fatal("the steer was recorded with no model call to read it")
		}
	}
	// The host answers both. The approved call runs: it came before the
	// skip, and the steer reaches the next model call after its result.
	for _, pending := range out.Pending {
		var settle Resolve
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
	if req.Step.Kind == StepToolFinished && req.Step.Tool.Call.ID == c.id &&
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
	runner := skipRunner(client, bench, rec)
	if _, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		skipStart(2),
		WithSteering(inbox),
	); err == nil {
		t.Fatal("the lost settlement was not reported")
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	view, err := saved.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Tools["b"].Status != ToolStarted ||
		view.Tools["c"].Status != ToolFinished ||
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
	runner := skipRunner(client, bench, nil)
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		skipStart(1),
		WithSteering(inbox),
	)
	if err != nil {
		t.Fatal(err)
	}
	requireSkipped(t, out.Turn, "b", true)
	for _, reason := range []string{"", "tool unavailable", "not accepted: steering"} {
		forged := cloneTurn(out.Turn)
		for i, step := range forged.Steps {
			if step.Kind == StepToolFinished && step.Tool.Call.ID == "b" {
				forged.Steps[i].Tool.Reason = reason
			}
		}
		if _, err := forged.View(); err == nil ||
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
		ToolResultUnknown:   false,
	} {
		s := &replayState{
			callOrder:    []string{"a", "b"},
			callAttempts: map[string]string{"a": "m1", "b": "m1"},
		}
		s.Tools = map[string]ToolView{
			"a": {Status: ToolFinished, steered: true},
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

// A submission is never skipped: the finish point decides a waiting steer.
// On the last model call no further call can deliver the steer, so the
// valid output stands and the steer waits with a warning.
func TestUrgentSteerNeverSkipsSubmission(t *testing.T) {
	inbox := newWatchedInbox()
	agent := testAgent()
	agent.OutputSchema = json.RawMessage(`{"type":"object"}`)
	agent.Limits.ModelCalls = 1
	agent.Limits.NoReservedFinalReply = true
	submit := func(string) string { return SubmitOutputTool }
	client := newUrgentClient(urgentCall{
		during: func(ctx context.Context) {
			inbox.sendUrgent(t, "also mention the date")
			<-ctx.Done()
		},
		result: batchResult(submit, "s1"),
	})
	out := runUrgentAgent(t, client, inbox, agent)
	if out.Stop.Kind != StopCompleted || out.Output == nil {
		t.Fatalf("stop=%+v output=%s", out.Stop, out.Output)
	}
	if fact := settledFact(t, out.Turn, "s1"); fact.Reason == reasonSteering {
		t.Fatalf("submission skipped: %+v", fact)
	}
	if !hasWarning(out, WarningSteering) {
		t.Fatalf("warnings: %+v", out.Warnings)
	}
}

// A skip the model never reads would only cancel its work: with no further
// model call possible, the batch runs and the steer waits with one warning.
func TestUrgentSteerSkipsNothingWithoutANextCall(t *testing.T) {
	inbox := newWatchedInbox()
	bench := newSkipBench()
	agent := testAgent()
	agent.Limits.ModelCalls = 1
	agent.Limits.NoReservedFinalReply = true
	client := newUrgentClient(urgentCall{
		during: func(ctx context.Context) {
			inbox.sendUrgent(t, "note")
			<-ctx.Done()
		},
		result: batchResult(allWork, "a", "b"),
	})
	out := runUrgentAgent(t, client, inbox, agent, func(r *Runner) {
		r.Tools = tool.List{bench.tool()}
	})
	requireRan(t, out.Turn, "a")
	requireRan(t, out.Turn, "b")
	n := 0
	for _, w := range out.Warnings {
		if w.Code == WarningSteering &&
			strings.Contains(w.Message, "did not skip") &&
			strings.Contains(w.Message, "stay with their source") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want one skip warning: %+v", out.Warnings)
	}
}

// On the last normal attempt, only the reserved final-answer call remains.
// It offers no tools, so the batch runs, that call receives the steer, and
// no warning claims the steer stays with its source.
func TestUrgentSteerOnLastAttemptReachesFinalAnswerCall(t *testing.T) {
	inbox := newWatchedInbox()
	bench := newSkipBench()
	agent := testAgent()
	agent.Limits.ModelCalls = 2
	client := newUrgentClient(
		urgentCall{
			during: func(ctx context.Context) {
				inbox.sendUrgent(t, "note")
				<-ctx.Done()
			},
			result: batchResult(allWork, "a", "b"),
		},
		urgentCall{result: final("x")},
	)
	out := runUrgentAgent(t, client, inbox, agent, func(r *Runner) {
		r.Tools = tool.List{bench.tool()}
	})
	// A reserved final answer ends the turn as limited by ModelCalls.
	if out.Stop.Kind != StopLimited {
		t.Fatalf("stop=%+v", out.Stop)
	}
	requireRan(t, out.Turn, "a")
	requireRan(t, out.Turn, "b")
	for _, w := range out.Warnings {
		if w.Code == WarningSteering {
			t.Fatalf("unexpected steering warning: %+v", w)
		}
	}
	ins := client.inputs()
	if len(ins) != 2 {
		t.Fatalf("want 2 model calls, got %d", len(ins))
	}
	if len(ins[1].Tools) != 0 {
		t.Fatalf("final-answer call offered %d tools", len(ins[1].Tools))
	}
	if hasText(ins[1].Messages, "note") < 0 {
		t.Fatal("steer missing from the final-answer call")
	}
	delivered := false
	for _, step := range out.Turn.Steps {
		if step.Kind == StepMessagesAdded && step.Context != nil &&
			step.Context.Steering {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("steer not recorded as delivered")
	}
}

// A call that fails its own checks reports that failure, not a skip.
func TestUrgentSteerLeavesUnknownToolResult(t *testing.T) {
	inbox := newWatchedInbox()
	bench := newSkipBench()
	release := bench.releaseOn("b")
	bench.run["a"] = func(context.Context) error {
		<-release
		inbox.sendUrgent(t, "stop")
		return nil
	}
	name := func(id string) string {
		if id == "x" {
			return "nope"
		}
		return "work"
	}
	client := newUrgentClient(
		urgentCall{result: batchResult(name, "a", "b", "x", "c")},
		urgentCall{result: final("ok")},
	)
	runner := skipRunner(client, bench, nil)
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		skipStart(1),
		WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	requireRan(t, out.Turn, "a")
	requireSkipped(t, out.Turn, "b", true)
	requireSkipped(t, out.Turn, "c", false)
	if fact := settledFact(t, out.Turn, "x"); fact.Reason != "unknown tool" {
		t.Fatalf("x settled as %+v", fact)
	}
	if _, err := cloneTurn(out.Turn).View(); err != nil {
		t.Fatal(err)
	}
}

// Handing a call to the host starts it, so a steer that arrives during an
// external call's BeforeTool skips it instead.
func TestUrgentSteerSkipsExternalCallBeforeHandOff(t *testing.T) {
	inbox := newWatchedInbox()
	bench := newSkipBench()
	bench.onPlan = func(id string) {
		if id == "e" {
			inbox.sendUrgent(t, "stop")
		}
	}
	external := tool.Tool{Definition: tool.Definition{
		Name:   "ext",
		Schema: json.RawMessage(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	client := newUrgentClient(
		urgentCall{result: batchResult(func(string) string { return "ext" }, "e")},
		urgentCall{result: final("ok")},
	)
	runner := skipRunner(client, bench, nil, external)
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		skipStart(1),
		WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	requireSkipped(t, out.Turn, "e", true)
	requireResultsThenSteer(t, client.inputs()[1], "stop", []string{"e"})
}

// steerOnModelSettled sends an urgent steer as the first model result is
// recorded: the call no longer runs, so the steer cannot cut it and waits
// for the batch.
type steerOnModelSettled struct {
	*MemoryRecorder
	t     *testing.T
	inbox *watchedInbox
	fired atomic.Bool
}

func (c *steerOnModelSettled) Commit(ctx context.Context, req CommitRequest) error {
	if err := c.MemoryRecorder.Commit(ctx, req); err != nil {
		return err
	}
	if req.Step.Kind == StepModelCallFinished && c.fired.CompareAndSwap(false, true) {
		c.inbox.sendUrgent(c.t, "also mention the date")
	}
	return nil
}

// With room for another call, the finish point turns the submission down
// for the steer, as it does a text answer; the skip never touches it.
func TestUrgentSteerLeavesSubmissionToTheFinishPoint(t *testing.T) {
	inbox := newWatchedInbox()
	agent := testAgent()
	agent.OutputSchema = json.RawMessage(`{"type":"object"}`)
	submit := func(string) string { return SubmitOutputTool }
	client := newUrgentClient(
		urgentCall{result: batchResult(submit, "s1")},
		urgentCall{result: batchResult(submit, "s2")},
	)
	rec := &steerOnModelSettled{
		MemoryRecorder: NewMemoryRecorder(),
		t:              t,
		inbox:          inbox,
	}
	out := runUrgentAgent(t, client, inbox, agent, func(r *Runner) {
		r.Recorder = rec
	})
	if out.Stop.Kind != StopCompleted || out.Output == nil {
		t.Fatalf("stop=%+v output=%s", out.Stop, out.Output)
	}
	fact := settledFact(t, out.Turn, "s1")
	if fact.Reason != "not accepted: "+sourceSteering {
		t.Fatalf("s1 settled as %+v", fact)
	}
}

// staleInbox is an Inbox with its own Ready signal. Once armed, its next
// Steers call reads the queue, then holds its answer until its context
// ends: a slow listener fetch that began before a steer was queued.
type staleInbox struct {
	*Inbox
	ready   chan struct{}
	armed   atomic.Bool
	entered chan struct{}
}

func newStaleInbox() *staleInbox {
	return &staleInbox{
		Inbox:   NewInbox(),
		ready:   make(chan struct{}, 1),
		entered: make(chan struct{}, 1),
	}
}

func (s *staleInbox) Ready() <-chan struct{} { return s.ready }

func (s *staleInbox) signal() {
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

func (s *staleInbox) Steers(ctx context.Context, turnID string) ([]Steer, error) {
	if !s.armed.CompareAndSwap(true, false) {
		return s.Inbox.Steers(ctx, turnID)
	}
	steers, err := s.Inbox.Steers(ctx, turnID)
	s.entered <- struct{}{}
	<-ctx.Done()
	return steers, err
}

// sendDuringFetch queues an urgent steer and signals Ready while the
// listener's fetch, begun before the steer, is in flight. It returns with
// the steer queued and signalled but not in the urgent set.
func (s *staleInbox) sendDuringFetch(t *testing.T, text string) {
	t.Helper()
	s.armed.Store(true)
	s.signal()
	<-s.entered
	if _, err := s.SendUrgent(text); err != nil {
		t.Error(err)
		return
	}
	s.signal()
}

// oneProc runs the test on one processor, so the listener cannot wake for a
// signal until the run goroutine blocks: a steer sent from the run
// goroutine stays unfetched until the run's next check looks.
func oneProc(t *testing.T) {
	prev := goruntime.GOMAXPROCS(1)
	t.Cleanup(func() { goruntime.GOMAXPROCS(prev) })
}

// A steer already waiting when a call reaches its start skips the call,
// even when the listener has not fetched it yet: the listener has not
// woken for the signal, or its fetch began before the steer was queued.
// The check before the start, or before the hand-off to the host, then
// looks at the source itself.
func TestUrgentSteerWaitingAtStartSkipsTheCall(t *testing.T) {
	external := tool.Tool{Definition: tool.Definition{
		Name:   "ext",
		Schema: json.RawMessage(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	oneProc(t)
	for _, tc := range []struct {
		name  string
		tool  string
		stale bool
	}{
		{"unfetched", "work", false},
		{"unfetched external", "ext", false},
		{"stale fetch", "work", true},
		{"stale fetch external", "ext", true},
	} {
		name := tc.tool
		t.Run(tc.name, func(t *testing.T) {
			inbox := newStaleInbox()
			bench := newSkipBench()
			// BeforeTool for b runs after the check at the top of the loop
			// and before the check at the start, on the run goroutine.
			bench.onPlan = func(id string) {
				if id != "b" {
					return
				}
				if tc.stale {
					inbox.sendDuringFetch(t, "stop")
					return
				}
				if _, err := inbox.SendUrgent("stop"); err != nil {
					t.Error(err)
				}
				inbox.signal()
			}
			named := func(id string) string {
				if id == "b" {
					return name
				}
				return "work"
			}
			client := newUrgentClient(
				urgentCall{result: batchResult(named, "a", "b")},
				urgentCall{result: final("ok")},
			)
			runner := skipRunner(client, bench, nil, external)
			out, err := runner.Run(
				t.Context(),
				Turn{ID: "t1"},
				skipStart(1),
				WithSteering(inbox),
			)
			if err != nil || out.Stop.Kind != StopCompleted {
				t.Fatalf("stop=%+v err=%v", out.Stop, err)
			}
			requireRan(t, out.Turn, "a")
			requireSkipped(t, out.Turn, "b", true)
			if bench.runs("b") != 0 {
				t.Fatal("b ran")
			}
			requireResultsThenSteer(
				t,
				client.inputs()[1],
				"stop",
				[]string{"b"},
				"a",
			)
		})
	}
}

// A steer queued after a call started does not interrupt it; it skips only
// the calls not yet started, even while the listener's fetch is in flight.
func TestUrgentSteerAfterStartDoesNotInterruptTheCall(t *testing.T) {
	oneProc(t)
	inbox := newStaleInbox()
	bench := newSkipBench()
	var cut atomic.Bool
	release := bench.releaseOn("b")
	bench.run["a"] = func(ctx context.Context) error {
		<-release
		inbox.sendDuringFetch(t, "stop")
		cut.Store(ctx.Err() != nil)
		return nil
	}
	client := newUrgentClient(
		urgentCall{result: batchResult(allWork, "a", "b")},
		urgentCall{result: final("ok")},
	)
	runner := skipRunner(client, bench, nil)
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		skipStart(1),
		WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if cut.Load() {
		t.Fatal("the steer cancelled the started call")
	}
	requireRan(t, out.Turn, "a")
	// b was allowed while a ran, and waited for a's slot.
	requireSkipped(t, out.Turn, "b", true)
	requireResultsThenSteer(t, client.inputs()[1], "stop", []string{"b"}, "a")
}

// A steer never skips the submission, so a record claiming it did is
// forged.
func TestReplayRejectsSteeringSkipOfSubmission(t *testing.T) {
	agent := testAgent()
	agent.OutputSchema = json.RawMessage(`{"type":"object"}`)
	submit := func(string) string { return SubmitOutputTool }
	client := newUrgentClient(urgentCall{result: batchResult(submit, "s1")})
	out := runUrgentAgent(t, client, NewInbox(), agent)
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	if _, err := cloneTurn(out.Turn).View(); err != nil {
		t.Fatal(err)
	}
	// Forge: the allowed submission settles as skipped by a steer instead
	// of starting.
	forged := cloneTurn(out.Turn)
	var steps []Step
	for _, step := range forged.Steps {
		if step.Tool != nil && step.Tool.Call.ID == "s1" {
			if step.Kind == StepToolStarted {
				continue
			}
			if step.Kind == StepToolFinished {
				skipped := tool.Outcome{
					State:  tool.StateNotExecuted,
					Output: new(tool.TextOutput(steerSkipText)),
				}
				step.Tool.Outcome = &skipped
				step.Tool.Reason = reasonSteering
				step.Tool.Result = nil
			}
		}
		step.Seq = int64(len(steps) + 1)
		steps = append(steps, step)
	}
	forged.Steps = steps
	if _, err := forged.View(); err == nil ||
		!strings.Contains(err.Error(), "never skips the reserved output tool") {
		t.Fatalf("replay err = %v", err)
	}
}

// cancellingInbox is a steering source whose armed fetch cancels the run
// and then blocks until the run's context ends, as a slow source does when
// the run is cancelled during the fetch.
type cancellingInbox struct {
	*staleInbox
	cancel context.CancelFunc
}

func (s *cancellingInbox) Steers(ctx context.Context, turnID string) ([]Steer, error) {
	if !s.armed.CompareAndSwap(true, false) {
		return s.Inbox.Steers(ctx, turnID)
	}
	s.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

// A run cancelled while the check before a call's start fetches from the
// source stops as a cancel before the check does: a non-repeatable local
// call is not recorded started, and an external call is not handed to the
// host.
func TestCancelDuringStartCheckStartsNothing(t *testing.T) {
	oneProc(t)
	external := tool.Tool{Definition: tool.Definition{
		Name:   "ext",
		Schema: json.RawMessage(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	var writes atomic.Int32
	write := tool.Tool{
		Definition: tool.Definition{
			Name:   "write",
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			writes.Add(1)
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("written")),
			}, nil
		}),
	}
	kinds := func(turn Turn) []StepKind {
		var out []StepKind
		for _, step := range turn.Steps {
			out = append(out, step.Kind)
		}
		return out
	}
	for _, name := range []string{"write", "ext"} {
		run := func(t *testing.T, duringFetch bool) RunResult {
			t.Helper()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			inbox := &cancellingInbox{staleInbox: newStaleInbox(), cancel: cancel}
			bench := newSkipBench()
			// BeforeTool runs after the check at the top of the loop and
			// before the check at the start. A pending signal makes that
			// check fetch from the source itself.
			bench.onPlan = func(string) {
				if !duringFetch {
					cancel()
					return
				}
				inbox.armed.Store(true)
				inbox.signal()
			}
			client := newUrgentClient(
				urgentCall{result: batchResult(
					func(string) string { return name },
					"b",
				)},
				urgentCall{result: final("ok")},
			)
			runner := skipRunner(client, bench, nil, external, write)
			out, err := runner.Run(
				ctx,
				Turn{ID: "t1"},
				skipStart(1),
				WithSteering(inbox),
			)
			if err != nil || out.Stop.Kind != StopInterrupted {
				t.Fatalf("stop=%+v err=%v", out.Stop, err)
			}
			for _, kind := range []StepKind{StepToolStarted, StepToolWaiting} {
				if steps := stepsOf(out.Turn, kind); len(steps) != 0 {
					t.Fatalf("%s recorded: %+v", kind, steps)
				}
			}
			if ids := pendingIDs(out.Pending); len(ids) != 0 {
				t.Fatalf("pending=%v", ids)
			}
			if writes.Load() != 0 {
				t.Fatal("write ran")
			}
			view, err := out.Turn.View()
			if err != nil {
				t.Fatal(err)
			}
			if status := view.Tools["b"].Status; status != ToolAllowed {
				t.Fatalf("b status = %s, want allowed", status)
			}
			return out
		}
		t.Run(name, func(t *testing.T) {
			before := run(t, false)
			during := run(t, true)
			if !slices.Equal(kinds(before.Turn), kinds(during.Turn)) {
				t.Fatalf(
					"steps %v, want %v",
					kinds(during.Turn),
					kinds(before.Turn),
				)
			}
		})
	}
}
