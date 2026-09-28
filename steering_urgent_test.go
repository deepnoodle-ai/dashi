package dashi

import (
	"context"
	"encoding/json"
	"errors"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// urgentCall scripts one model call for urgentClient. The client streams
// deltas, then runs during; when block is set it then waits for the call's
// context to end and streams late, the frames a provider sends after the
// cancel. It returns result, with the context's error when it blocked.
type urgentCall struct {
	deltas []llm.Delta
	during func(ctx context.Context)
	block  bool
	late   []llm.Delta
	result llm.Result
}

// urgentClient is a deterministic model client for urgent steers. Prepare
// and the client's identity come from base, a scripted or priced client, so
// base.inputs holds every prepared request.
type urgentClient struct {
	llm.Client
	base  *scriptedClient
	calls []urgentCall
	mu    sync.Mutex
	sends int
}

func newUrgentClient(calls ...urgentCall) *urgentClient {
	base := &scriptedClient{}
	return &urgentClient{Client: base, base: base, calls: calls}
}

func newPricedUrgentClient(calls ...urgentCall) *urgentClient {
	priced := newPricedClient()
	return &urgentClient{
		Client: priced,
		base:   &priced.scriptedClient,
		calls:  calls,
	}
}

func (c *urgentClient) Send(
	ctx context.Context,
	call llm.Call,
) (llm.Result, error) {
	c.mu.Lock()
	if c.sends >= len(c.calls) {
		c.mu.Unlock()
		return llm.Result{}, errors.New("unexpected model call")
	}
	script := c.calls[c.sends]
	c.sends++
	c.mu.Unlock()
	emit := func(deltas []llm.Delta) {
		for _, delta := range deltas {
			if call.Sink != nil {
				call.Sink(delta)
			}
		}
	}
	emit(script.deltas)
	if script.during != nil {
		script.during(ctx)
	}
	if !script.block {
		return script.result, nil
	}
	<-ctx.Done()
	emit(script.late)
	return script.result, ctx.Err()
}

func (c *urgentClient) inputs() []llm.Input {
	return c.base.inputs
}

// lastMessages returns the messages of the last prepared request.
func (c *urgentClient) lastMessages() []llm.Message {
	inputs := c.base.inputs
	return inputs[len(inputs)-1].Messages
}

// watchedInbox reports each Steers call on fetched. A test that sends an
// urgent steer while the run goroutine waits on it can then wait until the
// listener has taken the steer: the listener holds the source lock from
// that call until it has decided, and the run takes that lock to look.
type watchedInbox struct {
	*Inbox
	fetched chan struct{}
}

func newWatchedInbox() *watchedInbox {
	return &watchedInbox{Inbox: NewInbox(), fetched: make(chan struct{}, 64)}
}

func (w *watchedInbox) Steers(
	ctx context.Context,
	turnID string,
) ([]Steer, error) {
	steers, err := w.Inbox.Steers(ctx, turnID)
	w.fetched <- struct{}{}
	return steers, err
}

// sendUrgent sends an urgent steer and waits until the listener fetched it.
// Call it only while the run goroutine is blocked, so no other Steers call
// can be the one observed.
func (w *watchedInbox) sendUrgent(t *testing.T, text string) {
	t.Helper()
	for len(w.fetched) > 0 {
		<-w.fetched
	}
	if _, err := w.SendUrgent(text); err != nil {
		t.Error(err)
		return
	}
	select {
	case <-w.fetched:
	case <-time.After(5 * time.Second):
		t.Error("the listener never fetched the urgent steer")
	}
}

func textDelta(text string) llm.Delta {
	return llm.Delta{Kind: llm.DeltaText, Text: text}
}

// cutCall streams text, sends an urgent steer, and blocks until cut.
func cutCall(t *testing.T, inbox *watchedInbox, text, steer string) urgentCall {
	return urgentCall{
		deltas: []llm.Delta{textDelta(text)},
		during: func(context.Context) { inbox.sendUrgent(t, steer) },
		block:  true,
	}
}

func runUrgent(
	t *testing.T,
	client llm.Client,
	source Steering,
	opts ...func(*Runner),
) Outcome {
	t.Helper()
	runner := Runner{Agent: testAgent(), Client: client}
	for _, opt := range opts {
		opt(&runner)
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		startInput(),
		WithSteering(source),
	)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// lookupTool is a local tool that always succeeds.
func lookupTool() tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(
			context.Context,
			tool.Call,
		) (tool.Outcome, error) {
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("found")),
			}, nil
		}),
	}
}

