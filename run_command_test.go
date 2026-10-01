package dashi

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestRunnerStartUsesHistoryWithoutRecordingIt(t *testing.T) {
	history := []llm.Message{llm.UserMessage(llm.Text("earlier conversation"))}
	input := llm.UserMessage(llm.Text("current input"))
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	out, err := (Runner{
		Client:   client,
		Recorder: NewMemoryRecorder(),
	}).Run(
		t.Context(),
		Turn{ID: "history-start"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			History:   history,
			Input:     []llm.Message{input},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("turn did not complete: %+v", out.Stop)
	}
	if len(client.inputs) != 1 {
		t.Fatalf("model requests = %d, want 1", len(client.inputs))
	}
	want := []llm.Message{history[0], input}
	if !reflect.DeepEqual(client.inputs[0].Messages, want) {
		t.Fatalf("model request omitted history: %#v", client.inputs[0].Messages)
	}
	if out.Turn.Steps[0].Started.HistoryFingerprint == "" {
		t.Fatal("start did not record a history fingerprint")
	}
	encoded, err := json.Marshal(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "earlier conversation") {
		t.Fatal("saved turn contains prior conversation")
	}
}

func TestRunnerContinueUsesHistoryWithoutRecordingIt(t *testing.T) {
	history := []llm.Message{llm.UserMessage(llm.Text("prior exchange"))}
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first := startPausedHistoryTurn(t, client, recorder, history)
	out, err := (Runner{
		Client:   client,
		Recorder: recorder,
	}).Run(
		t.Context(),
		first.Turn,
		Continue{CommandID: "continue", History: history},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("continued turn did not complete: %+v", out.Stop)
	}
	if len(client.inputs) != 1 {
		t.Fatalf("model requests = %d, want 1", len(client.inputs))
	}
	want := []llm.Message{history[0], llm.UserMessage(llm.Text("current input"))}
	if !reflect.DeepEqual(client.inputs[0].Messages, want) {
		t.Fatalf("continued request omitted history: %#v", client.inputs[0].Messages)
	}
	encoded, err := json.Marshal(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "prior exchange") {
		t.Fatal("saved turn contains prior conversation")
	}
}

func TestRunnerContinueWarnsOnDifferentHistoryAndProceeds(t *testing.T) {
	history := []llm.Message{llm.UserMessage(llm.Text("original history"))}
	changed := []llm.Message{llm.UserMessage(llm.Text("different history"))}
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first := startPausedHistoryTurn(t, client, recorder, history)
	out, err := (Runner{
		Client:   client,
		Recorder: recorder,
	}).Run(
		t.Context(),
		first.Turn,
		Continue{CommandID: "continue", History: changed},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("history mismatch stopped the turn: %+v", out.Stop)
	}
	if len(out.Warnings) != 1 {
		t.Fatalf("outcome warnings = %d, want 1", len(out.Warnings))
	}
	if out.Warnings[0].Code != WarningHistoryMismatch {
		t.Fatalf("wrong warning code: %s", out.Warnings[0].Code)
	}
	if len(first.Turn.Steps) >= len(out.Turn.Steps) {
		t.Fatal("continued turn did not save new steps")
	}
	continued := out.Turn.Steps[len(first.Turn.Steps)]
	if continued.Kind != StepContinued {
		t.Fatalf("first new step = %s, want continued", continued.Kind)
	}
	if len(continued.Warnings) != 1 {
		t.Fatalf("continued step warnings = %d, want 1", len(continued.Warnings))
	}
	if len(client.inputs) != 1 {
		t.Fatalf("model requests = %d, want 1", len(client.inputs))
	}
	if len(client.inputs[0].Messages) == 0 {
		t.Fatal("model request has no messages")
	}
	if !reflect.DeepEqual(client.inputs[0].Messages[0], changed[0]) {
		t.Fatal("model request did not use the supplied history")
	}
}

func TestRunnerStrictContinueRejectsDifferentHistory(t *testing.T) {
	history := []llm.Message{llm.UserMessage(llm.Text("original history"))}
	changed := []llm.Message{llm.UserMessage(llm.Text("compacted history"))}
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first := startPausedHistoryTurn(t, client, recorder, history)
	runner := Runner{Client: client, Recorder: recorder}
	_, err := runner.Run(
		t.Context(),
		first.Turn,
		Continue{
			CommandID:         "continue",
			History:           changed,
			OnHistoryMismatch: HistoryMismatchReject,
		},
	)
	if !errors.Is(err, ErrHistoryMismatch) {
		t.Fatalf("error = %v, want ErrHistoryMismatch", err)
	}
	if len(client.inputs) != 0 {
		t.Fatal("rejected continue called the model")
	}
	saved, err := recorder.Load(t.Context(), first.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Steps) != len(first.Turn.Steps) {
		t.Fatal("rejected continue changed the saved turn")
	}

	// The same command ID with the original history resumes the turn.
	out, err := runner.Run(
		t.Context(),
		saved,
		Continue{
			CommandID:         "continue",
			History:           history,
			OnHistoryMismatch: HistoryMismatchReject,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("strict continue did not complete: %+v", out.Stop)
	}
	if len(out.Warnings) != 0 {
		t.Fatalf("matching history caused warning: %#v", out.Warnings)
	}
	if !reflect.DeepEqual(client.inputs[0].Messages[0], history[0]) {
		t.Fatal("model request did not use the original history")
	}
}

func TestRunnerStrictContinueRejectsInStreamErrorSlot(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first := startPausedHistoryTurn(t, client, recorder, nil)
	changed := []llm.Message{llm.UserMessage(llm.Text("new session input"))}
	var events int
	var got error
	for event, err := range (Runner{
		Client:   client,
		Recorder: recorder,
	}).Stream(
		t.Context(),
		first.Turn,
		Continue{
			CommandID:         "continue",
			History:           changed,
			OnHistoryMismatch: HistoryMismatchReject,
		},
	) {
		if err != nil {
			got = err
			continue
		}
		if event.Outcome != nil {
			events++
		}
	}
	if !errors.Is(got, ErrHistoryMismatch) || events != 0 {
		t.Fatalf("stream error = %v, outcomes = %d", got, events)
	}
}

func TestRunnerStrictContinueRejectsAtInterruptionLimit(t *testing.T) {
	history := []llm.Message{llm.UserMessage(llm.Text("original history"))}
	changed := []llm.Message{llm.UserMessage(llm.Text("changed history"))}
	recorder, limited := interruptionLimitedHistoryTurn(t, history)
	_, err := (Runner{
		Client:   &scriptedClient{},
		Recorder: recorder,
	}).Run(
		t.Context(),
		limited,
		Continue{
			CommandID:         "at-limit",
			History:           changed,
			OnHistoryMismatch: HistoryMismatchReject,
		},
	)
	if !errors.Is(err, ErrHistoryMismatch) {
		t.Fatalf("error = %v, want ErrHistoryMismatch", err)
	}
	assertSavedSteps(t, recorder, limited)

	// Warn mode still records the interruption-limit stop.
	out, err := (Runner{
		Client:   &scriptedClient{},
		Recorder: recorder,
	}).Run(
		t.Context(),
		limited,
		Continue{CommandID: "at-limit", History: changed},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopLimited {
		t.Fatalf("warn mode stop = %+v, want limited", out.Stop)
	}
}

func TestRunnerInvalidHistoryAtInterruptionLimitCommitsNothing(t *testing.T) {
	recorder, limited := interruptionLimitedHistoryTurn(t, nil)
	_, err := (Runner{
		Client:   &scriptedClient{},
		Recorder: recorder,
	}).Run(
		t.Context(),
		limited,
		Continue{CommandID: "at-limit", History: []llm.Message{{Role: llm.RoleUser}}},
	)
	if err == nil {
		t.Fatal("invalid history was accepted")
	}
	assertSavedSteps(t, recorder, limited)
}

// interruptionLimitedHistoryTurn returns a turn that has used its only
// resume, so the next Continue reaches the interruption limit.
func interruptionLimitedHistoryTurn(
	t *testing.T,
	history []llm.Message,
) (*MemoryRecorder, Turn) {
	t.Helper()
	def := testAgent()
	def.Limits.Interruptions = 1
	recorder := NewMemoryRecorder()
	// The first pause follows a model attempt, so resuming from it counts.
	paused := make(chan struct{})
	runner := Runner{
		Client:   &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}},
		Tools:    tool.List{pausingTool(paused)},
		Recorder: recorder,
	}
	out, err := runner.Run(
		t.Context(),
		Turn{ID: "history-limit"},
		Start{
			CommandID: "start",
			Agent:     def,
			History:   history,
			Input:     []llm.Message{llm.UserMessage(llm.Text("current input"))},
		},
		WithPause(paused),
	)
	if err != nil {
		t.Fatal(err)
	}
	out, err = runner.Run(
		t.Context(),
		out.Turn,
		Continue{CommandID: "resume", History: history},
		WithPause(paused),
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopInterrupted {
		t.Fatalf("resume did not interrupt: %+v", out.Stop)
	}
	view, err := Replay(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if view.Interruptions != 1 || view.ReadyToContinue {
		t.Fatalf("turn is not at its interruption limit: %+v", view)
	}
	return recorder, out.Turn
}

func assertSavedSteps(t *testing.T, recorder *MemoryRecorder, want Turn) {
	t.Helper()
	saved, err := recorder.Load(t.Context(), want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Steps) != len(want.Steps) {
		t.Fatalf("saved steps = %d, want %d", len(saved.Steps), len(want.Steps))
	}
}

func TestRunnerInvalidContinueHistoryLeavesTurnUnchanged(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first := startPausedHistoryTurn(t, client, recorder, nil)
	_, err := (Runner{
		Client:   client,
		Recorder: recorder,
	}).Run(
		t.Context(),
		first.Turn,
		Continue{CommandID: "continue", History: []llm.Message{{Role: llm.RoleUser}}},
	)
	if err == nil {
		t.Fatal("invalid history was accepted")
	}
	saved, err := recorder.Load(t.Context(), first.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Steps) != len(first.Turn.Steps) {
		t.Fatal("invalid history changed the saved turn")
	}
}

func TestRunnerCrashThenContinueUsesSuppliedHistory(t *testing.T) {
	history := []llm.Message{llm.UserMessage(llm.Text("history after reload"))}
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first, err := (Runner{
		Client:   client,
		Recorder: failIntentRecorder{base: recorder},
	}).Run(
		t.Context(),
		Turn{ID: "history-recovery"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			History:   history,
			Input:     []llm.Message{llm.UserMessage(llm.Text("current input"))},
		},
	)
	if persistence(err) != PersistenceRejected {
		t.Fatalf("model intent write was not rejected: %v", err)
	}
	if first.Turn.ID != "history-recovery" {
		t.Fatalf("outcome lost the turn: %+v", first.Turn)
	}
	saved, err := recorder.Load(t.Context(), "history-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Steps) != 1 || saved.Steps[0].Kind != StepStarted {
		t.Fatalf("saved prefix after crash = %#v", saved.Steps)
	}
	out, err := (Runner{
		Client:   client,
		Recorder: recorder,
	}).Run(t.Context(), saved, Continue{CommandID: "resume", History: history})
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("reloaded turn did not complete: %+v", out.Stop)
	}
	if len(client.inputs) != 2 {
		t.Fatalf("prepared requests = %d, want 2", len(client.inputs))
	}
	if !reflect.DeepEqual(client.inputs[1].Messages[0], history[0]) {
		t.Fatal("reloaded turn omitted supplied history")
	}
}

func TestRunnerNoRecorderCanContinueReturnedTurn(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	first := startPausedHistoryTurn(t, client, nil, nil)
	out, err := (Runner{Client: client}).Run(
		t.Context(),
		first.Turn,
		Continue{CommandID: "continue"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("turn did not complete without recorder: %+v", out.Stop)
	}
	if len(out.Turn.Steps) <= len(first.Turn.Steps) {
		t.Fatal("continuation did not append steps")
	}
}

func TestRunnerEmptyHistoryFormsMatchOnContinue(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first := startPausedHistoryTurn(t, client, recorder, nil)
	out, err := (Runner{
		Client:   client,
		Recorder: recorder,
	}).Run(
		t.Context(),
		first.Turn,
		Continue{CommandID: "continue", History: []llm.Message{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("continued turn did not complete: %+v", out.Stop)
	}
	if len(out.Warnings) != 0 {
		t.Fatalf("empty history caused warning: %#v", out.Warnings)
	}
}

func startPausedHistoryTurn(
	t *testing.T,
	client *scriptedClient,
	recorder Recorder,
	history []llm.Message,
) Outcome {
	t.Helper()
	paused := make(chan struct{})
	close(paused)
	out, err := (Runner{
		Client:   client,
		Recorder: recorder,
	}).Run(
		t.Context(),
		Turn{ID: "history-paused"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			History:   history,
			Input:     []llm.Message{llm.UserMessage(llm.Text("current input"))},
		},
		WithPause(paused),
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopInterrupted {
		t.Fatalf("start did not stop before model: %+v", out.Stop)
	}
	return out
}
