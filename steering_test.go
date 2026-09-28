package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// sendingClient runs onSend before each model call is answered, so a test
// can send a steer while the call is in flight.
type sendingClient struct {
	*scriptedClient
	onSend func(n int)
}

func (c sendingClient) Send(ctx context.Context, call llm.Call) (llm.Result, error) {
	if c.onSend != nil {
		c.onSend(c.sends + 1)
	}
	return c.scriptedClient.Send(ctx, call)
}

func startInput() Start {
	return Start{
		CommandID: "start",
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	}
}

func mustSend(t *testing.T, in *Inbox, text string) string {
	t.Helper()
	id, err := in.SendText(text)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// hasText reports whether any message carries text containing want.
func hasText(messages []llm.Message, want string) int {
	for i, m := range messages {
		if strings.Contains(m.Text(), want) {
			return i
		}
	}
	return -1
}

func stepsOf(turn Turn, kind StepKind) []Step {
	var out []Step
	for _, step := range turn.Steps {
		if step.Kind == kind {
			out = append(out, step)
		}
	}
	return out
}

func warningsOf(turn Turn, code WarningCode) []Warning {
	var out []Warning
	for _, step := range turn.Steps {
		for _, w := range step.Warnings {
			if w.Code == code {
				out = append(out, w)
			}
		}
	}
	return out
}

func TestSteerQueuedBeforeStartReachesFirstCall(t *testing.T) {
	inbox := NewInbox()
	id := mustSend(t, inbox, "use the staging database")
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := (Runner{Agent: testAgent(), Client: client}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if hasText(client.inputs[0].Messages, "staging") < 0 {
		t.Fatal("the first model call did not see the steer")
	}
	delivered := stepsOf(out.Turn, StepDelivered)
	if len(delivered) != 1 || delivered[0].Items()[0].ID != id ||
		delivered[0].Seq != 2 || !delivered[0].Context.Steering {
		t.Fatalf("delivered = %+v", delivered)
	}
	left, err := inbox.Leftovers(out.Turn)
	if err != nil || len(left) != 0 {
		t.Fatalf("leftovers = %+v err=%v", left, err)
	}
	if inbox.Cancel(id) {
		t.Fatal("a delivered steer was cancelled")
	}
}

func TestSteerDuringToolFollowsItsResult(t *testing.T) {
	inbox := NewInbox()
	client := &scriptedClient{results: []llm.Result{
		callResult("c1", "lookup"),
		final("done"),
	}}
	lookup := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			mustSend(t, inbox, "also check the README")
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("found")),
			}, nil
		}),
	}
	out, err := (Runner{Agent: testAgent(), Client: client, Tools: tool.List{lookup}}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	second := client.inputs[1].Messages
	steer := hasText(second, "README")
	result := -1
	for i, m := range second {
		for _, p := range m.Content {
			if p.Kind == llm.ContentToolResult {
				result = i
			}
		}
	}
	if steer < 0 || result < 0 || steer < result {
		t.Fatalf("steer at %d, tool result at %d: %+v", steer, result, second)
	}
}

func TestSteerSupersedesFinalText(t *testing.T) {
	inbox := NewInbox()
	var id string
	client := sendingClient{
		scriptedClient: &scriptedClient{results: []llm.Result{
			final("first answer"),
			final("second answer"),
		}},
		onSend: func(n int) {
			if n == 1 {
				id = mustSend(t, inbox, "no, use metric units")
			}
		},
	}
	var finishes int
	guard := FinishHook{Name: "count", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		finishes++
		return FinishDecision{}, nil
	}}
	out, err := (Runner{Agent: testAgent(), Client: client, Hooks: guard.Hooks()}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted || out.Answer != "second answer" {
		t.Fatalf("stop=%+v answer=%q err=%v", out.Stop, out.Answer, err)
	}
	if finishes != 1 {
		t.Fatalf("BeforeFinish ran %d times; the superseded answer skips it", finishes)
	}
	loops := stepsOf(out.Turn, StepLoopContinued)
	if len(loops) != 1 || len(loops[0].Items()) != 1 || loops[0].Items()[0].ID != id {
		t.Fatalf("loop continuations = %+v", loops)
	}
	if hasText(client.inputs[1].Messages, "metric") < 0 {
		t.Fatal("the second call did not see the steer")
	}
}