func settlements(turn Turn) []*ModelFact {
	var out []*ModelFact
	for _, step := range stepsOf(turn, StepModelSettled) {
		out = append(out, step.Model)
	}
	return out
}

func TestUrgentSteerCutsStreamedCall(t *testing.T) {
	inbox := newWatchedInbox()
	client := newUrgentClient(
		urgentCall{
			deltas: []llm.Delta{textDelta("Editing main.go")},
			during: func(context.Context) {
				inbox.sendUrgent(t, "stop, wrong file")
			},
			block: true,
			late:  []llm.Delta{textDelta(" and more text never shown")},
		},
		urgentCall{result: final("ok, editing util.go")},
	)
	out := runUrgent(t, client, inbox)
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	cut := settlements(out.Turn)[0]
	if cut.Interrupted != InterruptSteering || cut.NoDispatch {
		t.Fatalf("the cut call was settled as %+v", cut)
	}
	if got := cut.Result.Message.Text(); got != "Editing main.go" {
		t.Fatalf("partial = %q; text after the cut must not be kept", got)
	}
	if out.ModelAttempts != 1 || out.Interruptions != 0 {
		t.Fatalf(
			"attempts=%d interruptions=%d; a steering cut counts neither",
			out.ModelAttempts,
			out.Interruptions,
		)
	}
	next := client.inputs()[1].Messages
	partial := hasText(next, "Editing main.go")
	steer := hasText(next, "stop, wrong file")
	if partial < 0 || steer != partial+2 {
		t.Fatalf(
			"partial at %d, steer at %d: want partial, reminder, steer",
			partial,
			steer,
		)
	}
	if next[partial].Role != llm.RoleAssistant {
		t.Fatal("the partial is not an assistant message")
	}
	reminder := messageReminder(next[partial+1])
	if reminder == nil || reminder.Content != steeringInterruptedText {
		t.Fatalf(
			"no interruption reminder follows the partial: %+v",
			next[partial+1],
		)
	}
	if strings.Contains(next[partial].Text(), "never shown") {
		t.Fatal("text streamed after the cut reached the model")
	}
	if out.Answer != "ok, editing util.go" {
		t.Fatalf("answer = %q; a cut call is never an answer", out.Answer)
	}
}

func TestUrgentSteerLateFramesCountTowardOutput(t *testing.T) {
	inbox := newWatchedInbox()
	client := newUrgentClient(
		urgentCall{
			deltas: []llm.Delta{
				textDelta("abcd"),
				{Kind: llm.DeltaReasoning, Text: strings.Repeat("r", 40)},
			},
			during: func(context.Context) { inbox.sendUrgent(t, "stop") },
			block:  true,
			late:   []llm.Delta{textDelta(strings.Repeat("x", 40))},
		},
		urgentCall{result: final("ok")},
	)
	out := runUrgent(t, client, inbox)
	cut := settlements(out.Turn)[0]
	// 4 text + 40 reasoning + 40 late text runes, four to a token.
	if cut.Result.Usage == nil || cut.Result.Usage.OutputTokens != 21 {
		t.Fatalf("recorded output estimate: %+v", cut.Result.Usage)
	}
	if !cut.EstimatedUsage {
		t.Fatal("the cut call's usage is not marked estimated")
	}
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Spend.Usage.OutputTokens < 21 {
		t.Fatalf(
			"replayed output = %d; the estimate was lost",
			view.Spend.Usage.OutputTokens,
		)
	}
}

func TestUrgentSteerCutsCollectedCall(t *testing.T) {
	inbox := newWatchedInbox()
	client := newUrgentClient(
		urgentCall{
			during: func(context.Context) { inbox.sendUrgent(t, "stop") },
			block:  true,
		},
		urgentCall{result: final("ok")},
	)
	out := runUrgent(t, client, inbox)
	cut := settlements(out.Turn)[0]
	if cut.Interrupted != InterruptSteering || cut.NoDispatch {
		t.Fatalf("a cut collected call must count as sent: %+v", cut)
	}
	if !cut.EstimatedUsage || out.Spend.Estimated != 2 {
		t.Fatalf(
			"estimated calls = %d; the cut call was not counted",
			out.Spend.Estimated,
		)
	}
	if out.Spend.Usage.InputTokens <= 0 {
		t.Fatal("the cut call's input was not counted")
	}
	if hasText(client.inputs()[1].Messages, "stop") < 0 {
		t.Fatal("the steer did not reach the next call")
	}
}

