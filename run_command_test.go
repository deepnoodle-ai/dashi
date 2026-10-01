package dashi

import (
	"context"
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
	if out.Turn.Steps[0].Started.HistoryHash == "" {
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
	if continued.Kind != StepTurnContinued {
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
		if event.Result != nil {
			events++
		}
	}
	if !errors.Is(got, ErrHistoryMismatch) || events != 0 {
		t.Fatalf("stream error = %v, outcomes = %d", got, events)
	}
}

func TestRunnerStrictContinueRejectsAtResumeLimit(t *testing.T) {
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

	// Warn mode still records the resume-limit stop.
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

func TestRunnerInvalidHistoryAtResumeLimitCommitsNothing(t *testing.T) {
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
// resume, so the next Continue reaches the resume limit.
func interruptionLimitedHistoryTurn(
	t *testing.T,
	history []llm.Message,
) (*MemoryRecorder, Turn) {
	t.Helper()
	def := testAgent()
	def.Limits.Resumes = 1
	recorder := NewMemoryRecorder()
	// The first pause follows a model call, so resuming from it counts.
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
	view, err := out.Turn.View()
	if err != nil {
		t.Fatal(err)
	}
	if view.Resumes != 1 || view.ReadyToContinue {
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
		Recorder: failCallStartRecorder{base: recorder},
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
	if !nothingWritten(err) {
		t.Fatalf("model intent write was not rejected: %v", err)
	}
	if first.Turn.ID != "history-recovery" {
		t.Fatalf("outcome lost the turn: %+v", first.Turn)
	}
	saved, err := recorder.Load(t.Context(), "history-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Steps) != 1 || saved.Steps[0].Kind != StepTurnStarted {
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
) RunResult {
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

func TestDuplicateCommandLoadsCurrentTurn(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{callResult("call-1", "lookup"), final("found")},
	}
	rec := NewMemoryRecorder()
	external := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindExternal,
		},
	}
	first, err := start(t, client, tool.List{external}, rec, Hooks{})
	if err != nil || first.Stop.Kind != StopWaiting {
		t.Fatalf("start: stop=%+v err=%v", first.Stop, err)
	}
	result := tool.Outcome{
		State:  tool.StateSucceeded,
		Output: new(tool.TextOutput("result")),
	}
	accepted, err := (Runner{
		Client:   client,
		Tools:    tool.List{external},
		Recorder: rec,
	}).Run(
		t.Context(),
		first.Turn,
		Resolve{CommandID: "result", CallID: "call-1", Outcome: &result},
	)
	if err != nil || !accepted.ReadyToContinue || client.sends != 1 {
		t.Fatalf("settle: out=%+v err=%v sends=%d", accepted, err, client.sends)
	}
	completed, err := (Runner{
		Client:   client,
		Tools:    tool.List{external},
		Recorder: rec,
	}).Run(t.Context(), accepted.Turn, Continue{CommandID: "continue"})
	if err != nil || completed.Stop.Kind != StopCompleted {
		t.Fatalf("settle: stop=%+v err=%v", completed.Stop, err)
	}
	duplicate, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		first.Turn,
		Start{CommandID: "start", Agent: testAgent(), Input: []llm.Message{llm.UserMessage(llm.Text("do it"))}},
	)
	if err != nil || duplicate.Stop.Kind != StopCompleted || len(duplicate.Turn.Steps) != len(completed.Turn.Steps) || client.sends != 2 {
		t.Fatalf(
			"duplicate: stop=%+v steps=%d err=%v sends=%d",
			duplicate.Stop,
			len(duplicate.Turn.Steps),
			err,
			client.sends,
		)
	}
}

// noModelClient fails the test if a run prepares or sends a model request.
type noModelClient struct{ t *testing.T }

func (noModelClient) Provider() string { return "test" }

func (noModelClient) API() string { return "test.chat" }

func (noModelClient) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}

func (c noModelClient) Prepare(context.Context, llm.Input) (llm.Request, error) {
	c.t.Helper()
	c.t.Fatal("unexpected Prepare")
	return llm.Request{}, nil
}

func (c noModelClient) Send(context.Context, llm.Call) (llm.Result, error) {
	c.t.Helper()
	c.t.Fatal("unexpected Send")
	return llm.Result{}, nil
}

func TestRecordWithoutClient(t *testing.T) {
	rec := NewMemoryRecorder()
	paused := make(chan struct{})
	close(paused)
	admit := Start{
		CommandID: "start",
		Agent:     testAgent(),
		Input:     []llm.Message{llm.UserMessage(llm.Text("do it"))},
	}
	admitted, err := (Runner{Recorder: rec}).Run(
		t.Context(), Turn{ID: "t1"}, admit, WithPause(paused),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(admitted.Turn.Steps) != 2 ||
		admitted.Stop.Kind != StopInterrupted ||
		admitted.Stop.Reason != "pause requested" ||
		admitted.Next() != NextContinue {
		t.Fatalf("admitted=%+v steps=%d", admitted.Stop, len(admitted.Turn.Steps))
	}

	// The same unrun Start with a Client records the same Start step.
	withClient, err := (Runner{Client: &scriptedClient{}}).Run(
		t.Context(), Turn{ID: "t1"}, admit, WithPause(paused),
	)
	if err != nil {
		t.Fatal(err)
	}
	got, want := admitted.Turn.Steps[0], withClient.Turn.Steps[0]
	if got.CommandHash != want.CommandHash ||
		!reflect.DeepEqual(got.Started, want.Started) {
		t.Fatalf("start step differs without a client:\n%+v\n%+v", got, want)
	}

	// A retried Start with a Client is the same command.
	client := &scriptedClient{results: []llm.Result{final("done")}}
	runner := Runner{Client: client, Recorder: rec}
	retried, err := runner.Run(t.Context(), Turn{ID: "t1"}, admit)
	if err != nil {
		t.Fatal(err)
	}
	if len(retried.Turn.Steps) != 2 || client.sends != 0 {
		t.Fatalf("retry ran: steps=%d sends=%d", len(retried.Turn.Steps), client.sends)
	}

	// Cancel needs no Client.
	canceled, err := (Runner{Recorder: rec}).Run(
		t.Context(), retried.Turn, Cancel{CommandID: "cancel", Reason: "operator"},
	)
	if err != nil || canceled.Stop.Kind != StopCanceled {
		t.Fatalf("cancel=%+v err=%v", canceled.Stop, err)
	}

	// Continue with a Client runs a turn that was recorded without running.
	rec2 := NewMemoryRecorder()
	again, err := (Runner{Recorder: rec2}).Run(
		t.Context(), Turn{ID: "t2"}, admit, WithPause(paused),
	)
	if err != nil {
		t.Fatal(err)
	}
	client = &scriptedClient{results: []llm.Result{final("done")}}
	done, err := (Runner{Client: client, Recorder: rec2}).Run(
		t.Context(), again.Turn, Continue{CommandID: "continue"},
	)
	if err != nil || done.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("done=%+v sends=%d err=%v", done.Stop, client.sends, err)
	}
}

func TestStartWithoutClientNeedsAFullAgent(t *testing.T) {
	paused := make(chan struct{})
	close(paused)
	for _, clear := range []func(*Agent){
		func(a *Agent) { a.Provider = "" },
		func(a *Agent) { a.API = "" },
		func(a *Agent) { a.Model = "" },
	} {
		rec := NewMemoryRecorder()
		agent := testAgent()
		clear(&agent)
		_, err := (Runner{Recorder: rec}).Run(t.Context(), Turn{ID: "t1"}, Start{
			Agent: agent,
			Input: []llm.Message{llm.UserMessage(llm.Text("hi"))},
		}, WithPause(paused))
		if err == nil || !strings.Contains(err.Error(), "client is required to fill") {
			t.Fatalf("err=%v", err)
		}
		if _, err := rec.Load(t.Context(), "t1"); !errors.Is(err, ErrTurnNotFound) {
			t.Fatalf("turn was recorded: %v", err)
		}
	}
}

func TestStartWithoutClientAndActiveCompactionFails(t *testing.T) {
	paused := make(chan struct{})
	close(paused)
	rec := NewMemoryRecorder()
	agent := testAgent()
	agent.ActiveCompaction = &ActiveCompactionPolicy{}
	_, err := (Runner{Recorder: rec}).Run(t.Context(), Turn{ID: "t1"}, Start{
		Agent: agent,
		Input: []llm.Message{llm.UserMessage(llm.Text("hi"))},
	}, WithPause(paused))
	if err == nil || !strings.Contains(err.Error(), "context window") {
		t.Fatalf("err=%v", err)
	}
	if _, err := rec.Load(t.Context(), "t1"); !errors.Is(err, ErrTurnNotFound) {
		t.Fatalf("turn was recorded: %v", err)
	}
}

func TestRunWithoutClientCannotCallTheModel(t *testing.T) {
	rec := NewMemoryRecorder()
	runner := Runner{Recorder: rec}
	_, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		Agent: testAgent(),
		Input: []llm.Message{llm.UserMessage(llm.Text("hi"))},
	}, WithPause(make(chan struct{})))
	if err == nil || !strings.Contains(err.Error(), "client is required to call the model") {
		t.Fatalf("start err=%v", err)
	}
	if _, err := rec.Load(t.Context(), "t1"); !errors.Is(err, ErrTurnNotFound) {
		t.Fatalf("start was recorded: %v", err)
	}

	paused := make(chan struct{})
	close(paused)
	admitted, err := runner.Run(t.Context(), Turn{ID: "t1"}, Start{
		Agent: testAgent(),
		Input: []llm.Message{llm.UserMessage(llm.Text("hi"))},
	}, WithPause(paused))
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(t.Context(), admitted.Turn, Continue{})
	if err == nil || !strings.Contains(err.Error(), "client is required to call the model") {
		t.Fatalf("continue err=%v", err)
	}
	saved, err := rec.Load(t.Context(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Steps) != len(admitted.Turn.Steps) {
		t.Fatalf("continue recorded steps: %d, want %d", len(saved.Steps), len(admitted.Turn.Steps))
	}
}