func TestSteerWaitsWhenNoCallRemains(t *testing.T) {
	inbox := NewInbox()
	client := sendingClient{
		scriptedClient: &scriptedClient{results: []llm.Result{
			callResult("c1", "lookup"),
			final("answer"),
		}},
		onSend: func(n int) {
			if n == 2 {
				mustSend(t, inbox, "too late")
			}
		},
	}
	lookup := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("ok"))}, nil
		}),
	}
	// Three attempts with the last reserved: after two calls, no normal
	// call remains, so the steer cannot supersede the answer.
	out, err := (Runner{Agent: testAgent(), Client: client, Tools: tool.List{lookup}}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted || out.Answer != "answer" {
		t.Fatalf("stop=%+v answer=%q err=%v", out.Stop, out.Answer, err)
	}
	warnings := warningsOf(out.Turn, WarningSteering)
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "2 of 3") {
		t.Fatalf("warnings = %+v", warnings)
	}
	left, err := inbox.Leftovers(out.Turn)
	if err != nil || len(left) != 1 || left[0].Item.Message.Text() != "too late" {
		t.Fatalf("leftovers = %+v err=%v", left, err)
	}
}

func TestSteerSupersedesSubmission(t *testing.T) {
	inbox := NewInbox()
	var id string
	client := sendingClient{
		scriptedClient: &scriptedClient{results: []llm.Result{
			submitResult("out-1", `{"answer":1}`),
			submitResult("out-2", `{"answer":2}`),
		}},
		onSend: func(n int) {
			if n == 1 {
				id = mustSend(t, inbox, "the answer is 2")
			}
		},
	}
	agent := structuredAgent()
	agent.Limits.ModelAttempts = 4
	out, err := (Runner{Agent: agent, Client: client}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted ||
		string(out.Output.Value) != `{"answer":2}` {
		t.Fatalf("stop=%+v output=%+v err=%v", out.Stop, out.Output, err)
	}
	rejected := settlementOf(t, out.Turn, "out-1")
	if rejected.Tool.Outcome.State != tool.StateFailed ||
		rejected.Tool.Reason != "not accepted: steering" ||
		len(rejected.Items()) != 1 || rejected.Items()[0].ID != id {
		t.Fatalf("rejected step = %+v", rejected)
	}
	want := "Not accepted: the user sent new input. Read it, then submit again."
	if seen := toolResultText(t, out.Turn, "out-1"); seen != want {
		t.Fatalf("model saw %q", seen)
	}
	if len(stepsOf(out.Turn, StepDelivered)) != 0 {
		t.Fatal("the steer was recorded twice")
	}
}

// tableSource is a durable-host style source: rows stay until Delivered
// removes them, and it can be told to fail.
type tableSource struct {
	mu          sync.Mutex
	rows        []Steer
	failSteers  error
	failAck     error
	acked       [][]string
	steersCalls int
}

func (s *tableSource) Steers(context.Context, string) ([]Steer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steersCalls++
	if s.failSteers != nil {
		return nil, s.failSteers
	}
	return slices.Clone(s.rows), nil
}

func (s *tableSource) Delivered(_ context.Context, _ string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acked = append(s.acked, append([]string(nil), ids...))
	if s.failAck != nil {
		return s.failAck
	}
	for _, id := range ids {
		for i, row := range s.rows {
			if row.Item.ID == id {
				s.rows = append(s.rows[:i], s.rows[i+1:]...)
				break
			}
		}
	}
	return nil
}

func textSteer(id, text string) Steer {
	return Steer{Item: ContextItem{ID: id, Message: llm.UserMessage(llm.Text(text))}}
}

func TestSteerRecordedOnceWhenAcknowledgmentFails(t *testing.T) {
	source := &tableSource{
		rows:    []Steer{textSteer("s1", "hello")},
		failAck: errors.New("table unavailable"),
	}
	client := &scriptedClient{results: []llm.Result{
		callResult("c1", "lookup"),
		final("done"),
	}}
	lookup := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("ok"))}, nil
		}),
	}
	out, err := (Runner{Agent: testAgent(), Client: client, Tools: tool.List{lookup}}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(source),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if n := len(stepsOf(out.Turn, StepDelivered)); n != 1 {
		t.Fatalf("steer recorded %d times", n)
	}
	// Recorded before call 1, found again before call 2 and at the answer.
	if len(source.acked) != 3 {
		t.Fatalf("acknowledgments = %v", source.acked)
	}
	if len(warningsOf(out.Turn, WarningSteering)) == 0 {
		t.Fatal("the failed acknowledgment left no warning")
	}
}

