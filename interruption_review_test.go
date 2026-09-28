package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/deepnoodle-ai/kernel/llm"
)

func TestContinueAcceptsLegacyDefaultInterruptionLimit(t *testing.T) {
	legacy := definition()
	legacy.Limits = DefaultLimits()
	legacy.Limits.Interruptions = 0
	current := legacy
	current.Limits.Interruptions = DefaultLimits().Interruptions
	client := &scriptedClient{results: []llm.Result{final("done")}}
	rec := NewMemoryRecorder()
	pause := make(chan struct{})
	close(pause)
	start := Start{
		ID:    "start",
		Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
	}
	first, err := (Engine{}).Apply(
		context.Background(),
		Turn{ID: "legacy"},
		legacy,
		start,
		Bindings{Client: client, Recorder: rec, Pause: pause},
	)
	if err != nil || first.Stop.Kind != StopInterrupted || client.sends != 0 {
		t.Fatalf(
			"legacy start: stop=%+v err=%v sends=%d",
			first.Stop,
			err,
			client.sends,
		)
	}
	raw, err := json.Marshal(first.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"interruptions"`)) {
		t.Fatalf("legacy record unexpectedly has interruption limit: %s", raw)
	}
	var saved Turn
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	changed := current
	changed.Limits.Interruptions++
	if _, err := (Engine{}).Apply(
		context.Background(),
		saved,
		changed,
		Continue{ID: "wrong-limit"},
		Bindings{Client: client, Recorder: rec},
	); !errors.Is(err, ErrDefinitionChanged) {
		t.Fatalf("changed limit passed definition check: %v", err)
	}
	done, err := (Engine{}).Apply(
		context.Background(),
		saved,
		current,
		Continue{ID: "resume"},
		Bindings{Client: client, Recorder: rec},
	)
	if err != nil || done.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf(
			"legacy continuation: stop=%+v err=%v sends=%d",
			done.Stop,
			err,
			client.sends,
		)
	}
	duplicate, err := (Engine{}).Apply(
		context.Background(),
		saved,
		current,
		start,
		Bindings{Client: client, Recorder: rec},
	)
	if err != nil || duplicate.Stop.Kind != StopCompleted || client.sends != 1 {
		t.Fatalf(
			"legacy duplicate: stop=%+v err=%v sends=%d",
			duplicate.Stop,
			err,
			client.sends,
		)
	}
}
