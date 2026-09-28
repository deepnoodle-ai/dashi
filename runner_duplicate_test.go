package dashi

import (
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

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
		Agent:    testAgent(),
		Client:   client,
		Tools:    tool.List{external},
		Recorder: rec,
	}).Run(
		t.Context(),
		first.Turn,
		Settle{CommandID: "result", CallID: "call-1", Outcome: &result},
	)
	if err != nil || !accepted.ReadyToContinue || client.sends != 1 {
		t.Fatalf("settle: out=%+v err=%v sends=%d", accepted, err, client.sends)
	}
	completed, err := (Runner{
		Agent:    testAgent(),
		Client:   client,
		Tools:    tool.List{external},
		Recorder: rec,
	}).Run(t.Context(), accepted.Turn, Continue{CommandID: "continue"})
	if err != nil || completed.Stop.Kind != StopCompleted {
		t.Fatalf("settle: stop=%+v err=%v", completed.Stop, err)
	}
	duplicate, err := (Runner{
		Agent:    testAgent(),
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		first.Turn,
		Start{CommandID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("do it"))}},
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