func TestInvalidSteersAndFailingSourceWarn(t *testing.T) {
	result := ContextItem{ID: "bad", Message: llm.Message{
		Role: llm.RoleUser,
		Content: []llm.Content{{
			Kind:       llm.ContentToolResult,
			ToolResult: &llm.ToolResult{CallID: "x", Output: tool.TextOutput("hi")},
		}},
	}}
	source := &tableSource{rows: []Steer{
		{Item: result},
		{Item: ContextItem{Message: llm.UserMessage(llm.Text("no id"))}},
	}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := (Runner{Agent: testAgent(), Client: client}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(source),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if len(stepsOf(out.Turn, StepDelivered)) != 0 {
		t.Fatal("an invalid steer was recorded")
	}
	// Steers runs before the call and at the answer; each is warned once.
	if n := len(warningsOf(out.Turn, WarningSteering)); n != 2 {
		t.Fatalf("got %d steering warnings", n)
	}

	failing := &tableSource{failSteers: errors.New("down")}
	client = &scriptedClient{results: []llm.Result{final("done")}}
	out, err = (Runner{Agent: testAgent(), Client: client}).Run(
		t.Context(), Turn{ID: "t2"}, startInput(), WithSteering(failing),
	)
	// The source is asked before the call and at the answer, and its
	// failure is reported once.
	if err != nil || out.Stop.Kind != StopCompleted ||
		len(warningsOf(out.Turn, WarningSteering)) != 1 {
		t.Fatalf("stop=%+v warnings=%+v err=%v", out.Stop, out.Warnings, err)
	}
	if failing.steersCalls != 2 {
		t.Fatalf("source asked %d times", failing.steersCalls)
	}
}

func TestSteerWithRecordedIDButOtherContentIsSkipped(t *testing.T) {
	source := &tableSource{rows: []Steer{textSteer("s1", "first")}}
	client := &scriptedClient{results: []llm.Result{final("one"), final("two")}}
	runner := Runner{Agent: testAgent(), Client: client}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(source))
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	// The same ID with other content is refused in t1.
	view, _ := Replay(out.Turn)
	if len(view.Warnings) != 0 {
		t.Fatalf("warnings = %+v", view.Warnings)
	}
	s, err := replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	rt := runtime{ctx: t.Context(), turn: out.Turn, opts: runOptions{
		steering: &tableSource{rows: []Steer{textSteer("s1", "changed")}},
	}}
	if got := rt.waitingSteers(s); len(got) != 0 {
		t.Fatalf("fresh = %+v", got)
	}
	if len(rt.pendingWarnings) != 1 || rt.pendingWarnings[0].Code != WarningSteering {
		t.Fatalf("warnings = %+v", rt.pendingWarnings)
	}
}

func TestDeliverSkipsRepeatedIDs(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("c1", "wait")}}
	external := tool.Tool{Definition: tool.Definition{
		Name:   "wait",
		Schema: json.RawMessage(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	runner := Runner{Agent: testAgent(), Client: client, Tools: tool.List{external}}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, startInput())
	if err != nil || out.Stop.Kind != StopWaiting {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	item := ContextItem{ID: "d1", Message: llm.UserMessage(llm.Text("note"))}
	out, err = runner.Run(t.Context(), out.Turn, Deliver{CommandID: "d1", Items: []ContextItem{item}})
	if err != nil {
		t.Fatal(err)
	}
	n := len(out.Turn.Steps)
	out, err = runner.Run(t.Context(), out.Turn, Deliver{CommandID: "d2", Items: []ContextItem{item}})
	if err != nil || len(out.Turn.Steps) != n {
		t.Fatalf("repeat recorded: steps=%d err=%v", len(out.Turn.Steps), err)
	}
	changed := item
	changed.Message = llm.UserMessage(llm.Text("other"))
	if _, err = runner.Run(t.Context(), out.Turn, Deliver{CommandID: "d3", Items: []ContextItem{changed}}); err == nil {
		t.Fatal("a repeated ID with other content was accepted")
	}
	call := ContextItem{Message: llm.Message{Role: llm.RoleUser, Content: []llm.Content{{
		Kind:       llm.ContentToolResult,
		ToolResult: &llm.ToolResult{CallID: "c1", Output: tool.TextOutput("forged")},
	}}}}
	if _, err = runner.Run(t.Context(), out.Turn, Deliver{CommandID: "d4", Items: []ContextItem{call}}); err == nil {
		t.Fatal("a tool result was accepted as context")
	}
}

func TestReplayRejectsRepeatedContextID(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("c1", "wait")}}
	external := tool.Tool{Definition: tool.Definition{
		Name:   "wait",
		Schema: json.RawMessage(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	runner := Runner{Agent: testAgent(), Client: client, Tools: tool.List{external}}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, startInput())
	if err != nil {
		t.Fatal(err)
	}
	item := ContextItem{ID: "d1", Message: llm.UserMessage(llm.Text("note"))}
	out, err = runner.Run(t.Context(), out.Turn, Deliver{CommandID: "d1", Items: []ContextItem{item}})
	if err != nil {
		t.Fatal(err)
	}
	forged := cloneTurn(out.Turn)
	step := cloneStep(forged.Steps[len(forged.Steps)-1])
	step.Seq++
	step.CommandID = "d2"
	forged.Steps = append(forged.Steps, step)
	if _, err := Replay(forged); !errors.Is(err, ErrInvalidTurn) ||
		!strings.Contains(err.Error(), "repeated context item ID") {
		t.Fatalf("err = %v", err)
	}
}

// pauseOnIntent closes pause when the runner records a model request, so
// the call is recorded but never dispatched.
type pauseOnIntent struct {
	*MemoryRecorder
	pause chan struct{}
}

func (p pauseOnIntent) Commit(ctx context.Context, req CommitRequest) error {
	if req.Step.Kind == StepModelIntent {
		close(p.pause)
	}
	return p.MemoryRecorder.Commit(ctx, req)
}

// TestRecordedSteerReachesNextTurnThroughHistory records a steer and then
// pauses before its model call is dispatched. The host gives the turn up:
// Leftovers returns nothing, because the steer is in the turn's record, and
// the steer reaches the next turn exactly once through Project. A
// NextCallOnly steer does too: a call that was never dispatched does not
// expire it, in Project or in the replayed view.
func TestRecordedSteerReachesNextTurnThroughHistory(t *testing.T) {
	for _, nextCallOnly := range []bool{false, true} {
		name := "plain"
		if nextCallOnly {
			name = "next call only"
		}
		t.Run(name, func(t *testing.T) {
			inbox := NewInbox()
			if _, err := inbox.Send(Steer{Item: ContextItem{
				Name:         "correction",
				Message:      llm.UserMessage(llm.Text("use the staging database")),
				NextCallOnly: nextCallOnly,
			}}); err != nil {
				t.Fatal(err)
			}
			pause := make(chan struct{})
			client := &scriptedClient{results: []llm.Result{final("done")}}
			runner := Runner{
				Agent:    testAgent(),
				Client:   client,
				Recorder: pauseOnIntent{NewMemoryRecorder(), pause},
			}
			out, err := runner.Run(
				t.Context(), Turn{ID: "t1"}, startInput(),
				WithSteering(inbox), WithPause(pause),
			)
			if err != nil || out.Stop.Kind != StopInterrupted || client.sends != 0 {
				t.Fatalf("stop=%+v sends=%d err=%v", out.Stop, client.sends, err)
			}
			if len(stepsOf(out.Turn, StepModelIntent)) != 1 {
				t.Fatal("the model request was not recorded")
			}
			if _, ok := out.Context(ContextUser, "correction"); !ok {
				t.Fatal("the replayed view expired a steer no call saw")
			}
			left, err := inbox.Leftovers(out.Turn)
			if err != nil || len(left) != 0 {
				t.Fatalf("leftovers = %+v err=%v", left, err)
			}
			history, err := Project(out.Turn)
			if err != nil {
				t.Fatal(err)
			}
			runner.Recorder = nil
			next, err := runner.Run(t.Context(), Turn{ID: "t2"}, Start{
				CommandID: "start",
				Input:     []llm.Message{llm.UserMessage(llm.Text("go on"))},
				History:   history,
			}, WithSteering(inbox))
			if err != nil || next.Stop.Kind != StopCompleted {
				t.Fatalf("stop=%+v err=%v", next.Stop, err)
			}
			count := 0
			for _, m := range client.inputs[0].Messages {
				if strings.Contains(m.Text(), "staging") {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("the steer appears %d times in the next request", count)
			}
		})
	}
}

// TestNextCallOnlyExpiresAfterDispatchedCall checks the other side: once a
// dispatched call follows a NextCallOnly item, later calls and the replayed
// view no longer carry it.
func TestNextCallOnlyExpiresAfterDispatchedCall(t *testing.T) {
	inbox := NewInbox()
	if _, err := inbox.Send(Steer{Item: ContextItem{
		Name:         "hint",
		Message:      llm.UserMessage(llm.Text("once only")),
		NextCallOnly: true,
	}}); err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{results: []llm.Result{callResult("c1", "lookup"), final("done")}}
	lookup := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("ok"))}, nil
		}),
	}
	out, err := (Runner{Agent: testAgent(), Client: client, Tools: tool.List{lookup}}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if hasText(client.inputs[0].Messages, "once only") < 0 ||
		hasText(client.inputs[1].Messages, "once only") >= 0 {
		t.Fatal("the one-call steer should reach only the first call")
	}
	if _, ok := out.Context(ContextUser, "hint"); ok {
		t.Fatal("the replayed view kept an expired item")
	}
}

