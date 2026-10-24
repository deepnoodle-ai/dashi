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
// hand: a limit kind fills Limit and Reason, and Ended is set for the
// stops that end a turn whatever its state (completed, canceled, deadline,
// resumes, and model calls, which the runner records only once
// they are used up).
func testStop(kind StopKind, limit LimitKind) *Stop {
	stop := &Stop{Kind: kind}
	if limit != "" {
		stop.Reason = string(limit)
		stop.Limit = &LimitDetail{Kind: limit}
	}
	switch {
	case kind == StopCompleted, kind == StopCanceled:
		stop.Ended = true
	case limit == LimitDeadline, limit == LimitResumes:
		stop.Ended = true
	}
	return stop
}

func TestEndedStops(t *testing.T) {
	limited := func(kind LimitKind) Stop {
		return Stop{Kind: StopLimited, Limit: &LimitDetail{Kind: kind}}
	}
	tests := []struct {
		name        string
		stop        Stop
		modelCalls  int
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
		{name: "result unknown", stop: Stop{Kind: StopResultUnknown}},
		{name: "deadline", stop: limited(LimitDeadline), want: true},
		{name: "resumes", stop: limited(LimitResumes), want: true},
		{name: "model calls", stop: limited(LimitModelCalls), modelCalls: 8},
		{name: "cost after last model call", stop: limited(LimitCost), modelCalls: 8},
		{name: "host after last model call", stop: limited(LimitHost), modelCalls: 9},
		{name: "cost", stop: limited(LimitCost)},
		{name: "cost with unknown spend", stop: limited(LimitCost), costUnknown: 1, want: true},
		{name: "unpriced model", stop: Stop{Kind: StopLimited, Limit: &LimitDetail{Kind: LimitCost, UnpricedModel: "m"}}},
		{name: "output tokens", stop: limited(LimitOutputTokens), modelCalls: 7},
		{name: "total tokens", stop: limited(LimitTotalTokens)},
		{name: "provider pauses", stop: limited(LimitProviderPauses)},
		{name: "host", stop: limited(LimitHost)},
		{name: "reason without limit", stop: Stop{Kind: StopLimited, Reason: "deadline"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &replayState{}
			s.Stop, s.ModelCalls = tt.stop, tt.modelCalls
			s.modelCallCap = 8
			s.LastSeq = 2
			s.Spend.CostUnknown = tt.costUnknown
			if got := s.ended(); got != tt.want {
				t.Fatalf("ended=%t, want %t", got, tt.want)
			}
			if !tt.want {
				return
			}
			// Invariant: a stop that ended the turn cannot continue under
			// any raise.
			huge := &Budget{
				MaxOutputTokens: 1 << 40,
				MaxTotalTokens:  1 << 40,
				MaxCost:         &Money{Amount: money.Amount(1 << 60), Currency: "USD"},
			}
			if s.canContinueWith(huge, 1<<30) || s.canContinue() {
				t.Fatal("a ending stop can continue")
			}
		})
	}
}

