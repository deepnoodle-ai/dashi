package kernel

import (
	"slices"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

func TestReplayInterruptedModelRetainsUsageWithoutAttempt(t *testing.T) {
	def := definition()
	def.Limits.Interruptions = 3
	partial := callResult("call-1", "lookup")
	partial.Usage = &llm.Usage{InputTokens: 3, OutputTokens: 2}
	second := final("partial answer")
	second.Usage = &llm.Usage{InputTokens: 4, OutputTokens: 1}
	third := final("still partial")
	third.Usage = &llm.Usage{InputTokens: 2, OutputTokens: 1}
	turn := interruptionRecord(
		interruptionStart(def),
		interruptionModelIntent("model-1"),
		Step{
			Kind: StepModelSettled,
			Model: &ModelFact{
				AttemptID:   "model-1",
				MessageID:   "message-1",
				Result:      &partial,
				Error:       "lease lost",
				Interrupted: true,
			},
		},
		Step{Kind: StepStopped, Stop: &Stop{Kind: StopInterrupted}},
		Step{Kind: StepContinued},
		interruptionModelIntent("model-2"),
		Step{
			Kind: StepModelSettled,
			Model: &ModelFact{
				AttemptID:   "model-2",
				MessageID:   "message-2",
				Result:      &second,
				Interrupted: true,
			},
		},
		Step{Kind: StepStopped, Stop: &Stop{Kind: StopInterrupted}},
		Step{Kind: StepContinued},
		interruptionModelIntent("model-3"),
		Step{
			Kind: StepModelSettled,
			Model: &ModelFact{
				AttemptID:   "model-3",
				MessageID:   "message-3",
				Result:      &third,
				Interrupted: true,
			},
		},
		Step{Kind: StepStopped, Stop: &Stop{Kind: StopInterrupted}},
		Step{Kind: StepContinued},
	)
	state, err := replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if state.ModelAttempts != 0 {
		t.Fatalf("interrupted calls consumed %d model attempts", state.ModelAttempts)
	}
	if state.modelCalls != 3 {
		t.Fatalf("model call sequence=%d, want 3", state.modelCalls)
	}
	if state.Interruptions != 3 {
		t.Fatalf("resumes=%d, want 3", state.Interruptions)
	}
	if state.Spend.Usage.InputTokens != 9 ||
		state.Spend.Usage.OutputTokens != 4 {
		t.Fatalf("usage=%+v, want input 9 and output 4", state.Spend.Usage)
	}
	if len(state.Tools) != 0 {
		t.Fatalf("partial tool calls became actionable: %+v", state.Tools)
	}
	if turn.AnswerText() != "" {
		t.Fatalf("partial answer became final text: %q", turn.AnswerText())
	}
	if state.ReadyToContinue {
		t.Fatal("turn at the interruption limit is ready again")
	}
	turn.Steps = append(turn.Steps, Step{
		Version: 1,
		TurnID:  turn.ID,
		Seq:     int64(len(turn.Steps) + 1),
		Kind:    StepStopped,
		Stop: &Stop{
			Kind:   StopLimited,
			Reason: "interruptions",
		},
	})
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.ReadyToContinue {
		t.Fatal("interruption-limited turn is ready to continue")
	}
}

func TestReplayCrashWithModelIntentCanContinue(t *testing.T) {
	turn := interruptionRecord(
		interruptionStart(definition()),
		interruptionModelIntent("model-1"),
	)
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if !view.ReadyToContinue {
		t.Fatal("crashed model call cannot be picked up")
	}
	turn.Steps = append(turn.Steps, Step{
		Version: 1,
		TurnID:  turn.ID,
		Seq:     int64(len(turn.Steps) + 1),
		Kind:    StepContinued,
	})
	state, err := replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if state.Interruptions != 1 {
		t.Fatalf("crash resume count=%d, want 1", state.Interruptions)
	}
	if state.modelIntent != "model-1" {
		t.Fatalf("unclosed model intent=%q, want model-1", state.modelIntent)
	}
	if state.ModelAttempts != 0 {
		t.Fatalf("crashed call consumed %d attempts", state.ModelAttempts)
	}
}

func TestReplayReadyAtInterruptionLimitDependsOnStop(t *testing.T) {
	def := definition()
	def.Limits.Interruptions = 1
	steps := []Step{
		interruptionStart(def),
		{Kind: StepStopped, Stop: &Stop{Kind: StopInterrupted}},
		{Kind: StepContinued},
	}
	crashed := interruptionRecord(steps...)
	view, err := Replay(crashed)
	if err != nil {
		t.Fatal(err)
	}
	if view.ReadyToContinue {
		t.Fatal("crashed turn is ready after using its last resume")
	}
	interrupted := interruptionRecord(append(
		slices.Clone(steps),
		Step{Kind: StepStopped, Stop: &Stop{Kind: StopInterrupted}},
	)...)
	view, err = Replay(interrupted)
	if err != nil {
		t.Fatal(err)
	}
	if view.ReadyToContinue {
		t.Fatal("interrupted turn is ready after using its last resume")
	}
	failed := interruptionRecord(append(
		slices.Clone(steps),
		Step{Kind: StepStopped, Stop: &Stop{Kind: StopFailed}},
	)...)
	view, err = Replay(failed)
	if err != nil {
		t.Fatal(err)
	}
	if !view.ReadyToContinue {
		t.Fatal("failed turn cannot continue at the interruption limit")
	}
	failed.Steps = append(failed.Steps, Step{
		Version: 1,
		TurnID:  failed.ID,
		Seq:     int64(len(failed.Steps) + 1),
		Kind:    StepContinued,
	})
	view, err = Replay(failed)
	if err != nil {
		t.Fatal(err)
	}
	if view.Interruptions != 1 {
		t.Fatalf("non-interruption continuation counted as resume: %d", view.Interruptions)
	}
}

func TestReplayUncertainModelIntentNeedsReport(t *testing.T) {
	turn := interruptionRecord(
		interruptionStart(definition()),
		interruptionModelIntent("model-1"),
		Step{
			Kind: StepStopped,
			Stop: &Stop{
				Kind:   StopUncertain,
				Reason: "model outcome unknown",
			},
		},
	)
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.ReadyToContinue {
		t.Fatal("explicit uncertain model call needs a report")
	}
}

func TestReplayInterruptionPreservesSettledAnswer(t *testing.T) {
	answer := final("finished before pause")
	turn := interruptionRecord(
		interruptionStart(definition()),
		interruptionModelIntent("model-1"),
		Step{
			Kind: StepModelSettled,
			Model: &ModelFact{
				AttemptID: "model-1",
				MessageID: "message-1",
				Result:    &answer,
			},
		},
		Step{Kind: StepStopped, Stop: &Stop{Kind: StopInterrupted}},
		Step{Kind: StepContinued},
	)
	state, err := replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if !state.awaitingClassification {
		t.Fatal("completed model result was lost before classification")
	}
	if state.ModelAttempts != 1 {
		t.Fatalf("settled call attempts=%d, want 1", state.ModelAttempts)
	}
	if state.latestResult == nil ||
		state.latestResult.Message.Text() != "finished before pause" {
		t.Fatal("completed model result was not retained")
	}
}

func TestReplayReportedModelNonexecutionCanContinue(t *testing.T) {
	turn := interruptionRecord(
		interruptionStart(definition()),
		interruptionModelIntent("model-1"),
		Step{
			Kind: StepModelSettled,
			Model: &ModelFact{
				AttemptID: "model-1",
				MessageID: "message-1",
				Error:     "connection lost",
				Uncertain: true,
			},
		},
		Step{Kind: StepStopped, Stop: &Stop{Kind: StopUncertain}},
		Step{
			Kind: StepModelReconciled,
			Model: &ModelFact{
				AttemptID: "model-1",
				MessageID: "message-1",
				Error:     "request was not dispatched",
				Failure:   llm.FailureNotDispatched,
				Evidence:  "provider request log",
			},
		},
	)
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if !view.ReadyToContinue {
		t.Fatal("verified nonexecution did not make turn ready")
	}
	turn.Steps = append(turn.Steps, Step{
		Version: 1,
		TurnID:  turn.ID,
		Seq:     int64(len(turn.Steps) + 1),
		Kind:    StepContinued,
	})
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
		interruptionStart(definition()),
		interruptionModelIntent("model-1"),
		Step{
			Kind: StepModelSettled,
			Model: &ModelFact{
				AttemptID: "model-1",
				MessageID: "message-1",
				Error:     "request was not dispatched",
				Failure:   llm.FailureNotDispatched,
				Evidence:  "provider request log",
			},
		},
		Step{Kind: StepContinued},
	)
	state, err := replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if state.failedModel != "" {
		t.Fatalf("reported crash nonexecution still fails the turn: %q",
			state.failedModel)
	}
	if state.modelIntent != "" {
		t.Fatalf("reported model call remains open: %q", state.modelIntent)
	}
	if state.Interruptions != 1 {
		t.Fatalf("crash resume count=%d, want 1", state.Interruptions)
	}
}