// TestLeftoversKeepsSteerWithCollidingID queues a steer whose ID a Deliver
// item already uses with other content. It was never recorded as a steer,
// so Leftovers returns it rather than dropping it.
func TestLeftoversKeepsSteerWithCollidingID(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("c1", "wait")}}
	external := tool.Tool{Definition: tool.Definition{
		Name:   "wait",
		Schema: json.RawMessage(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	runner := Runner{Agent: testAgent(), Client: client, Tools: tool.List{external}}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, startInput())
	if err != nil {
		t.Fatal(err)
	}
	note := ContextItem{ID: "x", Message: llm.UserMessage(llm.Text("A"))}
	out, err = runner.Run(t.Context(), out.Turn, Deliver{CommandID: "d", Items: []ContextItem{note}})
	if err != nil {
		t.Fatal(err)
	}
	inbox := NewInbox()
	if _, err := inbox.Send(Steer{Item: ContextItem{
		ID:      "x",
		Message: llm.UserMessage(llm.Text("B")),
	}}); err != nil {
		t.Fatal(err)
	}
	left, err := inbox.Leftovers(out.Turn)
	if err != nil || len(left) != 1 || left[0].Item.Message.Text() != "B" {
		t.Fatalf("leftovers = %+v err=%v", left, err)
	}
}

func TestInboxClaimsAndRelease(t *testing.T) {
	inbox := NewInbox()
	a := mustSend(t, inbox, "a")
	b := mustSend(t, inbox, "b")
	if !inbox.Cancel(b) {
		t.Fatal("an unclaimed steer could not be cancelled")
	}
	got, _ := inbox.Steers(t.Context(), "t1")
	if len(got) != 1 || got[0].Item.ID != a || got[0].Item.Origin != ContextUser {
		t.Fatalf("steers = %+v", got)
	}
	if inbox.Cancel(a) {
		t.Fatal("a claimed steer was cancelled")
	}
	if other, _ := inbox.Steers(t.Context(), "t2"); len(other) != 0 {
		t.Fatalf("another turn received a claimed steer: %+v", other)
	}
	inbox.Release(t.Context(), "t1", []string{a}, nil)
	if !inbox.Cancel(a) {
		t.Fatal("a released steer could not be cancelled")
	}
	if _, err := inbox.SendText("  "); err == nil {
		t.Fatal("empty text accepted")
	}
	if _, err := inbox.Send(Steer{Item: ContextItem{ID: "x", Message: llm.UserMessage(llm.Text("x"))}}); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Send(Steer{Item: ContextItem{ID: "x", Message: llm.UserMessage(llm.Text("y"))}}); err == nil {
		t.Fatal("a repeated ID was queued")
	}
	if _, err := inbox.Leftovers(Turn{}); err == nil {
		t.Fatal("an invalid turn was accepted")
	}
}

func TestRunReleasesUnrecordedClaims(t *testing.T) {
	inbox := NewInbox()
	client := sendingClient{
		scriptedClient: &scriptedClient{results: []llm.Result{final("done")}},
		onSend:         func(int) { mustSend(t, inbox, "late") },
	}
	// The steer arrives during the only call allowed, so it waits.
	agent := testAgent()
	agent.Limits.ModelAttempts = 1
	out, err := (Runner{Agent: agent, Client: client}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	left, _ := inbox.Steers(t.Context(), "t2")
	if len(left) != 1 {
		t.Fatalf("the waiting steer was not released: %+v", left)
	}
}

func TestInboxConcurrentSends(t *testing.T) {
	inbox := NewInbox()
	var mu sync.Mutex
	var sent []string
	send := func() {
		id, err := inbox.SendText("x")
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		sent = append(sent, id)
		mu.Unlock()
	}
	// Sends also come from inside each model call and each tool call, so
	// they interleave with the run's Steers calls.
	client := sendingClient{
		scriptedClient: &scriptedClient{results: []llm.Result{
			callResult("c1", "lookup"),
			callResult("c2", "lookup"),
			final("done"),
		}},
		onSend: func(int) { send() },
	}
	lookup := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			send()
			return tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("ok"))}, nil
		}),
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					send()
					time.Sleep(50 * time.Microsecond)
				}
			}
		})
	}
	// Four attempts with the last reserved: the third call's answer cannot
	// be superseded, so the script's three results suffice.
	agent := testAgent()
	agent.Limits.ModelAttempts = 4
	out, err := (Runner{Agent: agent, Client: client, Tools: tool.List{lookup}}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(inbox),
	)
	close(stop)
	wg.Wait()
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	seen := map[string]int{}
	deliveries := 0
	for _, step := range out.Turn.Steps {
		if step.Context == nil || !step.Context.Steering {
			continue
		}
		deliveries++
		for _, item := range step.Items() {
			seen[item.ID]++
		}
	}
	left, err := inbox.Leftovers(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	for _, steer := range left {
		seen[steer.Item.ID]++
	}
	if deliveries < 2 {
		t.Fatalf("steers reached the turn in %d steps; want interleaving", deliveries)
	}
	if len(seen) != len(sent) {
		t.Fatalf("%d IDs recorded or left over; %d sent", len(seen), len(sent))
	}
	for _, id := range sent {
		if seen[id] != 1 {
			t.Fatalf("steer %s appears %d times", id, seen[id])
		}
	}
}