func TestRecordedStopsCarryEnded(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := start(t, client, nil, NewMemoryRecorder(), Hooks{})
	if err != nil || !out.Stop.Ended || !out.TurnView.Stop.Ended {
		t.Fatalf("completed stop=%+v err=%v", out.Stop, err)
	}
	last := out.Turn.Steps[len(out.Turn.Steps)-1]
	raw, err := json.Marshal(last.Stop)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"kind":"completed","ended":true}` {
		t.Fatalf("recorded stop=%s", raw)
	}

	pause := make(chan struct{})
	close(pause)
	paused, err := (Runner{Client: client}).Run(
		t.Context(), Turn{ID: "p"}, limitsStart(testAgent()), WithPause(pause),
	)
	if err != nil || paused.Stop.Kind != StopInterrupted || paused.Stop.Ended {
		t.Fatalf("interrupted stop=%+v err=%v", paused.Stop, err)
	}
	raw, err = json.Marshal(paused.Turn.Steps[len(paused.Turn.Steps)-1].Stop)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ended") {
		t.Fatalf("non-ending stop recorded %s", raw)
	}
}

func TestReplayRejectsMisstatedEnded(t *testing.T) {
	for _, stop := range []*Stop{
		{Kind: StopCompleted},
		{Kind: StopWaiting, Ended: true},
		{Kind: StopLimited, Reason: "deadline", Limit: &LimitDetail{Kind: LimitDeadline}},
		{
			Kind:   StopLimited,
			Reason: "cost",
			Limit:  &LimitDetail{Kind: LimitCost, CostLimit: 100, Currency: "USD"},
			Ended:  true,
		},
	} {
		turn := interruptionRecord(
			interruptionStart(testAgent()),
			Step{Kind: StepTurnStopped, Stop: stop},
		)
		if _, err := turn.View(); !errors.Is(err, ErrInvalidTurn) || !strings.Contains(err.Error(), "ended") {
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
	if err != nil || out.Stop.Kind != StopLimited || !out.Stop.Ended {
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

// A retried Cancel gets its recorded result; a new one is refused.
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
	if err != nil || !canceled.Stop.Ended {
		t.Fatalf("cancel=%+v err=%v", canceled.Stop, err)
	}
	again, err := runner.Run(t.Context(), canceled.Turn, cancel)
	if err != nil || again.Stop.Kind != StopCanceled {
		t.Fatalf("retried cancel=%+v err=%v", again.Stop, err)
	}
	if _, err := runner.Run(t.Context(), canceled.Turn, Cancel{CommandID: "other"}); !errors.Is(err, ErrTurnEnded) {
		t.Fatalf("second cancel err=%v", err)
	}
	// The unstarted call was finished as not executed, so approving it now
	// reports the real reason.
	approve := first.Pending[0].Approve()
	approve.CommandID = "approve"
	if _, err := runner.Run(t.Context(), canceled.Turn, approve); !errors.Is(err, ErrTurnEnded) {
		t.Fatalf("approval err=%v", err)
	}
}

// Every stop that ends the turn applies the cancel rules: an approval
// pending when the deadline stopped the turn is finished as not executed
// and cannot be given.
func TestDeadlineStopFinishesPendingApproval(t *testing.T) {
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
	view, err := turn.View()
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

// An ended turn's conversation is closed: a started tool's result is refused.
func TestCanceledTurnRefusesLateToolResult(t *testing.T) {
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
	if err != nil || !canceled.Stop.Ended {
		t.Fatalf("cancel=%+v err=%v", canceled.Stop, err)
	}
	late, err := runner.Run(t.Context(), canceled.Turn, Resolve{
		CommandID: "late",
		CallID:    "call-1",
		Outcome:   new(tool.Succeeded("done late")),
	})
	if !errors.Is(err, ErrTurnEnded) ||
		late.Tools["call-1"].Status != ToolResultUnknown {
		t.Fatalf("late=%+v tool=%+v err=%v", late.Stop, late.Tools["call-1"], err)
	}
	saved, err := rec.Load(t.Context(), canceled.Turn.ID)
	if err != nil || len(saved.Steps) != len(canceled.Turn.Steps) {
		t.Fatalf("late result was recorded: steps=%d err=%v", len(saved.Steps), err)
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

func TestProviderToolApprovalAfterEndIsRefused(t *testing.T) {
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
		CommandID:            "provider tool",
		CallID:               "req-1",
		ProviderToolApproval: &llm.ProviderToolApprovalResponse{RequestID: "req-1", Approve: true},
	})
	if !errors.Is(err, ErrTurnEnded) || err.Error() != "dashi: turn has ended: canceled" {
		t.Fatalf("err=%v", err)
	}
}

// A started tool's late result is refused after a deadline stop too, and a
// record holding one fails View.
func TestDeadlineStopRefusesLateToolResult(t *testing.T) {
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
	_, err = runner.Run(t.Context(), turn, Resolve{
		CommandID: "late",
		CallID:    "call-1",
		Outcome:   new(tool.Succeeded("done late")),
	})
	if err == nil || err.Error() != "dashi: turn has ended: limited (deadline)" {
		t.Fatalf("late err=%v", err)
	}
	outcome := tool.Succeeded("done late")
	turn.Steps = append(turn.Steps, Step{
		Version: 1,
		TurnID:  turn.ID,
		Seq:     int64(len(turn.Steps) + 1),
		Kind:    StepToolFinished,
		Tool:    &ToolFact{Call: first.Tools["call-1"].Call, Outcome: &outcome},
	})
	if _, err := turn.View(); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("view err=%v", err)
	}
}

// A budget stop after the last model call does not end the turn: a raise of
// the budget and of the model call limit together resumes it, and View
// agrees with the recorded flag.
func TestBudgetStopAfterLastModelCallContinuesWithBothRaises(t *testing.T) {
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
	if out.Stop.Ended {
		t.Fatal("budget stop after the last model call is terminal")
	}
	if _, err := out.Turn.View(); err != nil {
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

// A late model result that would make an open limited stop end the turn
// is rejected: a turn ends only through a recorded stop. No runner path
// reaches this today, so the check is exercised on the replay state that
// model_call_finished and model_call_finished_by_host consult.
func TestLateResultCannotEndTurnSilently(t *testing.T) {
	s := &replayState{}
	s.modelCallCap = 2
	s.Stop = Stop{Kind: StopLimited, Limit: &LimitDetail{Kind: LimitHost, Code: "plan"}}
	s.ModelCalls = 1
	if err := s.checkStillOpen(); err != nil {
		t.Fatalf("open stop: %v", err)
	}
	// A late result that uses the last model call leaves the stop open,
	// since a Continue can raise the model call limit.
	s.ModelCalls = 2
	if err := s.checkStillOpen(); err != nil {
		t.Fatalf("spent cap: %v", err)
	}
	s.Stop.Ended = true
	if err := s.checkStillOpen(); err != nil {
		t.Fatalf("ending stop: %v", err)
	}
}

// Resolve checks a late model result by rebuilding the view before saving
// it, whether or not the turn rests at an open limited stop.
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
	if err := checkLateResultStep(first.Turn, bogus); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("bogus step at a result_unknown stop: err=%v", err)
	}
	// A host-supplied result reports a finished call, never an interrupted
	// one, which history would leave out though the view acts on it.
	answer := final("late")
	interrupted := Step{Kind: StepModelCallFinishedByHost, Model: &ModelFact{
		ModelCallID: "t1:model:1",
		MessageID:   "t1:model:1:message",
		Result:      &answer,
		Interrupted: InterruptRun,
	}}
	if err := checkLateResultStep(first.Turn, interrupted); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("interrupted host result: err=%v", err)
	}
	interrupted.Model.Interrupted = InterruptNone
	if err := checkLateResultStep(first.Turn, interrupted); err != nil {
		t.Fatalf("host result: err=%v", err)
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
	if err := checkLateResultStep(turn, bogus); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("bogus step at an open limited stop: err=%v", err)
	}
	late := final("late")
	out, err := (Runner{Client: client}).Run(
		t.Context(),
		turn,
		Resolve{CommandID: "late", CallID: "t1:model:1", ModelResult: &late},
	)
	if err != nil || out.Stop.Kind != StopLimited || out.Stop.Ended {
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

// compactionStart is a compaction turn's Start with a recorded fallback.
func compactionStart() Start {
	return Start{
		CommandID: "compact",
		Origin:    StartCompaction,
		Agent:     Agent{Model: "fixture", NoTools: true},
		Input:     []llm.Message{llm.UserMessage(llm.Text("Summarize."))},
		Compaction: &CompactionFact{
			Keep:            []llm.Message{llm.UserMessage(llm.Text("kept"))},
			SummaryFallback: "Earlier: the user asked for X.",
		},
	}
}

func fallbackCount(v TurnView) int {
	n := 0
	for _, w := range v.Warnings {
		if w.Code == WarningSummaryFallback {
			n++
		}
	}
	return n
}

// The engine ends a compaction turn at a failure or an empty reply, with
// one summary_fallback warning on the ending stop; a summary ends it with
// none.
func TestCompactionTurnEndsAtItsStop(t *testing.T) {
	for name, client := range map[string]*scriptedClient{
		"failed": {errors: []error{errors.New("provider down")}},
		"empty":  {results: []llm.Result{final("")}},
	} {
		out, err := Runner{Client: client}.Run(t.Context(), Turn{ID: "c"}, compactionStart())
		if err != nil || !out.Stop.Ended || out.Next() != NextNone {
			t.Fatalf("%s: stop=%+v err=%v", name, out.Stop, err)
		}
		if fallbackCount(out.TurnView) != 1 {
			t.Fatalf("%s: warnings=%v", name, out.Warnings)
		}
		if _, ok := out.ReplacementHistory(); !ok {
			t.Fatalf("%s: no replacement", name)
		}
	}
	out, err := Runner{Client: &scriptedClient{results: []llm.Result{final("<summary>S</summary>")}}}.
		Run(t.Context(), Turn{ID: "c"}, compactionStart())
	if err != nil || !out.Stop.Ended || fallbackCount(out.TurnView) != 0 {
		t.Fatalf("summary: stop=%+v warnings=%v err=%v", out.Stop, out.Warnings, err)
	}
}

// A host hold does not end a compaction turn; Cancel then ends it, and the
// canceling stop records the warning.
func TestCompactionTurnWaitsAtAHostHold(t *testing.T) {
	hooks := Hooks{BeforeModel: []ModelHook{{
		Name: "balance",
		Func: func(context.Context, ModelPlan) (ModelDecision, error) {
			return ModelDecision{Stop: &Stop{
				Kind: StopLimited, Reason: "balance too low",
				Limit: &LimitDetail{Kind: LimitHost, Code: "balance"},
			}}, nil
		},
	}}}
	runner := Runner{Client: &scriptedClient{}, Hooks: hooks}
	held, err := runner.Run(t.Context(), Turn{ID: "c"}, compactionStart())
	if err != nil || held.Stop.Ended || !heldStop(held.TurnView) || fallbackCount(held.TurnView) != 0 {
		t.Fatalf("held: stop=%+v warnings=%v err=%v", held.Stop, held.Warnings, err)
	}
	out, err := runner.Run(t.Context(), held.Turn, Cancel{CommandID: "close"})
	if err != nil || !out.Stop.Ended || fallbackCount(out.TurnView) != 1 {
		t.Fatalf("canceled: stop=%+v warnings=%v err=%v", out.Stop, out.Warnings, err)
	}
}