func TestCancelWithoutClient(t *testing.T) {
	rec := NewMemoryRecorder()
	external := tool.Tool{Definition: tool.Definition{
		Name:   "lookup",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}}
	waiting, err := start(t, client, tool.List{external}, rec, Hooks{})
	if err != nil || waiting.Stop.Kind != StopWaiting {
		t.Fatalf("waiting=%+v err=%v", waiting.Stop, err)
	}
	out, err := (Runner{Recorder: rec}).Run(
		t.Context(),
		waiting.Turn,
		Cancel{CommandID: "cancel", Reason: "operator"},
	)
	if err != nil || out.Stop.Kind != StopCanceled || out.Next() != NextNone {
		t.Fatalf("out=%+v err=%v", out.Stop, err)
	}
}

func TestCancelRefusesTurnThatHasNotStarted(t *testing.T) {
	_, err := (Runner{Recorder: NewMemoryRecorder()}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Cancel{CommandID: "cancel"},
	)
	if err == nil || !strings.Contains(err.Error(), "turn has not started") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveAndAddMessagesWithoutClient(t *testing.T) {
	external := tool.Tool{Definition: tool.Definition{
		Name:   "lookup",
		Schema: []byte(`{"type":"object"}`),
		Kind:   tool.KindExternal,
	}}
	clientless := func(rec Recorder) Runner {
		return Runner{Recorder: rec, Tools: tool.List{external}}
	}

	t.Run("tool outcome and deliver", func(t *testing.T) {
		rec := NewMemoryRecorder()
		client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}}
		waiting, err := start(t, client, tool.List{external}, rec, Hooks{})
		if err != nil {
			t.Fatal(err)
		}
		delivered, err := clientless(rec).Run(t.Context(), waiting.Turn, AddMessages{
			CommandID: "note",
			Items: []ContextItem{{
				ID:      "note-1",
				Message: llm.UserMessage(llm.Text("also check the cache")),
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		result := tool.Succeeded("found")
		finished, err := clientless(rec).Run(
			t.Context(),
			delivered.Turn,
			Resolve{CommandID: "result", CallID: "call-1", Outcome: &result},
		)
		if err != nil || !finished.ReadyToContinue || client.sends != 1 {
			t.Fatalf("settled=%+v err=%v sends=%d", finished.Stop, err, client.sends)
		}
	})

	t.Run("approval", func(t *testing.T) {
		rec := NewMemoryRecorder()
		local := tool.Tool{
			Definition: tool.Definition{
				Name:   "lookup",
				Schema: []byte(`{"type":"object"}`),
			},
			Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
				return tool.Succeeded("ok"), nil
			}),
		}
		client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup")}}
		waiting, err := start(t, client, tool.List{local}, rec, RequireApproval())
		if err != nil || waiting.Next() != NextResolvePending {
			t.Fatalf("waiting=%+v err=%v", waiting.Stop, err)
		}
		approved, err := (Runner{Recorder: rec}).Run(
			t.Context(),
			waiting.Turn,
			waiting.Pending[0].Deny("not now"),
		)
		if err != nil || client.sends != 1 {
			t.Fatalf("approved=%+v err=%v", approved.Stop, err)
		}
	})

	for _, tc := range []struct {
		name   string
		finish Resolve
	}{
		{"model result", Resolve{ModelResult: new(final("recovered"))}},
		{"model failure", Resolve{ModelFailure: &ModelFailure{
			SendStatus: llm.SendStatusNotSent,
			Reason:     "worker did not send",
			Evidence:   "dispatch log",
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := NewMemoryRecorder()
			turn := crashedModelCallStart(t, rec)
			tc.finish.CommandID = "reconcile"
			tc.finish.CallID = "t1:model:1"
			out, err := (Runner{Recorder: rec}).Run(t.Context(), turn, tc.finish)
			if err != nil || !out.ReadyToContinue {
				t.Fatalf("out=%+v err=%v", out.Stop, err)
			}
		})
	}
}

// crashedModelCallStart records a turn whose model call has a
// model_call_started step and a result_unknown stop, so a Resolve must
// supply its result.
func crashedModelCallStart(t *testing.T, rec *MemoryRecorder) Turn {
	t.Helper()
	def := testAgent()
	turn := Turn{ID: "t1"}
	for _, step := range []Step{
		{Kind: StepTurnStarted, Started: &StartFact{
			Agent: def,
			Input: []llm.Message{llm.UserMessage(llm.Text("hello"))},
		}},
		{Kind: StepModelCallStarted, Model: &ModelFact{
			ModelCallID: "t1:model:1",
			MessageID:   "t1:model:1:message",
			RequestHash: "digest",
			Provider:    def.Provider,
			API:         def.API,
			Model:       def.Model,
		}},
		{Kind: StepTurnStopped, Stop: &Stop{Kind: StopResultUnknown, Reason: "worker crashed"}},
	} {
		step.Version = 1
		step.TurnID = turn.ID
		step.Seq = int64(len(turn.Steps) + 1)
		if err := rec.Commit(
			t.Context(),
			CommitRequest{Step: step, ExpectedSeq: step.Seq - 1},
		); err != nil {
			t.Fatal(err)
		}
		turn.Steps = append(turn.Steps, step)
	}
	return turn
}

func TestRecordWithoutRunning(t *testing.T) {
	rec := NewMemoryRecorder()
	paused := make(chan struct{})
	close(paused)
	admitted, err := (Runner{Client: noModelClient{t}, Recorder: rec}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("hello"))},
		},
		WithPause(paused),
	)
	if err != nil {
		t.Fatal(err)
	}
	steps := admitted.Turn.Steps
	if len(steps) != 2 ||
		steps[0].Kind != StepTurnStarted ||
		steps[1].Kind != StepTurnStopped ||
		steps[1].Stop.Kind != StopInterrupted ||
		steps[1].Stop.Reason != "pause requested" {
		t.Fatalf("steps=%+v", steps)
	}
	if admitted.Next() != NextContinue || !admitted.ReadyToContinue ||
		admitted.ModelCalls != 0 || admitted.Resumes != 0 {
		t.Fatalf("view=%+v next=%v", admitted.TurnView, admitted.Next())
	}

	canceled, err := (Runner{Recorder: rec}).Run(
		t.Context(),
		admitted.Turn,
		Cancel{CommandID: "cancel", Reason: "admission withdrawn"},
	)
	if err != nil || canceled.Stop.Kind != StopCanceled {
		t.Fatalf("canceled=%+v err=%v", canceled.Stop, err)
	}

	// The same unrun Start, continued later, runs normally.
	rec = NewMemoryRecorder()
	admitted, err = (Runner{Client: noModelClient{t}, Recorder: rec}).Run(
		t.Context(),
		Turn{ID: "t2"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("hello"))},
		},
		WithPause(paused),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	done, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		admitted.Turn,
		Continue{CommandID: "run"},
	)
	if err != nil || done.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf("done=%+v err=%v sends=%d", done.Stop, err, client.sends)
	}
	// A pause before the first model call interrupted no work.
	if done.Resumes != 0 {
		t.Fatalf("interruptions=%d", done.Resumes)
	}
}

