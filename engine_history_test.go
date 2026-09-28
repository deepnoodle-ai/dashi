package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

func TestEngineStartUsesHistoryWithoutRecordingIt(t *testing.T) {
	history := []llm.Message{
		llm.UserMessage(llm.Text("earlier conversation")),
	}
	input := llm.UserMessage(llm.Text("current input"))
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	out, err := (Engine{}).Apply(
		context.Background(),
		Turn{ID: "history-start"},
		definition(),
		Start{
			ID:      "start",
			History: history,
			Input:   []llm.Message{input},
		},
		Bindings{Client: client, Recorder: NewMemoryRecorder()},
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

func TestEngineContinueUsesHistoryWithoutRecordingIt(t *testing.T) {
	history := []llm.Message{
		llm.UserMessage(llm.Text("prior exchange")),
	}
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first := startPausedHistoryTurn(t, client, recorder, history)
	out, err := (Engine{}).Apply(
		context.Background(),
		first.Turn,
		definition(),
		Continue{ID: "continue", History: history},
		Bindings{Client: client, Recorder: recorder},
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
	want := []llm.Message{
		history[0],
		llm.UserMessage(llm.Text("current input")),
	}
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

func TestEngineContinueWarnsOnDifferentHistoryAndProceeds(t *testing.T) {
	history := []llm.Message{
		llm.UserMessage(llm.Text("original history")),
	}
	changed := []llm.Message{
		llm.UserMessage(llm.Text("different history")),
	}
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first := startPausedHistoryTurn(t, client, recorder, history)
	out, err := (Engine{}).Apply(
		context.Background(),
		first.Turn,
		definition(),
		Continue{ID: "continue", History: changed},
		Bindings{Client: client, Recorder: recorder},
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

func TestEngineStrictContinueRejectsDifferentHistory(t *testing.T) {
	history := []llm.Message{
		llm.UserMessage(llm.Text("original history")),
	}
	changed := []llm.Message{
		llm.UserMessage(llm.Text("compacted history")),
	}
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first := startPausedHistoryTurn(t, client, recorder, history)
	strict := Bindings{
		Client:            client,
		Recorder:          recorder,
		OnHistoryMismatch: HistoryMismatchReject,
	}
	_, err := (Engine{}).Apply(
		context.Background(),
		first.Turn,
		definition(),
		Continue{ID: "continue", History: changed},
		strict,
	)
	if !errors.Is(err, ErrHistoryMismatch) {
		t.Fatalf("error = %v, want ErrHistoryMismatch", err)
	}
	if len(client.inputs) != 0 {
		t.Fatal("rejected continue called the model")
	}
	saved, err := recorder.Load(first.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Steps) != len(first.Turn.Steps) {
		t.Fatal("rejected continue changed the saved turn")
	}

	// The same command ID with the original history resumes the turn.
	out, err := (Engine{}).Apply(
		context.Background(),
		saved,
		definition(),
		Continue{ID: "continue", History: history},
		strict,
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

func TestEngineStrictContinueRejectsInStreamErrorSlot(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first := startPausedHistoryTurn(t, client, recorder, nil)
	changed := []llm.Message{llm.UserMessage(llm.Text("new session input"))}
	var events int
	var got error
	for event, err := range (Engine{}).Stream(
		context.Background(),
		first.Turn,
		definition(),
		Continue{ID: "continue", History: changed},
		Bindings{
			Client:            client,
			Recorder:          recorder,
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

func TestEngineStrictContinueRejectsAtInterruptionLimit(t *testing.T) {
	history := []llm.Message{llm.UserMessage(llm.Text("original history"))}
	changed := []llm.Message{llm.UserMessage(llm.Text("changed history"))}
	def, recorder, limited := interruptionLimitedHistoryTurn(t, history)
	_, err := (Engine{}).Apply(
		context.Background(),
		limited,
		def,
		Continue{ID: "at-limit", History: changed},
		Bindings{
			Client:            &scriptedClient{},
			Recorder:          recorder,
			OnHistoryMismatch: HistoryMismatchReject,
		},
	)
	if !errors.Is(err, ErrHistoryMismatch) {
		t.Fatalf("error = %v, want ErrHistoryMismatch", err)
	}
	assertSavedSteps(t, recorder, limited)

	// Warn mode still records the interruption-limit stop.
	out, err := (Engine{}).Apply(
		context.Background(),
		limited,
		def,
		Continue{ID: "at-limit", History: changed},
		Bindings{Client: &scriptedClient{}, Recorder: recorder},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopLimited {
		t.Fatalf("warn mode stop = %+v, want limited", out.Stop)
	}
}

func TestEngineInvalidHistoryAtInterruptionLimitCommitsNothing(t *testing.T) {
	def, recorder, limited := interruptionLimitedHistoryTurn(t, nil)
	_, err := (Engine{}).Apply(
		context.Background(),
		limited,
		def,
		Continue{
			ID:      "at-limit",
			History: []llm.Message{{Role: llm.RoleUser}},
		},
		Bindings{Client: &scriptedClient{}, Recorder: recorder},
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
) (Definition, *MemoryRecorder, Turn) {
	t.Helper()
	def := definition()
	def.Limits.Interruptions = 1
	recorder := NewMemoryRecorder()
	paused := make(chan struct{})
	close(paused)
	b := Bindings{Client: &scriptedClient{}, Recorder: recorder, Pause: paused}
	out, err := (Engine{}).Apply(
		context.Background(),
		Turn{ID: "history-limit"},
		def,
		Start{
			ID:      "start",
			History: history,
			Input:   []llm.Message{llm.UserMessage(llm.Text("current input"))},
		},
		b,
	)
	if err != nil {
		t.Fatal(err)
	}
	out, err = (Engine{}).Apply(
		context.Background(),
		out.Turn,
		def,
		Continue{ID: "resume", History: history},
		b,
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
	return def, recorder, out.Turn
}

func assertSavedSteps(t *testing.T, recorder *MemoryRecorder, want Turn) {
	t.Helper()
	saved, err := recorder.Load(want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Steps) != len(want.Steps) {
		t.Fatalf("saved steps = %d, want %d", len(saved.Steps), len(want.Steps))
	}
}

func TestEngineStrictContinueWithoutFingerprintProceeds(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	first := startPausedHistoryTurn(t, client, nil, nil)
	// A record written before fingerprints has nothing to compare.
	legacy := cloneTurn(first.Turn)
	legacy.Steps[0].Started.HistoryFingerprint = ""
	out, err := (Engine{}).Apply(
		context.Background(),
		legacy,
		definition(),
		Continue{
			ID:      "continue",
			History: []llm.Message{llm.UserMessage(llm.Text("history"))},
		},
		Bindings{
			Client:            client,
			OnHistoryMismatch: HistoryMismatchReject,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted || len(out.Warnings) != 0 {
		t.Fatalf("legacy turn stop = %+v, warnings = %#v", out.Stop, out.Warnings)
	}
}

func TestEngineInvalidContinueHistoryLeavesTurnUnchanged(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first := startPausedHistoryTurn(t, client, recorder, nil)
	_, err := (Engine{}).Apply(
		context.Background(),
		first.Turn,
		definition(),
		Continue{
			ID:      "continue",
			History: []llm.Message{{Role: llm.RoleUser}},
		},
		Bindings{Client: client, Recorder: recorder},
	)
	if err == nil {
		t.Fatal("invalid history was accepted")
	}
	saved, err := recorder.Load(context.Background(), first.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Steps) != len(first.Turn.Steps) {
		t.Fatal("invalid history changed the saved turn")
	}
}

func TestEngineCrashThenContinueUsesSuppliedHistory(t *testing.T) {
	history := []llm.Message{
		llm.UserMessage(llm.Text("history after reload")),
	}
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first, err := (Engine{}).Apply(
		context.Background(),
		Turn{ID: "history-recovery"},
		definition(),
		Start{
			ID:      "start",
			History: history,
			Input: []llm.Message{
				llm.UserMessage(llm.Text("current input")),
			},
		},
		Bindings{
			Client:   client,
			Recorder: failIntentRecorder{base: recorder},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Persistence != PersistenceRejected {
		t.Fatalf("model intent write was not rejected: %s", first.Persistence)
	}
	saved, err := recorder.Load(context.Background(), "history-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Steps) != 1 || saved.Steps[0].Kind != StepStarted {
		t.Fatalf("saved prefix after crash = %#v", saved.Steps)
	}
	out, err := (Engine{}).Apply(
		context.Background(),
		saved,
		definition(),
		Continue{ID: "resume", History: history},
		Bindings{Client: client, Recorder: recorder},
	)
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

func TestEngineNoRecorderCanContinueReturnedTurn(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	first := startPausedHistoryTurn(t, client, nil, nil)
	out, err := (Engine{}).Apply(
		context.Background(),
		first.Turn,
		definition(),
		Continue{ID: "continue"},
		Bindings{Client: client},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopCompleted {
		t.Fatalf("turn did not complete without recorder: %+v", out.Stop)
	}
	if out.Persistence != PersistenceAcknowledged {
		t.Fatalf("turn persistence = %s", out.Persistence)
	}
	if len(out.Turn.Steps) <= len(first.Turn.Steps) {
		t.Fatal("continuation did not append steps")
	}
}

func TestEngineEmptyHistoryFormsMatchOnContinue(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("answer")}}
	recorder := NewMemoryRecorder()
	first := startPausedHistoryTurn(t, client, recorder, nil)
	out, err := (Engine{}).Apply(
		context.Background(),
		first.Turn,
		definition(),
		Continue{ID: "continue", History: []llm.Message{}},
		Bindings{Client: client, Recorder: recorder},
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
	out, err := (Engine{}).Apply(
		context.Background(),
		Turn{ID: "history-paused"},
		definition(),
		Start{
			ID:      "start",
			History: history,
			Input: []llm.Message{
				llm.UserMessage(llm.Text("current input")),
			},
		},
		Bindings{Client: client, Recorder: recorder, Pause: paused},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopInterrupted {
		t.Fatalf("start did not stop before model: %+v", out.Stop)
	}
	return out
}