func TestUrgentSteerBeforeHandOffSettlesNotDispatched(t *testing.T) {
	inbox := newWatchedInbox()
	client := newUrgentClient(urgentCall{result: final("ok")})
	rec := &intentHook{MemoryRecorder: NewMemoryRecorder()}
	rec.onIntent = func() { inbox.sendUrgent(t, "use staging") }
	out := runUrgent(t, client, inbox, func(r *Runner) { r.Recorder = rec })
	if out.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("stop=%+v sends=%d", out.Stop, client.sends)
	}
	cut := settlements(out.Turn)[0]
	if cut.Interrupted != InterruptSteering || !cut.NoDispatch ||
		cut.Result != nil || cut.Charge != nil {
		t.Fatalf("a call cut before hand-off: %+v", cut)
	}
	if hasText(client.lastMessages(), "use staging") < 0 {
		t.Fatal("the steer did not reach the call that was sent")
	}
}

// intentHook runs onIntent once, while the first model request commits.
type intentHook struct {
	*MemoryRecorder
	onIntent func()
	fired    bool
}

func (h *intentHook) Commit(ctx context.Context, req CommitRequest) error {
	if err := h.MemoryRecorder.Commit(ctx, req); err != nil {
		return err
	}
	if !h.fired && req.Step.Kind == StepModelIntent {
		h.fired = true
		h.onIntent()
	}
	return nil
}

func TestUrgentSteerBeforeSendGoesBackForTheSteer(t *testing.T) {
	inbox := newWatchedInbox()
	client := newUrgentClient(urgentCall{result: final("ok")})
	asked := 0
	hook := ModelHook{
		Name: "late",
		Func: func(context.Context, ModelPlan) (ModelDecision, error) {
			asked++
			if asked == 1 {
				inbox.sendUrgent(t, "use staging")
			}
			return ModelDecision{}, nil
		},
	}
	out := runUrgent(t, client, inbox, func(r *Runner) {
		r.Hooks = hook.Hooks()
	})
	if out.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("stop=%+v sends=%d", out.Stop, client.sends)
	}
	if n := len(stepsOf(out.Turn, StepModelIntent)); n != 1 {
		t.Fatalf(
			"%d model requests recorded; the first pass must record none",
			n,
		)
	}
	if asked != 2 {
		t.Fatalf(
			"BeforeModel ran %d times; the pass after the steer asks again",
			asked,
		)
	}
	if hasText(client.lastMessages(), "use staging") < 0 {
		t.Fatal("the steer did not reach the call")
	}
}

func TestCallCompletingAsSteerArrivesStands(t *testing.T) {
	inbox := newWatchedInbox()
	client := newUrgentClient(
		urgentCall{
			during: func(ctx context.Context) {
				inbox.sendUrgent(t, "also check the tests")
				<-ctx.Done()
			},
			result: final("done"),
		},
		urgentCall{result: final("tests checked")},
	)
	out := runUrgent(t, client, inbox)
	for _, fact := range settlements(out.Turn) {
		if fact.Interrupted != InterruptNone {
			t.Fatalf("a complete result was settled as interrupted: %+v", fact)
		}
	}
	if out.ModelAttempts != 2 || out.Answer != "tests checked" {
		t.Fatalf("attempts=%d answer=%q", out.ModelAttempts, out.Answer)
	}
	if hasText(client.inputs()[1].Messages, "also check the tests") < 0 {
		t.Fatal("the steer did not reach the next call")
	}
}