func TestReplayStartedToolResumptionDependsOnStop(t *testing.T) {
	call := tool.Call{
		ID:   "call-1",
		Name: "lookup",
		Args: []byte(`{}`),
	}
	def := tool.Definition{
		Name:   "lookup",
		Kind:   tool.KindLocal,
		Schema: []byte(`{"type":"object"}`),
	}
	intent := interruptionModelIntent("model-1")
	intent.Model.Tools = []tool.Definition{def}
	result := llm.Result{
		Message: llm.AssistantMessage(llm.Content{
			Kind:     llm.ContentToolCall,
			ToolCall: &call,
		}),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
	base := []Step{
		interruptionStart(definition()),
		intent,
		Step{
			Kind: StepModelSettled,
			Model: &ModelFact{
				AttemptID: "model-1",
				MessageID: "message-1",
				Result:    &result,
			},
		},
		Step{
			Kind: StepToolDecision,
			Tool: &ToolFact{
				Call:     call,
				Decision: ToolDecision{Kind: DecisionAllow},
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
		{name: "uncertain", stop: StopUncertain, ready: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := append([]Step(nil), base...)
			steps = append(steps, Step{
				Kind: StepStopped,
				Stop: &Stop{Kind: tc.stop},
			})
			view, err := Replay(interruptionRecord(steps...))
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
		interruptionStart(definition()),
		Step{
			Kind: StepStopped,
			Stop: &Stop{
				Kind:   StopLimited,
				Reason: "deadline",
			},
		},
	)
	view, err := Replay(turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.ReadyToContinue {
		t.Fatal("deadline-limited turn is ready to continue")
	}
}

func TestLimitsInterruptionDefaultAndValidation(t *testing.T) {
	if got := DefaultLimits().Interruptions; got != 10 {
		t.Fatalf("default interruption limit=%d, want 10", got)
	}
	def := definition()
	if got := def.Limits.interruptionLimit(); got != 10 {
		t.Fatalf("legacy definition interruption limit=%d, want 10", got)
	}
	def.Limits.Interruptions = -1
	if err := def.Validate(); err == nil {
		t.Fatal("negative interruption limit was accepted")
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

func interruptionStart(def Definition) Step {
	return Step{
		Kind: StepStarted,
		Started: &StartFact{
			Definition: def,
			Input: []llm.Message{
				llm.UserMessage(llm.Text("answer")),
			},
		},
	}
}

func interruptionModelIntent(id string) Step {
	messageID := "message-" + id[len("model-"):]
	return Step{
		Kind: StepModelIntent,
		Model: &ModelFact{
			AttemptID:     id,
			MessageID:     messageID,
			RequestDigest: "request-" + id,
			Provider:      "test",
			API:           "test.chat",
			Model:         "fixture",
		},
	}
}
