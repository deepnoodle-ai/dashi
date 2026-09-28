package kernel

import (
	"context"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

func TestDuplicateCommandLoadsCurrentTurn(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("call-1", "lookup"), final("found")}}
	rec := NewMemoryRecorder()
	external := tool.Tool{Definition: tool.Definition{Name: "lookup", Schema: []byte(`{"type":"object"}`), Kind: tool.KindExternal}}
	first, err := start(t, client, fixedTools{external}, rec, AllowLocalPolicy{})
	if err != nil || first.Stop.Kind != StopWaiting {
		t.Fatalf("start: stop=%+v err=%v", first.Stop, err)
	}
	result := tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("result"))}
	accepted, err := (Engine{}).Apply(
		context.Background(),
		first.Turn,
		definition(),
		Accept{ID: "result", CallID: "call-1", Outcome: &result},
		Bindings{
			Client:   client,
			Tools:    fixedTools{external},
			Recorder: rec,
			Policy:   AllowLocalPolicy{},
		},
	)
	if err != nil || !accepted.ReadyToContinue || client.sends != 1 {
		t.Fatalf("accept: out=%+v err=%v sends=%d", accepted, err, client.sends)
	}
	completed, err := (Engine{}).Apply(
		context.Background(),
		accepted.Turn,
		definition(),
		Continue{ID: "continue"},
		Bindings{
			Client:   client,
			Tools:    fixedTools{external},
			Recorder: rec,
			Policy:   AllowLocalPolicy{},
		},
	)
	if err != nil || completed.Stop.Kind != StopCompleted {
		t.Fatalf("accept: stop=%+v err=%v", completed.Stop, err)
	}
	duplicate, err := (Engine{}).Apply(context.Background(), first.Turn, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("do it"))}}, Bindings{Client: client, Recorder: rec})
	if err != nil || duplicate.Stop.Kind != StopCompleted || len(duplicate.Turn.Steps) != len(completed.Turn.Steps) || client.sends != 2 {
		t.Fatalf("duplicate: stop=%+v steps=%d err=%v sends=%d", duplicate.Stop, len(duplicate.Turn.Steps), err, client.sends)
	}
}