func TestRunEndingWithSteerSettlesAsRun(t *testing.T) {
	inbox := newWatchedInbox()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := newUrgentClient(urgentCall{
		deltas: []llm.Delta{textDelta("partial")},
		during: func(callCtx context.Context) {
			inbox.sendUrgent(t, "stop")
			<-callCtx.Done()
			cancel()
		},
		block: true,
	})
	rec := NewMemoryRecorder()
	out, err := Runner{Agent: testAgent(), Client: client, Recorder: rec}.Run(
		ctx,
		Turn{ID: "t1"},
		startInput(),
		WithSteering(inbox),
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopInterrupted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	if got := settlements(out.Turn)[0].Interrupted; got != InterruptRun {
		t.Fatalf("cause = %q; the run's context ending wins", got)
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	left, err := inbox.Leftovers(saved)
	if err != nil || len(left) != 1 || !left[0].Urgent {
		t.Fatalf(
			"leftovers=%+v err=%v; the steer must stay with its source",
			left,
			err,
		)
	}
}

func TestReservedFinalAnswerCallIsCutAndAskedAgain(t *testing.T) {
	inbox := newWatchedInbox()
	agent := testAgent()
	agent.Limits.ModelAttempts = 2
	client := newUrgentClient(
		urgentCall{result: callResult("c1", "lookup")},
		cutCall(t, inbox, "Final answ", "answer in French"),
		urgentCall{result: final("Réponse finale")},
	)
	out := runUrgent(t, client, inbox, func(r *Runner) {
		r.Agent = agent
		r.Tools = tool.List{lookupTool()}
	})
	// An answer on the reserved call ends the turn as limited, as always.
	if out.Stop.Reason != "model_attempts" || out.Answer != "Réponse finale" {
		t.Fatalf("stop=%+v answer=%q", out.Stop, out.Answer)
	}
	intents := stepsOf(out.Turn, StepModelIntent)
	if len(intents) != 3 || !intents[1].Model.FinalAnswerOnly ||
		!intents[2].Model.FinalAnswerOnly {
		t.Fatal("the replacement must still be the reserved final-answer call")
	}
	if out.ModelAttempts != 2 {
		t.Fatalf(
			"attempts = %d; the cut call spent the reserve",
			out.ModelAttempts,
		)
	}
}

func TestUrgentSteerUnderCostCapChargesWorstCase(t *testing.T) {
	inbox := newWatchedInbox()
	client := newPricedUrgentClient(
		urgentCall{
			// An Anthropic-style stream reports input usage first.
			deltas: []llm.Delta{
				{Kind: llm.DeltaUsage, Usage: &llm.Usage{InputTokens: 50}},
				textDelta("partial"),
			},
			during: func(context.Context) { inbox.sendUrgent(t, "stop") },
			block:  true,
		},
		urgentCall{result: final("ok")},
	)
	agent := testAgent()
	agent.Settings.MaxTokens = 100
	out, err := Runner{Agent: agent, Client: client}.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Budget: &Budget{
				MaxCost: &Money{Amount: amount("0.01"), Currency: "USD"},
			},
			Input: []llm.Message{llm.UserMessage(llm.Text("do it"))},
		},
		WithSteering(inbox),
	)
	if err != nil {
		t.Fatal(err)
	}
	intent := stepsOf(out.Turn, StepModelIntent)[0].Model
	cut := settlements(out.Turn)[0]
	if cut.Interrupted != InterruptSteering || cut.Charge == nil ||
		intent.AtRisk == nil || cut.Charge.Total != intent.AtRisk.Total {
		t.Fatalf("charge=%+v at risk=%+v", cut.Charge, intent.AtRisk)
	}
	if out.Spend.Cost["USD"] < intent.AtRisk.Total {
		t.Fatalf(
			"spend %s omits the worst case %s",
			out.Spend.Cost["USD"],
			intent.AtRisk.Total,
		)
	}
}

func TestUrgentSteerDoesNotCutCallWhenReplacementExceedsCostCap(t *testing.T) {
	inbox := newWatchedInbox()
	client := newPricedUrgentClient(
		urgentCall{
			during: func(context.Context) { inbox.sendUrgent(t, "stop") },
			result: final("done"),
		},
	)
	agent := testAgent()
	agent.Settings.MaxTokens = 100
	out, err := Runner{Agent: agent, Client: client}.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Budget: &Budget{
				MaxCost: &Money{Amount: amount("0.002"), Currency: "USD"},
			},
			Input: []llm.Message{llm.UserMessage(llm.Text("do it"))},
		},
		WithSteering(inbox),
	)
	if err != nil {
		t.Fatal(err)
	}
	settled := stepsOf(out.Turn, StepModelSettled)[0]
	if settled.Model.Interrupted != InterruptNone {
		t.Fatal("a call whose replacement would not fit the cap was cut")
	}
	var found bool
	for _, w := range settled.Warnings {
		if w.Code == WarningSteering &&
			strings.Contains(w.Message, "cost cap") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no warning on the call's settlement: %+v", settled.Warnings)
	}
	if n := len(warningsOf(out.Turn, WarningSteering)); n < 1 {
		t.Fatal("the decline was not reported")
	}
}

