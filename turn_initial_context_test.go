package dashi

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func initialContext() []ContextItem {
	return []ContextItem{
		{
			ID: "checkout", Name: "checkout", Origin: ContextRuntime,
			Snapshot: true, Message: llm.UserMessage(llm.Text("use /workspace")),
		},
		{
			ID: "policy", Name: "policy", Origin: ContextOperator,
			Message: llm.Message{Role: llm.RoleDeveloper, Content: []llm.Content{
				llm.Text("operator instruction"),
			}},
		},
		{
			ID: "once", Origin: ContextUser, NextCallOnly: true,
			Message: llm.UserMessage(llm.Text("first call only")),
		},
	}
}

func TestRunnerInitialContextSurvivesReloadAndExpiresAtDispatch(t *testing.T) {
	start := Say("work")
	start.CommandID, start.Agent, start.Context = "start", testAgent(), initialContext()
	client := &scriptedClient{results: []llm.Result{
		{
			Message: llm.AssistantMessage(llm.Text("partial answer")),
			Stop:    llm.Stop{Kind: llm.StopOutputLimit},
		},
		final("done"),
	}}
	out, err := (Runner{Client: client}).Run(t.Context(), Turn{ID: "context"}, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.inputs[0].Messages) != 4 {
		t.Fatalf("initial input/items not projected once: %+v", client.inputs[0].Messages)
	}
	if client.inputs[0].Messages[2].Role != llm.RoleDeveloper {
		t.Fatal("operator authority was changed")
	}
	if len(out.Input) != 2 {
		t.Fatal("initial user context absent from TurnView.Input")
	}
	if _, ok := out.Context(ContextRuntime, "checkout"); !ok {
		t.Fatal("initial snapshot absent from view")
	}
	raw, err := json.Marshal(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Turn
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	out, err = (Runner{Client: client}).Run(
		t.Context(), loaded, Continue{CommandID: "continue"},
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("reload did not continue: %v", err)
	}
	var persistent, temporary int
	for _, message := range client.inputs[1].Messages {
		if strings.Contains(message.Text(), "use /workspace") {
			persistent++
			if message.Context == nil || message.Context.Origin != "runtime" {
				t.Fatal("initial context lost projected identity")
			}
		}
		if strings.Contains(message.Text(), "first call only") {
			temporary++
		}
	}
	if persistent != 1 || temporary != 0 {
		t.Fatalf("context lifetime wrong: persistent=%d temporary=%d", persistent, temporary)
	}
	retry, err := (Runner{Client: admitUntouchedClient{t: t}}).Run(
		t.Context(), out.Turn, start,
	)
	if err != nil || len(retry.Turn.Steps) != len(out.Turn.Steps) {
		t.Fatalf("Start retry added context: %v", err)
	}
}

func TestRunnerInitialContextIDsJoinTurnWideDeduplication(t *testing.T) {
	start := Say("work")
	start.CommandID, start.Agent, start.Context = "start", testAgent(), initialContext()
	out, err := (Runner{}).Admit(t.Context(), Turn{ID: "context"}, start)
	if err != nil {
		t.Fatal(err)
	}
	before := len(out.Turn.Steps)
	duplicate, err := (Runner{}).Run(t.Context(), out.Turn, AddMessages{
		CommandID: "duplicate", Items: []ContextItem{start.Context[0]},
	})
	if err != nil || len(duplicate.Turn.Steps) != before {
		t.Fatalf("initial ID was added again: %v", err)
	}
	changed := start.Context[0]
	changed.NextCallOnly = true
	if _, err := (Runner{}).Run(t.Context(), out.Turn, AddMessages{
		CommandID: "changed", Items: []ContextItem{changed},
	}); err == nil {
		t.Fatal("initial ID accepted changed lifetime")
	}
	start.Context[0].NextCallOnly = true
	if _, err := (Runner{}).Admit(t.Context(), out.Turn, start); !errors.Is(err, ErrCommandIDReused) {
		t.Fatalf("initial lifetime excluded from Start hash: %v", err)
	}
}

func TestRunnerInitialContextRejectsInvalidBatchBeforeRecording(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []ContextItem
	}{
		{name: "repeated ID", items: []ContextItem{initialContext()[0], initialContext()[0]}},
		{name: "untrusted role", items: []ContextItem{{
			Origin: ContextRuntime,
			Message: llm.Message{Role: llm.RoleDeveloper, Content: []llm.Content{
				llm.Text("cannot elevate"),
			}},
		}}},
		{name: "unnamed snapshot", items: []ContextItem{{
			Snapshot: true, Message: llm.UserMessage(llm.Text("value")),
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := Say("work")
			start.Agent, start.Context = testAgent(), tc.items
			out, err := (Runner{}).Admit(t.Context(), Turn{ID: "context"}, start)
			if err == nil || Recorded(err) || len(out.Turn.Steps) != 0 {
				t.Fatalf("invalid initial batch recorded: %v", err)
			}
		})
	}
}

func TestReplayInitialContextVersionGating(t *testing.T) {
	start := Say("work")
	start.Agent, start.Context = testAgent(), initialContext()
	out, err := (Runner{}).Admit(t.Context(), Turn{ID: "context"}, start)
	if err != nil || out.Turn.Steps[0].Version != 2 {
		t.Fatalf("initial context was not versioned: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Step)
	}{
		{name: "downgraded", change: func(s *Step) { s.Version = 1 }},
		{name: "stripped", change: func(s *Step) { s.Started.Context = nil }},
		{name: "empty", change: func(s *Step) { s.Started.Context = []ContextItem{} }},
		{name: "unknown", change: func(s *Step) { s.Version = 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := out.Turn.Clone()
			tc.change(&changed.Steps[0])
			_, err := (Runner{Client: admitUntouchedClient{t: t}}).Run(
				t.Context(), changed, Continue{CommandID: "continue"},
			)
			if !errors.Is(err, ErrInvalidTurn) {
				t.Fatalf("unsupported record executed: %v", err)
			}
		})
	}
}

func TestInitialContextNilAndEmptyPreserveCommandHash(t *testing.T) {
	start := Say("work")
	start.CommandID, start.Agent = "start", testAgent()
	a, err := commandHash(start)
	if err != nil {
		t.Fatal(err)
	}
	start.Context = []ContextItem{}
	b, err := commandHash(start)
	if err != nil || a != b {
		t.Fatal("empty context changed command hash")
	}
	out, err := (Runner{}).Admit(t.Context(), Turn{ID: "legacy"}, start)
	if err != nil || out.Turn.Steps[0].Version != 1 {
		t.Fatalf("empty context changed record version: %v", err)
	}
}