// TestDeliverBeforeSubmissionStartReachesModel stops a run after the model
// submitted output but before the submission call started. A Deliver made
// then supersedes the kept submission on Continue.
func TestDeliverBeforeSubmissionStartReachesModel(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{
		submitResult("out-1", `{"answer":1}`),
		submitResult("out-2", `{"answer":2}`),
	}}
	rec := &failOnceKind{base: NewMemoryRecorder(), kind: StepToolStarted}
	agent := structuredAgent()
	agent.Limits.ModelAttempts = 4
	runner := Runner{Agent: agent, Client: client, Recorder: rec}
	if _, err := runner.Run(t.Context(), Turn{ID: "t1"}, startInput()); err == nil {
		t.Fatal("the fault was not reported")
	}
	saved, err := rec.base.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	runner.Recorder = rec.base
	note := ContextItem{Message: llm.UserMessage(llm.Text("one more thing"))}
	out, err := runner.Run(t.Context(), saved, Deliver{CommandID: "d", Items: []ContextItem{note}})
	if err != nil {
		t.Fatal(err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "c"})
	if err != nil || out.Stop.Kind != StopCompleted ||
		string(out.Output.Value) != `{"answer":2}` || client.sends != 2 {
		t.Fatalf("stop=%+v output=%+v sends=%d err=%v", out.Stop, out.Output, client.sends, err)
	}
	if hasText(client.inputs[1].Messages, "one more thing") < 0 {
		t.Fatal("the delivered item did not reach the model")
	}
}

