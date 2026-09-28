package kernel

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

func TestModelIntentRecordsResolvedCapabilityPolicy(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{final("done")}}
	out, err := start(t, client, nil, NewMemoryRecorder(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var intent *ModelFact
	for _, step := range out.Turn.Steps {
		if step.Kind == StepModelIntent {
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
	out, err := start(t, client, nil, NewMemoryRecorder(), nil)
	if err != nil {
		t.Fatal(err)
	}
	turn := cloneTurn(out.Turn)
	for _, step := range turn.Steps {
		if step.Kind == StepModelIntent {
			step.Model.CapabilityPolicy = "lenient"
		}
	}
	if _, err := Replay(turn); !errors.Is(err, ErrInvalidTurn) {
		t.Fatalf("replayed an unknown capability policy: %v", err)
	}
}
