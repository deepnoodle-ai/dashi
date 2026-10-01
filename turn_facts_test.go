package dashi

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestStartRecordsScheduledOriginWithoutPriorHistory(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("first")}}
	rec := NewMemoryRecorder()
	history := []llm.Message{
		llm.UserMessage(llm.Text("earlier request")),
		llm.AssistantMessage(llm.Text("earlier answer")),
	}
	start := Start{
		CommandID: "start",
		Agent:     testAgent(),
		Origin:    StartScheduled,
		History:   history,
		Input:     []llm.Message{llm.UserMessage(llm.Text("scheduled report"))},
	}
	first, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(t.Context(), Turn{ID: "t1"}, start)
	if err != nil || first.Stop.Kind != StopCompleted {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	started := first.Turn.Steps[0].Started
	if started.Origin != StartScheduled || started.HistoryFingerprint == "" {
		t.Fatalf("start fact=%+v", first.Turn.Steps[0].Started)
	}
	encoded, err := json.Marshal(first.Turn.Steps[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"history":`)) {
		t.Fatalf("start step saved prior history: %s", encoded)
	}
	history[0] = llm.UserMessage(llm.Text("changed outside turn"))
	projected, err := Project(first.Turn)
	if err != nil || len(projected) != 2 ||
		projected[0].Text() != "scheduled report" {
		t.Fatalf("projected=%+v err=%v", projected, err)
	}
	again := Start{
		CommandID: "start",
		Agent:     testAgent(),
		Origin:    StartScheduled,
		History:   history,
		Input:     start.Input,
	}
	_, err = (Runner{Client: client, Recorder: rec}).Run(
		t.Context(),
		first.Turn,
		again,
	)
	if err != ErrCommandConflict {
		t.Fatalf("changed accepted history did not conflict: %v", err)
	}
}

func TestOperatorStartCanCarryTrustedRole(t *testing.T) {
	message := llm.UserMessage(llm.Text("run maintenance"))
	message.Role = llm.RoleDeveloper
	for _, tc := range []struct {
		origin StartOrigin
		valid  bool
	}{
		{StartOperator, true},
		{StartScheduled, false},
		{StartUser, false},
	} {
		client := &scriptedClient{results: []llm.Result{final("done")}}
		out, err := (Runner{
			Client:   client,
			Recorder: NewMemoryRecorder(),
		}).Run(
			t.Context(),
			Turn{ID: "t1"},
			Start{CommandID: "start", Agent: testAgent(), Origin: tc.origin, Input: []llm.Message{message}},
		)
		if tc.valid && (err != nil || out.Stop.Kind != StopCompleted) {
			t.Fatalf("origin %q rejected: out=%+v err=%v", tc.origin, out, err)
		}
		if !tc.valid && err == nil {
			t.Fatalf("origin %q accepted trusted role", tc.origin)
		}
	}
}

func TestModelIntentRecordsResolvedCapabilityPolicy(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := start(t, client, nil, NewMemoryRecorder(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	var intent *ModelFact
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelCallStarted {
			intent = step.Model
		}
	}
	if intent == nil {
		t.Fatal("turn recorded no model intent")
	}
	if intent.CapabilityPolicy != llm.CapabilityAdjust {
		t.Fatalf("intent recorded policy %q", intent.CapabilityPolicy)
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"capability_policy":"adjust"`)) {
		t.Fatalf("intent omitted the resolved policy: %s", raw)
	}
}

func TestReplayRejectsUnknownCapabilityPolicy(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := start(t, client, nil, NewMemoryRecorder(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	turn := cloneTurn(out.Turn)
	for _, step := range turn.Steps {
		if step.Kind == StepModelCallStarted {
			step.Model.CapabilityPolicy = "lenient"
		}
	}
	if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("replayed an unknown capability policy: %v", err)
	}
}
