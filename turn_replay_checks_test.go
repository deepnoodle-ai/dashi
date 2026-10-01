package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
)

// testStop builds a stop the way the runner records it, for records built by
// hand: a limit kind fills Limit and Reason, and Terminal is set for the
// stops that end a turn whatever its state (completed, canceled, deadline,
// interruptions, and model attempts, which the runner records only once
// they are used up).
func testStop(kind StopKind, limit LimitKind) *Stop {
	stop := &Stop{Kind: kind}
	if limit != "" {
		stop.Reason = string(limit)
		stop.Limit = &LimitDetail{Kind: limit}
	}
	switch {
	case kind == StopCompleted, kind == StopCanceled:
		stop.Terminal = true
	case limit == LimitDeadline, limit == LimitResumes:
		stop.Terminal = true
	}
	return stop
}

func TestTerminalStops(t *testing.T) {
	limited := func(kind LimitKind) Stop {
		return Stop{Kind: StopLimited, Limit: &LimitDetail{Kind: kind}}
	}
	tests := []struct {
		name        string
		stop        Stop
		attempts    int
		costUnknown int
		want        bool
	}{
		{name: "completed", stop: Stop{Kind: StopCompleted}, want: true},
		{name: "canceled", stop: Stop{Kind: StopCanceled}, want: true},
		{name: "waiting", stop: Stop{Kind: StopWaiting}},
		{name: "interrupted", stop: Stop{Kind: StopInterrupted}},
		{name: "failed", stop: Stop{Kind: StopFailed}},
		{name: "hook failed", stop: Stop{Kind: StopFailed, Reason: "hook_failed"}},
		{name: "refused", stop: Stop{Kind: StopRefused}},
		{name: "uncertain", stop: Stop{Kind: StopResultUnknown}},
		{name: "deadline", stop: limited(LimitDeadline), want: true},
		{name: "resumes", stop: limited(LimitResumes), want: true},
		{name: "model attempts", stop: limited(LimitModelCalls), attempts: 8},
		{name: "cost after last attempt", stop: limited(LimitCost), attempts: 8},
		{name: "host after last attempt", stop: limited(LimitHost), attempts: 9},
		{name: "cost", stop: limited(LimitCost)},
		{name: "cost with unknown spend", stop: limited(LimitCost), costUnknown: 1, want: true},
		{name: "unpriced model", stop: Stop{Kind: StopLimited, Limit: &LimitDetail{Kind: LimitCost, UnpricedModel: "m"}}},
		{name: "output tokens", stop: limited(LimitOutputTokens), attempts: 7},
		{name: "total tokens", stop: limited(LimitTotalTokens)},
		{name: "provider pauses", stop: limited(LimitProviderPauses)},
		{name: "host", stop: limited(LimitHost)},
		{name: "reason without limit", stop: Stop{Kind: StopLimited, Reason: "deadline"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &replayState{}
			s.Stop, s.ModelCalls = tt.stop, tt.attempts
			s.attemptCap = 8
			s.Revision = 2
			s.Spend.CostUnknown = tt.costUnknown
			if got := s.terminal(); got != tt.want {
				t.Fatalf("terminal=%t, want %t", got, tt.want)
			}
			if !tt.want {
				return
			}
			// Invariant: a terminal stop cannot continue under any raise.
			huge := &Budget{
				MaxOutputTokens: 1 << 40,
				MaxTotalTokens:  1 << 40,
				MaxCost:         &Money{Amount: money.Amount(1 << 60), Currency: "USD"},
			}
			if s.canContinueWith(huge, 1<<30) || s.canContinue() {
				t.Fatal("a terminal stop can continue")
			}
		})
	}
}

