package dashi

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// unsettledCallsTurn records one model result with three calls that end
// started, waiting, and uncertain, and an uncertain stop.
func unsettledCallsTurn() Turn {
	def := tool.Definition{
		Name:   "work",
		Kind:   tool.KindLocal,
		Schema: []byte(`{"type":"object"}`),
	}
	intent := interruptionModelIntent("model-1")
	intent.Model.Tools = []tool.Definition{def}
	var parts []llm.Content
	var calls []tool.Call
	for _, id := range []string{"started", "waiting", "uncertain"} {
		call := tool.Call{ID: id, Name: "work", Args: []byte(`{}`)}
		calls = append(calls, call)
		parts = append(parts, llm.Content{Kind: llm.ContentToolCall, ToolCall: &call})
	}
	result := llm.Result{
		Message:   llm.AssistantMessage(parts...),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
	steps := []Step{
		interruptionStart(testAgent()),
		intent,
		{Kind: StepModelSettled, Model: &ModelFact{
			AttemptID: "model-1",
			MessageID: "message-1",
			Result:    &result,
		}},
	}
	for _, call := range calls {
		steps = append(steps,
			Step{Kind: StepToolDecision, Tool: &ToolFact{
				Call:    call,
				Verdict: ToolVerdict{Kind: DecisionAllow},
			}},
			Step{Kind: StepToolStarted, Tool: &ToolFact{Call: call}},
		)
	}
	wait := tool.Outcome{State: tool.StateWaiting, Wait: &tool.Wait{Reason: "external"}}
	steps = append(steps,
		Step{Kind: StepToolWaiting, Tool: &ToolFact{Call: calls[1], Outcome: &wait}},
		Step{Kind: StepToolSettled, Tool: &ToolFact{Call: calls[2]}},
		Step{Kind: StepStopped, Stop: &Stop{Kind: StopUncertain}},
	)
	return interruptionRecord(steps...)
}

func TestCanceledTurnProjectsUnsettledCallsAndFreezes(t *testing.T) {
	turn := unsettledCallsTurn()
	if _, err := Project(turn); err == nil {
		t.Fatal("an open turn with unsettled calls projected")
	}
	runner := Runner{Client: &scriptedClient{}}
	canceled, err := runner.Run(t.Context(), turn, Cancel{CommandID: "close"})
	if err != nil || !canceled.Stop.Terminal {
		t.Fatalf("cancel=%+v err=%v", canceled.Stop, err)
	}
	messages, err := Project(canceled.Turn)
	if err != nil {
		t.Fatal(err)
	}
	// The input, the model's calls, then one error result per call.
	if len(messages) != 5 {
		t.Fatalf("projected %d messages, want 5", len(messages))
	}
	for i, id := range []string{"started", "waiting", "uncertain"} {
		r := messages[2+i].Content[0].ToolResult
		if r == nil || r.CallID != id || !r.IsError ||
			!sameJSONValue(r.Output, tool.TextOutput(endedToolText)) {
			t.Fatalf("result %d = %+v", i, r)
		}
	}
	before, _ := json.Marshal(messages)

	// A late result is recorded and counted, but the projection is frozen.
	late, err := runner.Run(t.Context(), canceled.Turn, Settle{
		CommandID: "late",
		CallID:    "started",
		Outcome:   new(tool.Succeeded("done late")),
	})
	if err != nil || late.Tools["started"].Status != ToolSettled {
		t.Fatalf("late=%+v err=%v", late.Tools["started"], err)
	}
	after, err := Project(late.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(after); !bytes.Equal(raw, before) {
		t.Fatalf("late result changed the projection:\n%s\n%s", before, raw)
	}
}

// A canceled turn with a model attempt in flight projects its input only.
func TestCanceledTurnDropsPendingModelAttempt(t *testing.T) {
	turn := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelIntent("model-1"),
	)
	canceled, err := (Runner{Client: &scriptedClient{}}).Run(
		t.Context(), turn, Cancel{CommandID: "close"},
	)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := Project(canceled.Turn)
	if err != nil || len(messages) != 1 || messages[0].Role != llm.RoleUser {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
}

// Continue on a turn with no recorded stop tells the next model call that
// the worker stopped mid-turn; the call after it no longer sees that.
func TestCrashResumeCarriesReminder(t *testing.T) {
	crashed := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelIntent("model-1"),
	)
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "lookup"),
		final("done"),
	}}
	runner := Runner{
		Client: client,
		Tools:  tool.List{localTool("lookup", execFunc(lookup42))},
	}
	out, err := runner.Run(t.Context(), crashed, Continue{CommandID: "resume"})
	if err != nil || out.Stop.Kind != StopCompleted || len(client.inputs) != 2 {
		t.Fatalf("resume=%+v err=%v inputs=%d", out.Stop, err, len(client.inputs))
	}
	if n := countReminders(client.inputs[0].Messages); n != 1 {
		t.Fatalf("first call saw %d restart reminders, want 1", n)
	}
	if n := countReminders(client.inputs[1].Messages); n != 0 {
		t.Fatalf("second call saw %d restart reminders, want 0", n)
	}

	// A Continue after a recorded stop adds nothing.
	paused := make(chan struct{})
	close(paused)
	stopped, err := (Runner{Client: client}).Run(
		t.Context(), Turn{ID: "p"}, limitsStart(testAgent()), WithPause(paused),
	)
	if err != nil || stopped.Stop.Kind != StopInterrupted {
		t.Fatalf("pause=%+v err=%v", stopped.Stop, err)
	}
	client.results = append(client.results, final("ok"))
	if _, err := (Runner{Client: client}).Run(
		t.Context(), stopped.Turn, Continue{},
	); err != nil {
		t.Fatal(err)
	}
	last := client.inputs[len(client.inputs)-1]
	if n := countReminders(last.Messages); n != 0 {
		t.Fatalf("resume after a pause saw %d restart reminders", n)
	}
}

