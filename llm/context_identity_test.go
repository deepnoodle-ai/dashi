package llm_test

import (
	"encoding/json"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestMessageContextCloneJSONAndRequestOwnership(t *testing.T) {
	message := llm.UserMessage(llm.Text("seed"))
	message.Context = &llm.MessageContext{Origin: "runtime", Name: "seed"}
	cloned := message.Clone()
	cloned.Context.Name = "clone"
	if message.Context.Name != "seed" {
		t.Fatal("clone shares context metadata")
	}
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var restored llm.Message
	if err := json.Unmarshal(data, &restored); err != nil || restored.Context == nil || *restored.Context != *message.Context {
		t.Fatalf("round trip=%+v err=%v", restored, err)
	}
	input := llm.Input{Model: "test", Messages: []llm.Message{message}}
	request, err := llm.NewRequest(llm.RequestSnapshot{Origin: content.Origin{Provider: "test", API: "test.chat", Model: "test", Scope: "test"}, Input: input, Body: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	input.Messages[0].Context.Name = "caller mutation"
	got := request.Input()
	if got.Messages[0].Context.Name != "seed" {
		t.Fatal("request shares input metadata")
	}
	got.Messages[0].Context.Name = "returned mutation"
	if request.Input().Messages[0].Context.Name != "seed" {
		t.Fatal("request exposes mutable metadata")
	}
}

func TestInputContextStructuralValidation(t *testing.T) {
	for _, identity := range []*llm.MessageContext{nil, {Origin: "runtime", Name: "seed"}, {Origin: "extension-origin", Name: "seed"}} {
		message := llm.UserMessage(llm.Text("seed"))
		message.Context = identity
		if err := (llm.Input{Model: "test", Messages: []llm.Message{message}}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, identity := range []*llm.MessageContext{{}, {Origin: "user"}, {Name: "seed"}} {
		message := llm.UserMessage(llm.Text("seed"))
		message.Context = identity
		if err := (llm.Input{Model: "test", Messages: []llm.Message{message}}).Validate(); err == nil {
			t.Fatalf("accepted %+v", identity)
		}
	}
}