func TestSteeredPartialIsTrimmedOrSkipped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partial string
		want    string
	}{
		{name: "whitespace only", partial: " \n\t "},
		{name: "trailing newline", partial: "Checking\n\n", want: "Checking"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inbox := newWatchedInbox()
			client := newUrgentClient(
				cutCall(t, inbox, tc.partial, "stop"),
				urgentCall{result: final("ok")},
			)
			runUrgent(t, client, inbox)
			next := client.inputs()[1].Messages
			var assistant []llm.Message
			for _, m := range next {
				if m.Role == llm.RoleAssistant {
					assistant = append(assistant, m)
				}
				if r := messageReminder(m); r != nil && tc.want == "" {
					t.Fatal("a reminder follows an empty partial")
				}
			}
			if tc.want == "" {
				if len(assistant) != 0 {
					t.Fatalf("a blank partial was projected: %+v", assistant)
				}
				return
			}
			if len(assistant) != 1 || assistant[0].Text() != tc.want {
				t.Fatalf("projected partial: %+v", assistant)
			}
		})
	}
}

func TestSteeredPartialKeepsOnlyText(t *testing.T) {
	result := final("see [1] ")
	result.Message.Content = append(
		result.Message.Content,
		llm.Content{
			Kind:      llm.ContentReasoning,
			Reasoning: &llm.Reasoning{Text: "hidden"},
		},
	)
	got := steeredPartial(&result)
	if len(got) != 2 || len(got[0].Content) != 1 ||
		got[0].Text() != "see [1]" {
		t.Fatalf("projected partial: %+v", got)
	}
}

func TestNextCallOnlySurvivesSteeringInterrupt(t *testing.T) {
	inbox := newWatchedInbox()
	if _, err := inbox.Send(Steer{Item: ContextItem{
		Message:      llm.UserMessage(llm.Text("the build is red")),
		NextCallOnly: true,
	}}); err != nil {
		t.Fatal(err)
	}
	client := newUrgentClient(
		cutCall(t, inbox, "Looking", "stop"),
		urgentCall{result: final("ok")},
	)
	runUrgent(t, client, inbox)
	if hasText(client.inputs()[1].Messages, "the build is red") < 0 {
		t.Fatal("a NextCallOnly item expired on a call the user cut short")
	}
}

func TestVetoHoldsAcrossSteeringInterrupt(t *testing.T) {
	inbox := newWatchedInbox()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	asked := 0
	veto := FinishHook{
		Name: "pii",
		Func: func(context.Context, FinishPlan) (FinishDecision, error) {
			asked++
			return FinishDecision{Reason: "contains PII"}, nil
		},
	}
	client := newUrgentClient(
		urgentCall{result: final("ssn 123")},
		cutCall(t, inbox, "Clean ans", "shorter please"),
		urgentCall{
			during: func(context.Context) { cancel() },
			block:  true,
		},
	)
	out, err := Runner{
		Agent:  testAgent(),
		Client: client,
		Hooks:  veto.Hooks(),
	}.Run(ctx, Turn{ID: "t1"}, startInput(), WithSteering(inbox))
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopInterrupted || out.Answer != "" {
		t.Fatalf(
			"stop=%+v answer=%q; the veto must stay in force",
			out.Stop,
			out.Answer,
		)
	}
}