// TestNudgeOfRecordedItemsCostsNoCall has a BeforeFinish hook deliver the
// same identified item at every answer. The first delivery nudges the model
// once; afterwards the item is recorded, so it is no nudge.
func TestNudgeOfRecordedItemsCostsNoCall(t *testing.T) {
	note := ContextItem{ID: "n1", Message: llm.UserMessage(llm.Text("cite a source"))}
	nudge := FinishHook{Name: "nudge", Func: func(context.Context, FinishPlan) (FinishDecision, error) {
		return FinishDecision{Deliver: []ContextItem{note}}, nil
	}}
	client := &scriptedClient{results: []llm.Result{final("one"), final("two")}}
	agent := testAgent()
	agent.Limits.ModelAttempts = 5
	out, err := (Runner{Agent: agent, Client: client, Hooks: nudge.Hooks()}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(),
	)
	if err != nil || out.Stop.Kind != StopCompleted || out.Answer != "two" ||
		client.sends != 2 {
		t.Fatalf("stop=%+v answer=%q sends=%d err=%v", out.Stop, out.Answer, client.sends, err)
	}
}

// TestLeftoversIgnoresOtherIdentifiedItems checks that Leftovers returns
// only steers, not Deliver items that also carry IDs.
func TestLeftoversIgnoresOtherIdentifiedItems(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("c1", "wait")}}
	external := tool.Tool{Definition: tool.Definition{
		Name:   "wait",
		Schema: json.RawMessage(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	runner := Runner{Agent: testAgent(), Client: client, Tools: tool.List{external}}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, startInput())
	if err != nil {
		t.Fatal(err)
	}
	item := ContextItem{ID: "d1", Message: llm.UserMessage(llm.Text("note"))}
	out, err = runner.Run(t.Context(), out.Turn, Deliver{CommandID: "d1", Items: []ContextItem{item}})
	if err != nil {
		t.Fatal(err)
	}
	if left, err := NewInbox().Leftovers(out.Turn); err != nil || len(left) != 0 {
		t.Fatalf("leftovers = %+v err=%v", left, err)
	}
}
func TestHookCannotDeliverToolResult(t *testing.T) {
	bad := ModelHook{Name: "bad", Func: func(context.Context, ModelPlan) (ModelDecision, error) {
		return ModelDecision{Deliver: []ContextItem{{Message: llm.Message{
			Role: llm.RoleUser,
			Content: []llm.Content{{
				Kind:       llm.ContentToolResult,
				ToolResult: &llm.ToolResult{CallID: "x", Output: tool.TextOutput("forged")},
			}},
		}}}}, nil
	}}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := (Runner{Agent: testAgent(), Client: client, Hooks: bad.Hooks()}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(),
	)
	if err != nil || out.Stop.Kind != StopCompleted ||
		len(stepsOf(out.Turn, StepDelivered)) != 0 ||
		len(warningsOf(out.Turn, WarningHookFailed)) != 1 {
		t.Fatalf("stop=%+v warnings=%+v err=%v", out.Stop, out.Warnings, err)
	}
}

// TestDeliverAfterInterruptedCandidateReachesModel covers a Deliver sent
// after a run was interrupted at a final text answer: Continue asks the
// model again with it, instead of accepting the kept answer.
func TestDeliverAfterInterruptedCandidateReachesModel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts int
		answer   string
		dropped  bool
	}{
		{name: "call left", attempts: 3, answer: "second"},
		{name: "no call left", attempts: 2, answer: "first", dropped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			interrupt := FinishHook{Name: "interrupt", Func: func(
				hctx context.Context,
				p FinishPlan,
			) (FinishDecision, error) {
				if p.Answer == "first" && ctx.Err() == nil {
					cancel()
					return FinishDecision{}, hctx.Err()
				}
				return FinishDecision{}, nil
			}}
			client := &scriptedClient{results: []llm.Result{final("first"), final("second")}}
			agent := testAgent()
			agent.Limits.ModelAttempts = tc.attempts
			runner := Runner{Agent: agent, Client: client, Hooks: interrupt.Hooks()}
			out, err := runner.Run(ctx, Turn{ID: "t1"}, startInput())
			if err != nil || out.Stop.Kind != StopInterrupted {
				t.Fatalf("stop=%+v err=%v", out.Stop, err)
			}
			note := ContextItem{Message: llm.UserMessage(llm.Text("one more thing"))}
			out, err = runner.Run(t.Context(), out.Turn, Deliver{CommandID: "d", Items: []ContextItem{note}})
			if err != nil {
				t.Fatal(err)
			}
			out, err = runner.Run(t.Context(), out.Turn, Continue{CommandID: "c"})
			if err != nil || out.Stop.Kind != StopCompleted || out.Answer != tc.answer {
				t.Fatalf("stop=%+v answer=%q err=%v", out.Stop, out.Answer, err)
			}
			dropped := len(warningsOf(out.Turn, WarningDeliveryDropped)) > 0
			if dropped != tc.dropped {
				t.Fatalf("dropped warning = %v", dropped)
			}
			if !tc.dropped && hasText(client.inputs[1].Messages, "one more thing") < 0 {
				t.Fatal("the delivered item did not reach the model")
			}
		})
	}
}

