package dashi

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestReplayInterruptedModelRetainsUsageWithoutModelCall(t *testing.T) {
	def := testAgent()
	def.Limits.Resumes = 3
	partial := callResult("call-1", "lookup")
	partial.Usage = &llm.Usage{InputTokens: 3, OutputTokens: 2}
	second := final("partial answer")
	second.Usage = &llm.Usage{InputTokens: 4, OutputTokens: 1}
	third := final("still partial")
	third.Usage = &llm.Usage{InputTokens: 2, OutputTokens: 1}
	turn := interruptionRecord(
		interruptionStart(def),
		interruptionModelCallStart("model-1"),
		Step{
			Kind: StepModelCallFinished,
			Model: &ModelFact{
				ModelCallID: "model-1",
				MessageID:   "message-1",
				Result:      &partial,
				Error:       "lease lost",
				Interrupted: InterruptRun,
			},
		},
		Step{Kind: StepTurnStopped, Stop: &Stop{Kind: StopInterrupted}},
		Step{Kind: StepTurnContinued},
		interruptionModelCallStart("model-2"),
		Step{
			Kind: StepModelCallFinished,
			Model: &ModelFact{
				ModelCallID: "model-2",
				MessageID:   "message-2",
				Result:      &second,
				Interrupted: InterruptRun,
			},
		},
		Step{Kind: StepTurnStopped, Stop: &Stop{Kind: StopInterrupted}},
		Step{Kind: StepTurnContinued},
		interruptionModelCallStart("model-3"),
		Step{
			Kind: StepModelCallFinished,
			Model: &ModelFact{
				ModelCallID: "model-3",
				MessageID:   "message-3",
				Result:      &third,
				Interrupted: InterruptRun,
			},
		},
		Step{Kind: StepTurnStopped, Stop: &Stop{Kind: StopInterrupted}},
		Step{Kind: StepTurnContinued},
	)
	state, err := replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if state.ModelCalls != 0 {
		t.Fatalf("interrupted calls consumed %d model attempts", state.ModelCalls)
	}
	if state.modelCalls != 3 {
		t.Fatalf("model call sequence=%d, want 3", state.modelCalls)
	}
	if state.Resumes != 3 {
		t.Fatalf("resumes=%d, want 3", state.Resumes)
	}
	if state.Spend.Usage.InputTokens != 9 ||
		state.Spend.Usage.OutputTokens != 10 {
		t.Fatalf("usage=%+v, want input 9 and output 10", state.Spend.Usage)
	}
	if len(state.Tools) != 0 {
		t.Fatalf("partial tool calls became actionable: %+v", state.Tools)
	}
	if answerText(turn.Steps) != "" {
		t.Fatalf("partial answer became final text: %q", answerText(turn.Steps))
	}
	if state.ReadyToContinue {
		t.Fatal("turn at the interruption limit is ready again")
	}
	turn.Steps = append(
		turn.Steps,
		Step{
			Version: 1,
			TurnID:  turn.ID,
			Seq:     int64(len(turn.Steps) + 1),
			Kind:    StepTurnStopped,
			Stop:    testStop(StopLimited, LimitResumes),
		},
	)
	view, err := turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.ReadyToContinue {
		t.Fatal("interruption-limited turn is ready to continue")
	}
}

func TestReplayRejectsUnknownInterruptCause(t *testing.T) {
	partial := final("partial")
	turn := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelCallStart("model-1"),
		Step{
			Kind: StepModelCallFinished,
			Model: &ModelFact{
				ModelCallID: "model-1",
				MessageID:   "message-1",
				Result:      &partial,
				Error:       "cut short",
				Interrupted: InterruptCause("meteor"),
			},
		},
	)
	_, err := turn.View()
	if err == nil || !strings.Contains(err.Error(), "interrupt cause") {
		t.Fatalf("replay of an unknown interrupt cause: err=%v", err)
	}
}