func TestUrgentSteerRunLeavesNoGoroutines(t *testing.T) {
	before := goruntime.NumGoroutine()
	for range 10 {
		inbox := newWatchedInbox()
		client := newUrgentClient(
			cutCall(t, inbox, "partial", "stop"),
			urgentCall{result: final("ok")},
		)
		runUrgent(t, client, inbox)
	}
	deadline := time.Now().Add(5 * time.Second)
	for goruntime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf(
				"goroutines: %d before, %d after",
				before,
				goruntime.NumGoroutine(),
			)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSteeredTurnReplaysDeterministically(t *testing.T) {
	inbox := newWatchedInbox()
	client := newUrgentClient(
		urgentCall{
			deltas: []llm.Delta{
				{Kind: llm.DeltaUsage, Usage: &llm.Usage{InputTokens: 30}},
				textDelta("partial answer"),
			},
			during: func(context.Context) { inbox.sendUrgent(t, "stop") },
			block:  true,
			late:   []llm.Delta{textDelta("more")},
		},
		urgentCall{result: final("ok")},
	)
	out := runUrgent(t, client, inbox)
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
	if !sameJSONValue(view.Spend, out.Spend) ||
		view.ModelAttempts != out.ModelAttempts ||
		view.Answer != out.Answer {
		t.Fatalf("replay differs: %+v vs %+v", view.Spend, out.Spend)
	}
	projected, err := Project(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if hasText(projected, "partial answer") < 0 {
		t.Fatal("the partial is not projected from the decoded record")
	}
}

// crashOnSteeredSettlement loses the first steering settlement, as when a
// worker dies after the cancel and before the settlement is written.
type crashOnSteeredSettlement struct {
	*MemoryRecorder
	fired bool
}

func (c *crashOnSteeredSettlement) Commit(
	ctx context.Context,
	req CommitRequest,
) error {
	if !c.fired && req.Step.Kind == StepModelSettled &&
		req.Step.Model.Interrupted == InterruptSteering {
		c.fired = true
		return errors.New("injected: worker crashed")
	}
	return c.MemoryRecorder.Commit(ctx, req)
}

func TestCrashAfterSteeringCancelRecoversAsWorker(t *testing.T) {
	inbox := newWatchedInbox()
	client := newUrgentClient(
		cutCall(t, inbox, "partial", "use staging"),
		urgentCall{result: final("ok")},
	)
	rec := &crashOnSteeredSettlement{MemoryRecorder: NewMemoryRecorder()}
	runner := Runner{Agent: testAgent(), Client: client, Recorder: rec}
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
	out, err := runner.Run(
		t.Context(),
		saved,
		Continue{CommandID: "resume"},
		WithSteering(inbox),
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if got := settlements(out.Turn)[0].Interrupted; got != InterruptWorker {
		t.Fatalf("recovered cause = %q", got)
	}
	delivered := 0
	for _, step := range stepsOf(out.Turn, StepDelivered) {
		for _, item := range step.Items() {
			if strings.Contains(item.Message.Text(), "use staging") {
				delivered++
			}
		}
	}
	if delivered != 1 {
		t.Fatalf("the steer was recorded %d times", delivered)
	}
	if hasText(client.lastMessages(), "use staging") < 0 {
		t.Fatal("the steer did not reach the resent call")
	}
}

func TestUrgentSteerWithoutNotifierWarnsOnce(t *testing.T) {
	source := &tableSource{rows: []Steer{
		{
			Item: ContextItem{
				ID:      "s1",
				Message: llm.UserMessage(llm.Text("a")),
			},
			Urgent: true,
		},
	}}
	client := &scriptedClient{results: []llm.Result{final("one"), final("two")}}
	out, err := Runner{Agent: testAgent(), Client: client}.Run(
		t.Context(), Turn{ID: "t1"}, startInput(), WithSteering(source),
	)
	if err != nil {
		t.Fatal(err)
	}
	var unheard int
	for _, w := range warningsOf(out.Turn, WarningSteering) {
		if strings.Contains(w.Message, "SteeringNotifier") {
			unheard++
		}
	}
	if unheard != 1 {
		t.Fatalf("%d warnings for a source that cannot signal", unheard)
	}
}

func TestNonUrgentSteerDoesNotCut(t *testing.T) {
	inbox := newWatchedInbox()
	client := newUrgentClient(
		urgentCall{
			during: func(context.Context) {
				if _, err := inbox.SendText("also this"); err != nil {
					t.Error(err)
				}
			},
			result: final("done"),
		},
		urgentCall{result: final("done too")},
	)
	out := runUrgent(t, client, inbox)
	for _, fact := range settlements(out.Turn) {
		if fact.Interrupted != InterruptNone {
			t.Fatal("a non-urgent steer cut a call")
		}
	}
}

func TestInboxReadySignalsOnlyUrgentSteers(t *testing.T) {
	var in Inbox
	ready := in.Ready()
	if ready != in.Ready() {
		t.Fatal("Ready returned a different channel")
	}
	if _, err := in.SendText("later"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
		t.Fatal("a plain steer signaled Ready")
	default:
	}
	for range 3 {
		if _, err := in.SendUrgent("now"); err != nil {
			t.Fatal(err)
		}
	}
	<-ready
	select {
	case <-ready:
		t.Fatal("signals were not coalesced")
	default:
	}
	steers, err := in.Steers(t.Context(), "t1")
	if err != nil || len(steers) != 4 || steers[0].Urgent || !steers[1].Urgent {
		t.Fatalf("steers=%+v err=%v", steers, err)
	}
}

func TestInboxReadyIsSafeForConcurrentUse(t *testing.T) {
	var in Inbox
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_ = in.Ready()
			if _, err := in.SendUrgent("now"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	select {
	case <-in.Ready():
	default:
		t.Fatal("no signal after urgent sends")
	}
}

// stallingInbox is an Inbox whose Steers, once stall is set, blocks until
// its context ends, as a slow durable source might.
type stallingInbox struct {
	*Inbox
	mu      sync.Mutex
	stall   bool
	stalled chan struct{}
}

func (s *stallingInbox) Steers(
	ctx context.Context,
	turnID string,
) ([]Steer, error) {
	s.mu.Lock()
	stall := s.stall
	s.mu.Unlock()
	if stall {
		s.stalled <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.Inbox.Steers(ctx, turnID)
}

func TestStalledSourceStillLetsRunEnd(t *testing.T) {
	source := &stallingInbox{Inbox: NewInbox(), stalled: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := newUrgentClient(urgentCall{
		during: func(context.Context) {
			source.mu.Lock()
			source.stall = true
			source.mu.Unlock()
			if _, err := source.SendUrgent("stop"); err != nil {
				t.Error(err)
			}
			<-source.stalled
			cancel()
		},
		block: true,
	})
	rec := NewMemoryRecorder()
	out, err := Runner{Agent: testAgent(), Client: client, Recorder: rec}.Run(
		ctx,
		Turn{ID: "t1"},
		startInput(),
		WithSteering(source),
	)
	if err != nil || out.Stop.Kind != StopInterrupted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	left, err := source.Leftovers(saved)
	if err != nil || len(left) != 1 {
		t.Fatalf("leftovers=%+v err=%v; the claim was not released", left, err)
	}
}

// runCancelledMidStream runs a call that streams deltas, has the run's
// context cancelled, and then streams late frames.
func runCancelledMidStream(
	t *testing.T,
	def Agent,
	deltas []llm.Delta,
	late []llm.Delta,
) *ModelFact {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := newUrgentClient(urgentCall{
		deltas: deltas,
		during: func(context.Context) { cancel() },
		block:  true,
		late:   late,
	})
	out, err := Runner{Agent: def, Client: client}.Run(
		ctx,
		Turn{ID: "t1"},
		startInput(),
	)
	if err != nil {
		t.Fatal(err)
	}
	cut := settlements(out.Turn)[0]
	if cut.Interrupted != InterruptRun {
		t.Fatalf("cause = %q, want run", cut.Interrupted)
	}
	return cut
}

func TestRunInterruptCountsLateFrames(t *testing.T) {
	cut := runCancelledMidStream(
		t,
		testAgent(),
		[]llm.Delta{textDelta("abcd")},
		[]llm.Delta{textDelta(strings.Repeat("x", 40))},
	)
	// 4 text + 40 late text runes, four to a token.
	if cut.Result == nil || cut.Result.Usage == nil ||
		cut.Result.Usage.OutputTokens != 11 {
		t.Fatalf("recorded output estimate: %+v", cut.Result)
	}
}

func TestInterruptEstimateIsCappedAtMaxTokens(t *testing.T) {
	def := testAgent()
	def.Settings.MaxTokens = 5
	cut := runCancelledMidStream(
		t,
		def,
		[]llm.Delta{textDelta(strings.Repeat("x", 400))},
		nil,
	)
	if cut.Result == nil || cut.Result.Usage == nil ||
		cut.Result.Usage.OutputTokens != 5 {
		t.Fatalf("recorded output exceeds max_tokens: %+v", cut.Result)
	}
}

func TestAnswerAfterSteerOmitsTextBeforeThePause(t *testing.T) {
	inbox := newWatchedInbox()
	paused := final("Editing main.go")
	paused.Stop = llm.Stop{Kind: llm.StopPause}
	client := newUrgentClient(
		urgentCall{result: paused},
		cutCall(t, inbox, " and then", "stop, wrong file"),
		urgentCall{result: final("Editing util.go instead.")},
	)
	out := runUrgent(t, client, inbox)
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v", out.Stop)
	}
	if out.Answer != "Editing util.go instead." {
		t.Fatalf(
			"answer = %q; text from before the steer was joined",
			out.Answer,
		)
	}
}