// steerFaultRecorder fails the first commit whose step carries items, in
// the way mode names: rejected, lost before the write, or lost after it.
type steerFaultRecorder struct {
	*MemoryRecorder
	mode  string
	fired bool
}

func (f *steerFaultRecorder) Commit(ctx context.Context, req CommitRequest) error {
	if f.fired || len(req.Step.Items()) == 0 {
		return f.MemoryRecorder.Commit(ctx, req)
	}
	f.fired = true
	switch f.mode {
	case "reject":
		return &CommitError{Persistence: PersistenceRejected, Err: errors.New("injected")}
	case "lose_write":
		return &CommitError{Persistence: PersistenceUnknown, Err: errors.New("injected")}
	}
	if err := f.MemoryRecorder.Commit(ctx, req); err != nil {
		return err
	}
	return errors.New("injected: acknowledgement lost")
}

// TestSupersededSubmissionSurvivesCommitFaults fails the step that rejects
// a submission for a steer. After a reload and Continue with the same
// inbox, the steer is recorded once and the turn completes with the
// corrected output.
func TestSupersededSubmissionSurvivesCommitFaults(t *testing.T) {
	for _, mode := range []string{"reject", "lose_write", "lose_ack"} {
		t.Run(mode, func(t *testing.T) {
			inbox := NewInbox()
			client := sendingClient{
				scriptedClient: &scriptedClient{results: []llm.Result{
					submitResult("out-1", `{"answer":1}`),
					submitResult("out-2", `{"answer":2}`),
				}},
				onSend: func(n int) {
					if n == 1 {
						mustSend(t, inbox, "the answer is 2")
					}
				},
			}
			rec := &steerFaultRecorder{MemoryRecorder: NewMemoryRecorder(), mode: mode}
			agent := structuredAgent()
			agent.Limits.ModelAttempts = 4
			runner := Runner{Agent: agent, Client: client, Recorder: rec}
			if _, err := runner.Run(
				t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(inbox),
			); err == nil {
				t.Fatal("the fault was not reported")
			}
			saved, err := rec.Load(t.Context(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			out, err := runner.Run(
				t.Context(), saved, Continue{CommandID: "resume"}, WithSteering(inbox),
			)
			if err != nil || out.Stop.Kind != StopCompleted ||
				string(out.Output.Value) != `{"answer":2}` {
				t.Fatalf("stop=%+v output=%+v err=%v", out.Stop, out.Output, err)
			}
			count := 0
			for _, step := range out.Turn.Steps {
				count += len(step.Items())
			}
			if count != 1 || client.sends != 2 {
				t.Fatalf("steer recorded %d times, %d model calls", count, client.sends)
			}
			if left, _ := inbox.Leftovers(out.Turn); len(left) != 0 {
				t.Fatalf("leftovers = %+v", left)
			}
		})
	}
}

// TestReplayRejectsForgedSteeringSteps checks that only the runner records
// steers: never a command, and on a settlement only a rejected submission.
func TestReplayRejectsForgedSteeringSteps(t *testing.T) {
	inbox := NewInbox()
	mustSend(t, inbox, "hello")
	client := &scriptedClient{results: []llm.Result{
		callResult("c1", "lookup"),
		final("done"),
	}}
	lookup := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{State: tool.StateSucceeded, Output: new(tool.TextOutput("ok"))}, nil
		}),
	}
	out, err := (Runner{Agent: testAgent(), Client: client, Tools: tool.List{lookup}}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	forge := func(t *testing.T, want string, change func(*Step) bool) {
		t.Helper()
		turn := cloneTurn(out.Turn)
		for i := range turn.Steps {
			if change(&turn.Steps[i]) {
				if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) ||
					!strings.Contains(err.Error(), want) {
					t.Fatalf("forged step replayed: %v", err)
				}
				return
			}
		}
		t.Fatal("no step to forge")
	}
	t.Run("command", func(t *testing.T) {
		forge(t, "a command cannot record steers", func(s *Step) bool {
			if s.Kind != StepDelivered {
				return false
			}
			s.CommandID, s.CommandDigest = "deliver", "v2:sha256:00"
			return true
		})
	})
	t.Run("tool settlement", func(t *testing.T) {
		forge(t, "only a rejected submission settles with steers", func(s *Step) bool {
			if s.Kind != StepToolSettled {
				return false
			}
			s.Context = &ContextFact{
				Items: []ContextItem{{
					ID:      "s9",
					Message: llm.UserMessage(llm.Text("forged")),
				}},
				Steering: true,
			}
			return true
		})
	})
}