func TestInterruptCauseSurvivesJSONRoundTrip(t *testing.T) {
	fact := ModelFact{ModelCallID: "model-1", Interrupted: InterruptWorker}
	data, err := json.Marshal(fact)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"interrupted":"worker"`) {
		t.Fatalf("encoded cause is missing: %s", data)
	}
	var decoded ModelFact
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Interrupted != InterruptWorker {
		t.Fatalf("decoded cause = %q, want worker", decoded.Interrupted)
	}
	none, err := json.Marshal(ModelFact{ModelCallID: "model-2"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(none), "interrupted") {
		t.Fatalf("an uninterrupted call encodes a cause: %s", none)
	}
}

func TestReplayCrashWithModelCallStartCanContinue(t *testing.T) {
	turn := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelCallStart("model-1"),
	)
	view, err := turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if !view.ReadyToContinue {
		t.Fatal("crashed model call cannot be picked up")
	}
	turn.Steps = append(
		turn.Steps,
		Step{
			Version: 1,
			TurnID:  turn.ID,
			Seq:     int64(len(turn.Steps) + 1),
			Kind:    StepTurnContinued,
		},
	)
	state, err := replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if state.Resumes != 1 {
		t.Fatalf("crash resume count=%d, want 1", state.Resumes)
	}
	if state.modelCallStart != "model-1" {
		t.Fatalf("unclosed model intent=%q, want model-1", state.modelCallStart)
	}
	if state.ModelCalls != 0 {
		t.Fatalf("crashed call consumed %d attempts", state.ModelCalls)
	}
}

func TestReplayReadyAtResumeLimitDependsOnStop(t *testing.T) {
	def := testAgent()
	def.Limits.Resumes = 1
	// A model attempt comes first: a pause before any attempt interrupted
	// no work and does not count.
	steps := []Step{
		interruptionStart(def),
		interruptionModelCallStart("model-1"),
		{Kind: StepModelCallFinished, Model: &ModelFact{
			ModelCallID: "model-1",
			MessageID:   "message-1",
			Error:       "pause requested",
			Interrupted: InterruptRun,
			NotSent:     true,
		}},
		{Kind: StepTurnStopped, Stop: &Stop{Kind: StopInterrupted}},
		{Kind: StepTurnContinued},
	}
	crashed := interruptionRecord(steps...)
	view, err := crashed.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.ReadyToContinue {
		t.Fatal("crashed turn is ready after using its last resume")
	}
	interrupted := interruptionRecord(append(
		slices.Clone(steps),
		Step{Kind: StepTurnStopped, Stop: &Stop{Kind: StopInterrupted}},
	)...)
	view, err = interrupted.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.ReadyToContinue {
		t.Fatal("interrupted turn is ready after using its last resume")
	}
	failed := interruptionRecord(append(
		slices.Clone(steps),
		Step{Kind: StepTurnStopped, Stop: &Stop{Kind: StopFailed}},
	)...)
	view, err = failed.View()
	if err != nil {
		t.Fatal(err)
	}
	if !view.ReadyToContinue {
		t.Fatal("failed turn cannot continue at the interruption limit")
	}
	failed.Steps = append(
		failed.Steps,
		Step{
			Version: 1,
			TurnID:  failed.ID,
			Seq:     int64(len(failed.Steps) + 1),
			Kind:    StepTurnContinued,
		},
	)
	view, err = failed.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Resumes != 1 {
		t.Fatalf(
			"non-interruption continuation counted as resume: %d",
			view.Resumes,
		)
	}
}

func TestReplayResultUnknownModelCallStartNeedsReport(t *testing.T) {
	turn := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelCallStart("model-1"),
		Step{
			Kind: StepTurnStopped,
			Stop: &Stop{Kind: StopResultUnknown, Reason: "model outcome unknown"},
		},
	)
	view, err := turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.ReadyToContinue {
		t.Fatal("explicit uncertain model call needs a report")
	}
}

func TestReplayInterruptionPreservesFinishedAnswer(t *testing.T) {
	answer := final("finished before pause")
	turn := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelCallStart("model-1"),
		Step{
			Kind: StepModelCallFinished,
			Model: &ModelFact{
				ModelCallID: "model-1",
				MessageID:   "message-1",
				Result:      &answer,
			},
		},
		Step{Kind: StepTurnStopped, Stop: &Stop{Kind: StopInterrupted}},
		Step{Kind: StepTurnContinued},
	)
	state, err := replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if !state.awaitingClassification {
		t.Fatal("completed model result was lost before classification")
	}
	if state.ModelCalls != 1 {
		t.Fatalf("settled call attempts=%d, want 1", state.ModelCalls)
	}
	if state.latestResult == nil ||
		state.latestResult.Message.Text() != "finished before pause" {
		t.Fatal("completed model result was not retained")
	}
}

func TestReplayReportedModelNonexecutionCanContinue(t *testing.T) {
	turn := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelCallStart("model-1"),
		Step{
			Kind: StepModelCallFinished,
			Model: &ModelFact{
				ModelCallID:   "model-1",
				MessageID:     "message-1",
				Error:         "connection lost",
				ResultUnknown: true,
			},
		},
		Step{Kind: StepTurnStopped, Stop: &Stop{Kind: StopResultUnknown}},
		Step{
			Kind: StepModelCallFinishedByHost,
			Model: &ModelFact{
				ModelCallID: "model-1",
				MessageID:   "message-1",
				Error:       "request was not dispatched",
				SendStatus:  llm.SendStatusNotSent,
				Evidence:    "provider request log",
			},
		},
	)
	view, err := turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if !view.ReadyToContinue {
		t.Fatal("verified nonexecution did not make turn ready")
	}
	turn.Steps = append(
		turn.Steps,
		Step{
			Version: 1,
			TurnID:  turn.ID,
			Seq:     int64(len(turn.Steps) + 1),
			Kind:    StepTurnContinued,
		},
	)
	state, err := replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if state.failedModel != "" {
		t.Fatalf("reported nonexecution still fails the turn: %q", state.failedModel)
	}
	if state.unresolvedModel != "" {
		t.Fatalf("reported model call remains uncertain: %q", state.unresolvedModel)
	}
}

func TestReplayCrashReportedModelNonexecutionCanContinue(t *testing.T) {
	turn := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelCallStart("model-1"),
		Step{
			Kind: StepModelCallFinished,
			Model: &ModelFact{
				ModelCallID: "model-1",
				MessageID:   "message-1",
				Error:       "request was not dispatched",
				SendStatus:  llm.SendStatusNotSent,
				Evidence:    "provider request log",
			},
		},
		Step{Kind: StepTurnContinued},
	)
	state, err := replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if state.failedModel != "" {
		t.Fatalf(
			"reported crash nonexecution still fails the turn: %q",
			state.failedModel,
		)
	}
	if state.modelCallStart != "" {
		t.Fatalf("reported model call remains open: %q", state.modelCallStart)
	}
	if state.Resumes != 1 {
		t.Fatalf("crash resume count=%d, want 1", state.Resumes)
	}
}

func TestReplayStartedToolResumptionDependsOnStop(t *testing.T) {
	call := tool.Call{ID: "call-1", Name: "lookup", Args: []byte(`{}`)}
	def := tool.Definition{
		Name:   "lookup",
		Kind:   tool.KindLocal,
		Schema: []byte(`{"type":"object"}`),
	}
	callStart := interruptionModelCallStart("model-1")
	callStart.Model.Tools = []tool.Definition{def}
	result := llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &call,
		}),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
	base := []Step{
		interruptionStart(testAgent()),
		callStart,
		Step{
			Kind: StepModelCallFinished,
			Model: &ModelFact{
				ModelCallID: "model-1",
				MessageID:   "message-1",
				Result:      &result,
			},
		},
		Step{
			Kind: StepToolDecision,
			Tool: &ToolFact{
				Call:    call,
				Verdict: ToolVerdict{Kind: DecisionAllow},
			},
		},
		Step{Kind: StepToolStarted, Tool: &ToolFact{Call: call}},
	}
	for _, tc := range []struct {
		name  string
		stop  StopKind
		ready bool
	}{
		{name: "interrupted", stop: StopInterrupted, ready: true},
		{name: "uncertain", stop: StopResultUnknown, ready: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := append([]Step(nil), base...)
			steps = append(steps, Step{Kind: StepTurnStopped, Stop: &Stop{Kind: tc.stop}})
			view, err := interruptionRecord(steps...).View()
			if err != nil {
				t.Fatal(err)
			}
			if view.ReadyToContinue != tc.ready {
				t.Fatalf("ready=%t, want %t", view.ReadyToContinue, tc.ready)
			}
		})
	}
}

func TestReplayDeadlineLimitIsFinal(t *testing.T) {
	turn := interruptionRecord(
		interruptionStart(testAgent()),
		Step{
			Kind: StepTurnStopped,
			Stop: testStop(StopLimited, LimitDeadline),
		},
	)
	view, err := turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.ReadyToContinue {
		t.Fatal("deadline-limited turn is ready to continue")
	}
}

func interruptionRecord(steps ...Step) Turn {
	turn := Turn{ID: "interruption-test"}
	for i, step := range steps {
		step.Version = 1
		step.TurnID = turn.ID
		step.Seq = int64(i + 1)
		turn.Steps = append(turn.Steps, step)
	}
	return turn
}

func interruptionStart(def Agent) Step {
	return Step{
		Kind: StepTurnStarted,
		Started: &StartFact{
			Agent: def,
			Input: []llm.Message{llm.UserMessage(llm.Text("answer"))},
		},
	}
}

func interruptionModelCallStart(id string) Step {
	messageID := "message-" + id[len("model-"):]
	return Step{
		Kind: StepModelCallStarted,
		Model: &ModelFact{
			ModelCallID: id,
			MessageID:   messageID,
			RequestHash: "request-" + id,
			Provider:    "test",
			API:         "test.chat",
			Model:       "fixture",
		},
	}
}