func countReminders(messages []llm.Message) int {
	n := 0
	for _, m := range messages {
		for _, part := range m.Content {
			if part.Reminder != nil && part.Reminder.Name == "worker-restarted" {
				n++
			}
		}
	}
	return n
}

// A late model result after a cancel counts in Spend, but Answer, like
// Project, stays fixed at the terminal stop.
func TestLateModelResultKeepsEndedAnswer(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{{}},
		errors:  []error{errors.New("connection lost")},
	}
	first, err := start(t, client, writeTool(), NewMemoryRecorder(), Hooks{})
	if err != nil || first.Stop.Kind != StopUncertain {
		t.Fatalf("start: stop=%+v err=%v", first.Stop, err)
	}
	runner := Runner{Client: client}
	canceled, err := runner.Run(t.Context(), first.Turn, Cancel{CommandID: "close"})
	if err != nil || !canceled.Stop.Terminal {
		t.Fatalf("cancel=%+v err=%v", canceled.Stop, err)
	}
	before, err := Project(canceled.Turn)
	if err != nil {
		t.Fatal(err)
	}
	late := final("late")
	late.Usage = &llm.Usage{InputTokens: 5, OutputTokens: 2}
	out, err := runner.Run(t.Context(), canceled.Turn, Settle{
		CommandID:   "late",
		CallID:      "t1:model:1",
		ModelResult: &late,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Spend.Usage.OutputTokens != 2 {
		t.Fatalf("late result not counted: spend=%+v", out.Spend.Usage)
	}
	if out.Answer != "" || out.Next() != NextNone {
		t.Fatalf("answer=%q next=%s", out.Answer, out.Next())
	}
	view, err := Replay(out.Turn)
	if err != nil || view.Answer != "" {
		t.Fatalf("replayed answer=%q err=%v", view.Answer, err)
	}
	after, err := Project(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if !bytes.Equal(a, b) {
		t.Fatalf("late result changed the projection:\n%s\n%s", a, b)
	}
}

// A second crash before any model call reaches the provider does not add a
// second reminder: the next request carries one.
func TestRepeatedCrashResumeCarriesOneReminder(t *testing.T) {
	crashed := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelIntent("model-1"),
	)
	// The first resume records its Continue, then its worker crashes
	// before the model call: keep the record only up to that Continue.
	client := &scriptedClient{results: []llm.Result{final("unused")}}
	runner := Runner{Client: client}
	first, err := runner.Run(t.Context(), crashed, Continue{CommandID: "resume-1"})
	if err != nil {
		t.Fatal(err)
	}
	cut := slices.IndexFunc(first.Turn.Steps, func(s Step) bool {
		return s.Kind == StepContinued
	})
	if cut < 0 {
		t.Fatal("no continuation step recorded")
	}
	crashedAgain := cloneTurn(first.Turn)
	crashedAgain.Steps = crashedAgain.Steps[:cut+1]
	if view, err := Replay(crashedAgain); err != nil || view.Next() != NextResume {
		t.Fatalf("second crash: next=%s err=%v", view.Next(), err)
	}

	client = &scriptedClient{results: []llm.Result{final("done")}}
	runner.Client = client
	out, err := runner.Run(t.Context(), crashedAgain, Continue{CommandID: "resume-2"})
	if err != nil || out.Stop.Kind != StopCompleted || len(client.inputs) != 1 {
		t.Fatalf("resume=%+v err=%v inputs=%d", out.Stop, err, len(client.inputs))
	}
	if n := countReminders(client.inputs[0].Messages); n != 1 {
		t.Fatalf("request saw %d restart reminders, want 1", n)
	}
}