// loseIntentAck records the first model request and then reports its write
// as unknown, so the worker stops before the client is called, as a crash
// between the request and dispatch would.
type loseIntentAck struct {
	*MemoryRecorder
	fired bool
}

func (l *loseIntentAck) Commit(ctx context.Context, req CommitRequest) error {
	if err := l.MemoryRecorder.Commit(ctx, req); err != nil {
		return err
	}
	if !l.fired && req.Step.Kind == StepModelIntent {
		l.fired = true
		return errors.New("injected: acknowledgement lost")
	}
	return nil
}

// TestNextCallOnlySteerSurvivesCrashBeforeDispatch records a NextCallOnly
// steer and a model request, stops before the client is called, reloads,
// and continues. The recovered settlement is no evidence of dispatch, so
// the steer reaches the resent call.
func TestNextCallOnlySteerSurvivesCrashBeforeDispatch(t *testing.T) {
	inbox := NewInbox()
	if _, err := inbox.Send(Steer{Item: ContextItem{
		Message:      llm.UserMessage(llm.Text("use the staging database")),
		NextCallOnly: true,
	}}); err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := &loseIntentAck{MemoryRecorder: NewMemoryRecorder()}
	runner := Runner{Agent: testAgent(), Client: client, Recorder: rec}
	if _, err := runner.Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(inbox),
	); err == nil {
		t.Fatal("the lost acknowledgement was not reported")
	}
	if client.sends != 0 {
		t.Fatal("the client was called before the request was acknowledged")
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runner.Run(
		t.Context(), saved, Continue{CommandID: "resume"}, WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("stop=%+v sends=%d err=%v", out.Stop, client.sends, err)
	}
	if hasText(client.inputs[0].Messages, "staging") < 0 {
		t.Fatal("the steer did not reach the resent call")
	}
}
