package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

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
		delivered[0].Seq != 2 {
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
	if n := len(warningsOf(out.Turn, WarningSteering)); n < 2 {
		t.Fatalf("got %d steering warnings", n)
	}

	failing := &tableSource{failSteers: errors.New("down")}
	client = &scriptedClient{results: []llm.Result{final("done")}}
	out, err = (Runner{Agent: testAgent(), Client: client}).Run(
		t.Context(), Turn{ID: "t2"}, startInput(), WithSteering(failing),
	)
	if err != nil || out.Stop.Kind != StopCompleted ||
		len(warningsOf(out.Turn, WarningSteering)) == 0 {
		t.Fatalf("stop=%+v warnings=%+v err=%v", out.Stop, out.Warnings, err)
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

func TestLeftoversReturnsRecordedButUnseenSteers(t *testing.T) {
	inbox := NewInbox()
	id := mustSend(t, inbox, "hold on")
	deny := SendHook{Name: "deny", Func: func(context.Context, SendPlan) (SendDecision, error) {
		return SendDecision{Deny: true, Reason: "closed"}, nil
	}}
	client := &scriptedClient{}
	out, err := (Runner{Agent: testAgent(), Client: client, Hooks: deny.Hooks()}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopRefused {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if len(stepsOf(out.Turn, StepDelivered)) != 1 {
		t.Fatal("the steer was not recorded")
	}
	left, err := inbox.Leftovers(out.Turn)
	if err != nil || len(left) != 1 || left[0].Item.ID != id {
		t.Fatalf("leftovers = %+v err=%v", left, err)
	}
	if again, _ := inbox.Leftovers(out.Turn); len(again) != 0 {
		t.Fatalf("leftovers returned twice: %+v", again)
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
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				if _, err := inbox.SendText("x"); err != nil {
					t.Error(err)
				}
			}
		})
	}
	out, err := (Runner{Agent: testAgent(), Client: client, Tools: tool.List{lookup}}).Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(inbox),
	)
	wg.Wait()
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	recorded := 0
	for _, step := range out.Turn.Steps {
		recorded += len(step.Items())
	}
	left, err := inbox.Leftovers(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if recorded+len(left) != 160 {
		t.Fatalf("recorded %d and left %d of 160", recorded, len(left))
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
