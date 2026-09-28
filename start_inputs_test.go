package kernel

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

func TestStartRecordsScheduledOriginWithoutPriorHistory(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("first")}}
	rec := NewMemoryRecorder()
	history := []llm.Message{
		llm.UserMessage(llm.Text("earlier request")),
		llm.AssistantMessage(llm.Text("earlier answer")),
	}
	start := Start{
		ID:      "start",
		Origin:  StartScheduled,
		History: history,
		Input:   []llm.Message{llm.UserMessage(llm.Text("scheduled report"))},
	}
	first, err := (Engine{}).Apply(
		t.Context(),
		Turn{ID: "t1"},
		definition(),
		start,
		Bindings{Client: client, Recorder: rec},
	)
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
		ID:      "start",
		Origin:  StartScheduled,
		History: history,
		Input:   start.Input,
	}
	_, err = (Engine{}).Apply(
		t.Context(),
		first.Turn,
		definition(),
		again,
		Bindings{Client: client, Recorder: rec},
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
		out, err := (Engine{}).Apply(
			t.Context(),
			Turn{ID: "t1"},
			definition(),
			Start{ID: "start", Origin: tc.origin, Input: []llm.Message{message}},
			Bindings{Client: client, Recorder: NewMemoryRecorder()},
		)
		if tc.valid && (err != nil || out.Stop.Kind != StopCompleted) {
			t.Fatalf("origin %q rejected: out=%+v err=%v", tc.origin, out, err)
		}
		if !tc.valid && (err == nil && out.Persistence == PersistenceAcknowledged) {
			t.Fatalf("origin %q accepted trusted role", tc.origin)
		}
	}
}
