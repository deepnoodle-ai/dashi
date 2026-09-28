package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func TestLimitsZeroValueResolvesToDefaults(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{}
	want := Limits{
		ModelAttempts:   8,
		Interruptions:   10,
		ProviderPauses:  3,
		ToolConcurrency: 1,
	}
	if got := def.resolved().Limits; got != want {
		t.Fatalf("zero limits resolved to %+v, want %+v", got, want)
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("zero limits were rejected: %v", err)
	}
}

func TestLimitsResolveKeepsExplicitValues(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{
		ModelAttempts:         2,
		Interruptions:         4,
		ProviderPauses:        5,
		ToolConcurrency:       6,
		NoReservedFinalAnswer: true,
	}
	if got := def.resolved().Limits; got != def.Limits {
		t.Fatalf("explicit limits resolved to %+v, want %+v", got, def.Limits)
	}
}

func TestLimitsNegativeCountResolvesToNone(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{Interruptions: -5, ProviderPauses: -2}
	if err := def.Validate(); err != nil {
		t.Fatalf("negative none counts were rejected: %v", err)
	}
	got := def.resolved().Limits
	if got.Interruptions != -1 {
		t.Fatalf("interruptions resolved to %d, want -1", got.Interruptions)
	}
	if got.ProviderPauses != -1 {
		t.Fatalf("provider pauses resolved to %d, want -1", got.ProviderPauses)
	}
}

func TestLimitsRejectNegativeAttemptsAndConcurrency(t *testing.T) {
	tests := []struct {
		name   string
		limits Limits
	}{
		{name: "model attempts", limits: Limits{ModelAttempts: -1}},
		{name: "tool concurrency", limits: Limits{ToolConcurrency: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := testAgent()
			def.Limits = tt.limits
			if err := def.Validate(); err == nil {
				t.Fatal("negative limit was accepted")
			}
		})
	}
}

func TestNegativeProviderPausesAllowsNone(t *testing.T) {
	def := testAgent()
	def.Limits.ProviderPauses = -1
	paused := llm.Result{
		Message: llm.AssistantMessage(llm.Text("working")),
		Stop:    llm.Stop{Kind: llm.StopPause},
	}
	client := &scriptedClient{results: []llm.Result{paused, final("done")}}
	out, err := (Runner{Agent: def, Client: client}).Run(
		t.Context(),
		Turn{ID: "pauses"},
		limitsStart(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopLimited || out.Stop.Reason != "provider_pauses" {
		t.Fatalf("paused turn stopped with %+v, want provider_pauses", out.Stop)
	}
	if out.Stop.Limit == nil || out.Stop.Limit.Limit != 0 {
		t.Fatalf("limit detail=%+v, want a limit of 0", out.Stop.Limit)
	}
	if client.sends != 1 {
		t.Fatalf("sends=%d, want no continuation after the pause", client.sends)
	}
}

func TestNegativeInterruptionsAllowsNoResume(t *testing.T) {
	def := testAgent()
	def.Limits.Interruptions = -1
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := NewMemoryRecorder()
	pause := make(chan struct{})
	close(pause)
	first, err := (Runner{Agent: def, Client: client, Recorder: rec}).Run(
		t.Context(),
		Turn{ID: "interruptions"},
		limitsStart(),
		WithPause(pause),
	)
	if err != nil || first.Stop.Kind != StopInterrupted {
		t.Fatalf("start: stop=%+v err=%v", first.Stop, err)
	}
	if first.ReadyToContinue {
		t.Fatal("interrupted turn is ready to continue with no resumes allowed")
	}
	out, err := (Runner{Agent: def, Client: client, Recorder: rec}).Run(
		t.Context(),
		first.Turn,
		Continue{CommandID: "resume"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != StopLimited || out.Stop.Reason != "interruptions" {
		t.Fatalf("resume stopped with %+v, want interruptions", out.Stop)
	}
	if out.Stop.Limit == nil || out.Stop.Limit.Limit != 0 {
		t.Fatalf("limit detail=%+v, want a limit of 0", out.Stop.Limit)
	}
	if client.sends != 0 {
		t.Fatalf("sends=%d, want no model call", client.sends)
	}
}

func TestNoReservedFinalAnswerAdvertisesToolsOnLastAttempt(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{ModelAttempts: 2, NoReservedFinalAnswer: true}
	client := &scriptedClient{
		results: []llm.Result{callResult("lookup-1", "lookup"), final("42")},
	}
	binding := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(lookup42),
	}
	out, err := (Runner{
		Agent:  def,
		Client: client,
		Tools:  fixedTools{binding},
		Policy: AllowLocalPolicy{},
	}).Run(t.Context(), Turn{ID: "no-reserve"}, limitsStart())
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("stop=%+v err=%v, want completed", out.Stop, err)
	}
	if len(client.inputs) != 2 || len(client.inputs[1].Tools) != 1 {
		t.Fatalf("last attempt did not advertise tools: %+v", client.inputs)
	}
}

func TestRecordedAgentKeepsResolvedLimits(t *testing.T) {
	def := testAgent()
	def.Limits = Limits{}
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := NewMemoryRecorder()
	pause := make(chan struct{})
	close(pause)
	first, err := (Runner{Agent: def, Client: client, Recorder: rec}).Run(
		t.Context(),
		Turn{ID: "recorded"},
		limitsStart(),
		WithPause(pause),
	)
	if err != nil || first.Stop.Kind != StopInterrupted {
		t.Fatalf("start: stop=%+v err=%v", first.Stop, err)
	}
	var record struct {
		Limits map[string]any `json:"limits"`
	}
	raw, err := json.Marshal(first.Turn.Steps[0].Started.Agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]float64{
		"model_attempts":   8,
		"interruptions":    10,
		"provider_pauses":  3,
		"tool_concurrency": 1,
	} {
		if record.Limits[field] != want {
			got := record.Limits[field]
			t.Fatalf("recorded %s=%v, want %v", field, got, want)
		}
	}

	// A record made under different defaults keeps its own numbers. The
	// caller's zero limits now resolve differently, so they no longer match.
	old := first.Turn
	old.Steps = cloneTurn(first.Turn).Steps
	older := *old.Steps[0].Started
	older.Agent.Limits.Interruptions = 7
	old.Steps[0].Started = &older
	view, err := Replay(old)
	if err != nil {
		t.Fatal(err)
	}
	if view.Agent.Limits.Interruptions != 7 {
		t.Fatalf(
			"replayed interruptions=%d, want recorded 7",
			view.Agent.Limits.Interruptions,
		)
	}
	if _, err := (Runner{Agent: def, Client: client}).Run(
		t.Context(),
		old,
		Continue{CommandID: "resume"},
	); !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("zero limits matched a record with other values: %v", err)
	}
	pinned := def
	pinned.Limits = view.Agent.Limits
	out, err := (Runner{Agent: pinned, Client: client}).Run(
		t.Context(),
		old,
		Continue{CommandID: "resume"},
	)
	if err != nil || out.Stop.Kind != StopCompleted {
		t.Fatalf("pinned continue: stop=%+v err=%v", out.Stop, err)
	}
}

func limitsStart() Start {
	return Start{
		CommandID: "start",
		Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
	}
}

func lookup42(context.Context, tool.Call) (tool.Outcome, error) {
	return tool.Outcome{
		State:  tool.StateSucceeded,
		Output: new(tool.TextOutput("42")),
	}, nil
}