func TestRecordedTurnResumesWithNoResumesAllowed(t *testing.T) {
	rec := NewMemoryRecorder()
	paused := make(chan struct{})
	close(paused)
	agent := testAgent()
	agent.Limits.Resumes = -1
	admitted, err := (Runner{Client: noModelClient{t}, Recorder: rec}).Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     agent,
			Input:     []llm.Message{llm.UserMessage(llm.Text("hello"))},
		},
		WithPause(paused),
	)
	if err != nil || admitted.Next() != NextContinue {
		t.Fatalf("admitted=%+v err=%v", admitted.Stop, err)
	}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	done, err := (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		admitted.Turn,
		Continue{CommandID: "run"},
	)
	if err != nil || done.Stop.Kind != StopCompleted || done.Resumes != 0 {
		t.Fatalf("done=%+v interruptions=%d err=%v", done.Stop, done.Resumes, err)
	}
}

// pausingTool is a local "lookup" tool that closes paused when it runs, so
// a turn pauses after a model call, before its next model call.
func pausingTool(paused chan struct{}) tool.Tool {
	return tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			close(paused)
			return tool.Succeeded("ok"), nil
		}),
	}
}

func TestPauseAfterModelCallCountsAsResume(t *testing.T) {
	rec := NewMemoryRecorder()
	paused := make(chan struct{})
	local := pausingTool(paused)
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "lookup"),
		final("done"),
	}}
	runner := Runner{Client: client, Tools: tool.List{local}, Recorder: rec}
	stopped, err := runner.Run(
		t.Context(),
		Turn{ID: "t1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input:     []llm.Message{llm.UserMessage(llm.Text("hello"))},
		},
		WithPause(paused),
	)
	if err != nil || stopped.Stop.Kind != StopInterrupted || client.sends != 1 {
		t.Fatalf("stopped=%+v err=%v sends=%d", stopped.Stop, err, client.sends)
	}
	done, err := runner.Run(t.Context(), stopped.Turn, Continue{CommandID: "run"})
	if err != nil || done.Stop.Kind != StopCompleted || done.Resumes != 1 {
		t.Fatalf("done=%+v interruptions=%d err=%v", done.Stop, done.Resumes, err)
	}
}