func TestRecordedStopsCarryTerminal(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := start(t, client, nil, NewMemoryRecorder(), Hooks{})
	if err != nil || !out.Stop.Terminal || !out.TurnView.Stop.Terminal {
		t.Fatalf("completed stop=%+v err=%v", out.Stop, err)
	}
	last := out.Turn.Steps[len(out.Turn.Steps)-1]
	raw, err := json.Marshal(last.Stop)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"kind":"completed","terminal":true}` {
		t.Fatalf("recorded stop=%s", raw)
	}

	pause := make(chan struct{})
	close(pause)
	paused, err := (Runner{Client: client}).Run(
		t.Context(), Turn{ID: "p"}, limitsStart(testAgent()), WithPause(pause),
	)
	if err != nil || paused.Stop.Kind != StopInterrupted || paused.Stop.Terminal {
		t.Fatalf("interrupted stop=%+v err=%v", paused.Stop, err)
	}
	raw, err = json.Marshal(paused.Turn.Steps[len(paused.Turn.Steps)-1].Stop)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "terminal") {
		t.Fatalf("non-terminal stop recorded %s", raw)
	}
}

func TestReplayRejectsMisstatedTerminal(t *testing.T) {
	for _, stop := range []*Stop{
		{Kind: StopCompleted},
		{Kind: StopWaiting, Terminal: true},
		{Kind: StopLimited, Reason: "deadline", Limit: &LimitDetail{Kind: LimitDeadline}},
		{
			Kind:     StopLimited,
			Reason:   "cost",
			Limit:    &LimitDetail{Kind: LimitCost, CostLimit: 100, Currency: "USD"},
			Terminal: true,
		},
	} {
		turn := interruptionRecord(
			interruptionStart(testAgent()),
			Step{Kind: StepTurnStopped, Stop: stop},
		)
		if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) || !strings.Contains(err.Error(), "terminal") {
			t.Fatalf("replay of %+v: err=%v", stop, err)
		}
	}
}

func TestDeadlineStopEndsTurn(t *testing.T) {
	def := testAgent()
	def.Limits.Deadline = time.Now().Add(-time.Minute)
	client := &scriptedClient{results: []llm.Result{final("never")}}
	rec := NewMemoryRecorder()
	runner := Runner{Client: client, Recorder: rec}
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, limitsStart(def))
	if err != nil || out.Stop.Kind != StopLimited || !out.Stop.Terminal {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	commands := map[string]Command{
		"continue": Continue{CommandID: "c", Budget: &Budget{MaxOutputTokens: 1 << 20}},
		"deliver": AddMessages{CommandID: "d", Items: []ContextItem{
			{Origin: "user", Message: llm.UserMessage(llm.Text("more"))},
		}},
		"cancel": Cancel{CommandID: "x"},
	}
	for name, command := range commands {
		if _, err := runner.Run(t.Context(), out.Turn, command); !errors.Is(err, ErrTurnEnded) {
			t.Fatalf("%s: err=%v, want ErrTurnEnded", name, err)
		}
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil || client.sends != 0 || len(saved.Steps) != len(out.Turn.Steps) {
		t.Fatal("a command on an ended turn ran or recorded")
	}
}

// A retried Cancel gets its recorded answer; a new one is refused.
func TestCancelOnEndedTurn(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "write")}}
	rec := NewMemoryRecorder()
	first, err := start(t, client, writeTool(), rec, deferredHooks())
	if err != nil || first.Stop.Kind != StopWaiting {
		t.Fatalf("start=%+v err=%v", first.Stop, err)
	}
	runner := Runner{Client: client, Recorder: rec}
	cancel := Cancel{CommandID: "cancel", Reason: "user"}
	canceled, err := runner.Run(t.Context(), first.Turn, cancel)
	if err != nil || !canceled.Stop.Terminal {
		t.Fatalf("cancel=%+v err=%v", canceled.Stop, err)
	}
	again, err := runner.Run(t.Context(), canceled.Turn, cancel)
	if err != nil || again.Stop.Kind != StopCanceled {
		t.Fatalf("retried cancel=%+v err=%v", again.Stop, err)
	}
	if _, err := runner.Run(t.Context(), canceled.Turn, Cancel{CommandID: "other"}); !errors.Is(err, ErrTurnEnded) {
		t.Fatalf("second cancel err=%v", err)
	}
	// The unstarted call was settled as not executed, so approving it now
	// reports the real reason.
	approve := first.Pending[0].Approve()
	approve.CommandID = "approve"
	if _, err := runner.Run(t.Context(), canceled.Turn, approve); !errors.Is(err, ErrTurnEnded) {
		t.Fatalf("approval err=%v", err)
	}
}

// Every terminal stop applies the cancel rules: an approval pending when the
// deadline stopped the turn is settled as not executed and cannot be given.
func TestDeadlineStopSettlesPendingApproval(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "write")}}
	first, err := start(t, client, writeTool(), NewMemoryRecorder(), deferredHooks())
	if err != nil || first.Stop.Kind != StopWaiting {
		t.Fatalf("start=%+v err=%v", first.Stop, err)
	}
	turn := cloneTurn(first.Turn)
	turn.Steps = append(turn.Steps, Step{
		Version: 1,
		TurnID:  turn.ID,
		Seq:     int64(len(turn.Steps) + 1),
		Kind:    StepTurnStopped,
		Stop:    testStop(StopLimited, LimitDeadline),
	})
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Pending) != 0 ||
		view.Tools["call-1"].Status != ToolFinished ||
		view.Tools["call-1"].Outcome.State != tool.StateNotExecuted {
		t.Fatalf("pending=%+v tool=%+v", view.Pending, view.Tools["call-1"])
	}
	approve := first.Pending[0].Approve()
	approve.CommandID = "approve"
	runner := Runner{Client: client}
	if _, err := runner.Run(t.Context(), turn, approve); !errors.Is(err, ErrTurnEnded) {
		t.Fatalf("approval err=%v", err)
	}
}

// A started tool's late result is still recorded after the turn ends.
func TestCanceledTurnRecordsLateToolResult(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "remote")}}
	rec := NewMemoryRecorder()
	panics := execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		panic("boom")
	})
	runner := toolRunner(client, rec, panics)
	first, err := startTurn(testAgent(), runner)
	if err != nil || first.Stop.Kind != StopResultUnknown {
		t.Fatalf("start=%+v err=%v", first.Stop, err)
	}
	canceled, err := runner.Run(t.Context(), first.Turn, Cancel{CommandID: "cancel"})
	if err != nil || !canceled.Stop.Terminal {
		t.Fatalf("cancel=%+v err=%v", canceled.Stop, err)
	}
	late, err := runner.Run(t.Context(), canceled.Turn, Resolve{
		CommandID: "late",
		CallID:    "call-1",
		Outcome:   new(tool.Succeeded("done late")),
	})
	if err != nil || late.Stop.Kind != StopCanceled ||
		late.Tools["call-1"].Status != ToolFinished {
		t.Fatalf("late=%+v tool=%+v err=%v", late.Stop, late.Tools["call-1"], err)
	}
	if client.sends != 1 {
		t.Fatalf("sends=%d, want no model call after the late result", client.sends)
	}
}

// A late model result on an ended turn is recorded for accounting, but its
// tool calls never enter the turn.
func TestEndedTurnIgnoresLateModelToolCalls(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{{}},
		errors:  []error{errors.New("connection lost")},
	}
	first, err := start(t, client, writeTool(), NewMemoryRecorder(), Hooks{})
	if err != nil || first.Stop.Kind != StopResultUnknown {
		t.Fatalf("start: stop=%+v err=%v", first.Stop, err)
	}
	turn := cloneTurn(first.Turn)
	turn.Steps = append(turn.Steps, Step{
		Version: 1,
		TurnID:  turn.ID,
		Seq:     int64(len(turn.Steps) + 1),
		Kind:    StepTurnStopped,
		Stop:    testStop(StopLimited, LimitDeadline),
	})
	late := callResult("call-1", "write")
	out, err := (Runner{Client: client, Tools: writeTool()}).Run(
		t.Context(),
		turn,
		Resolve{CommandID: "late", CallID: "t1:model:1", ModelResult: &late},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 0 || len(out.Pending) != 0 || out.ReadyToContinue {
		t.Fatalf("tools=%+v pending=%+v", out.Tools, out.Pending)
	}
}

func TestHostedApprovalAfterEndIsRefused(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "write")}}
	rec := NewMemoryRecorder()
	first, err := start(t, client, writeTool(), rec, deferredHooks())
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Client: client, Recorder: rec}
	canceled, err := runner.Run(t.Context(), first.Turn, Cancel{CommandID: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(t.Context(), canceled.Turn, Resolve{
		CommandID:      "hosted",
		CallID:         "req-1",
		HostedApproval: &llm.HostedApprovalResponse{RequestID: "req-1", Approve: true},
	})
	if !errors.Is(err, ErrTurnEnded) || err.Error() != "dashi: turn has ended: canceled" {
		t.Fatalf("err=%v", err)
	}
}

// A started tool's late result is accepted after a deadline stop too.
func TestDeadlineStopRecordsLateToolResult(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "remote")}}
	panics := execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		panic("boom")
	})
	first, err := startTurn(testAgent(), toolRunner(client, NewMemoryRecorder(), panics))
	if err != nil || first.Stop.Kind != StopResultUnknown {
		t.Fatalf("start=%+v err=%v", first.Stop, err)
	}
	turn := cloneTurn(first.Turn)
	turn.Steps = append(turn.Steps, Step{
		Version: 1,
		TurnID:  turn.ID,
		Seq:     int64(len(turn.Steps) + 1),
		Kind:    StepTurnStopped,
		Stop:    testStop(StopLimited, LimitDeadline),
	})
	runner := toolRunner(client, nil, panics)
	late, err := runner.Run(t.Context(), turn, Resolve{
		CommandID: "late",
		CallID:    "call-1",
		Outcome:   new(tool.Succeeded("done late")),
	})
	if err != nil || late.Tools["call-1"].Status != ToolFinished || !late.Stop.Terminal {
		t.Fatalf("late=%+v err=%v", late.Stop, err)
	}
	_, err = runner.Run(t.Context(), late.Turn, Cancel{CommandID: "cancel"})
	if err == nil || err.Error() != "dashi: turn has ended: limited (deadline)" {
		t.Fatalf("cancel err=%v", err)
	}
}

// A budget stop after the last model attempt is not terminal: a raise of the
// budget and of the model-attempt cap together resumes it, and replay agrees
// with the recorded flag.
func TestBudgetStopAfterLastAttemptContinuesWithBothRaises(t *testing.T) {
	def := testAgent()
	def.Limits.ModelCalls = 1
	call := callResult("call-1", "remote")
	call.Usage = &llm.Usage{OutputTokens: 50}
	client := &scriptedClient{results: []llm.Result{call, final("done")}}
	ok := execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		return tool.Succeeded("done"), nil
	})
	runner := toolRunner(client, NewMemoryRecorder(), ok)
	out, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		CommandID: "start",
		Agent:     def,
		Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		Budget:    &Budget{MaxOutputTokens: 10},
	})
	if err != nil || out.Stop.Kind != StopLimited ||
		out.Stop.Limit == nil || out.Stop.Limit.Kind != LimitOutputTokens {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	if out.Stop.Terminal {
		t.Fatal("budget stop after the last attempt is terminal")
	}
	if _, err := Replay(out.Turn); err != nil {
		t.Fatal(err)
	}
	out, err = runner.Run(t.Context(), out.Turn, Continue{
		CommandID:  "raise",
		Budget:     &Budget{MaxOutputTokens: 1000},
		ModelCalls: 3,
	})
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
}

// A late model result that would make a non-terminal limited stop terminal
// is rejected: a turn ends only through a recorded stop. No runner path
// reaches this today, so the check is exercised on the replay state that
// model_call_finished and model_call_finished_by_host consult.
func TestLateResultCannotEndTurnSilently(t *testing.T) {
	s := &replayState{}
	s.attemptCap = 2
	s.Stop = Stop{Kind: StopLimited, Limit: &LimitDetail{Kind: LimitHost, Code: "plan"}}
	s.ModelCalls = 1
	if err := s.checkStillOpen(); err != nil {
		t.Fatalf("open stop: %v", err)
	}
	// A late result that uses the last attempt leaves the stop open, since a
	// Continue can raise the model-attempt cap.
	s.ModelCalls = 2
	if err := s.checkStillOpen(); err != nil {
		t.Fatalf("spent cap: %v", err)
	}
	s.Stop.Terminal = true
	if err := s.checkStillOpen(); err != nil {
		t.Fatalf("terminal stop: %v", err)
	}
}

// Resolve checks a late model result against replay before saving it when
// the turn rests at an open limited stop, and skips the replay otherwise.
func TestCheckLateResultStep(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{{}},
		errors:  []error{errors.New("connection lost")},
	}
	first, err := (Runner{Client: client}).Run(
		t.Context(), Turn{ID: "t1"}, limitsStart(testAgent()),
	)
	if err != nil || first.Stop.Kind != StopResultUnknown {
		t.Fatalf("start: stop=%+v err=%v", first.Stop, err)
	}
	bogus := Step{Kind: StepModelCallFinishedByHost, Model: &ModelFact{ModelCallID: "nope"}}
	state, err := replay(first.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkLateResultStep(first.Turn, state, bogus); err != nil {
		t.Fatalf("checked a turn that is not at a limited stop: %v", err)
	}

	turn := cloneTurn(first.Turn)
	turn.Steps = append(turn.Steps, Step{
		Version: 1,
		TurnID:  turn.ID,
		Seq:     int64(len(turn.Steps) + 1),
		Kind:    StepTurnStopped,
		Stop: &Stop{
			Kind:   StopLimited,
			Reason: "plan",
			Limit:  &LimitDetail{Kind: LimitHost, Code: "plan"},
		},
	})
	state, err = replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkLateResultStep(turn, state, bogus); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("bogus step at an open limited stop: err=%v", err)
	}
	late := final("late")
	out, err := (Runner{Client: client}).Run(
		t.Context(),
		turn,
		Resolve{CommandID: "late", CallID: "t1:model:1", ModelResult: &late},
	)
	if err != nil || out.Stop.Kind != StopLimited || out.Stop.Terminal {
		t.Fatalf("valid late result: stop=%+v err=%v", out.Stop, err)
	}
}

func writeTool() tool.List {
	return tool.List{tool.Tool{Definition: tool.Definition{
		Name:   "write",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindLocal,
	}}}
}
