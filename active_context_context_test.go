package dashi

import (
	"context"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestActiveCompactionSummaryUsesLiveProjectedContext(t *testing.T) {
	c, a := activeFixture()
	c.results = append(c.results[:3], final("done"))
	temporary := strings.Repeat("temporary note ", 560)
	runtimeNote := "Persistent runtime verification instruction."
	operatorNote := "Operator instruction: retain the original objective."
	runtimeMessage := llm.UserMessage(llm.Text(runtimeNote))
	operatorMessage := llm.UserMessage(llm.Text(operatorNote))
	operatorMessage.Role = llm.RoleDeveloper
	hooks := Hooks{BeforeModel: []ModelHook{{Name: "context", Func: func(_ context.Context, p ModelPlan) (ModelDecision, error) {
		if p.Turn.ModelAttempts != 0 {
			return ModelDecision{}, nil
		}
		return ModelDecision{Deliver: []ContextItem{
			{ID: "temporary", Origin: ContextRuntime, NextCallOnly: true, Message: llm.UserMessage(llm.Text(temporary))},
			{ID: "runtime", Origin: ContextRuntime, Name: "persistent", Message: runtimeMessage},
			{ID: "operator", Origin: ContextOperator, Name: "objective", Message: operatorMessage},
		}}, nil
	}}}}
	calls := 0
	out, err := (Runner{Client: c, Tools: activeTools(&calls), Hooks: hooks}).Run(t.Context(), Turn{ID: "projected-context"}, activeStart(a))
	if err != nil || out.Stop.Kind != StopCompleted || calls != 2 || c.sends != 4 || len(out.ContextCompactions) != 1 {
		t.Fatalf("stop=%+v calls=%d sends=%d summaries=%d err=%v", out.Stop, calls, c.sends, len(out.ContextCompactions), err)
	}
	for i, input := range c.sent {
		hasTemporary := false
		foundRuntime, foundOperator := -1, -1
		for index, message := range input.Messages {
			if strings.Contains(message.Text(), "temporary note") {
				hasTemporary = true
			}
			if message.Text() == runtimeNote {
				foundRuntime = index
				if !canonjson.SameValue(message, runtimeMessageWithContext(runtimeMessage, string(ContextRuntime), "persistent")) {
					t.Fatalf("send %d runtime role/metadata changed: %+v", i, message)
				}
			}
			if message.Text() == operatorNote {
				foundOperator = index
				if message.Role != llm.RoleDeveloper || message.Context == nil || message.Context.Origin != string(ContextOperator) || message.Context.Name != "objective" {
					t.Fatalf("send %d operator role/metadata changed: %+v", i, message)
				}
			}
		}
		if hasTemporary != (i == 0) {
			t.Fatalf("send %d expired one-call context=%v", i, hasTemporary)
		}
		if foundRuntime < 0 || foundOperator < foundRuntime {
			t.Fatalf("send %d persistent deliveries lost/reordered: runtime=%d operator=%d", i, foundRuntime, foundOperator)
		}
	}
	rawTemporary := false
	for _, step := range out.Turn.Steps {
		if step.Context != nil {
			for _, item := range step.Context.Items {
				if item.ID == "temporary" && item.Message.Text() == temporary {
					rawTemporary = true
				}
			}
		}
	}
	if !rawTemporary {
		t.Fatal("expired delivery erased from raw facts")
	}
	if c.sent[2].System != activeSummaryPrompt {
		t.Fatal("fixture did not dispatch a summary")
	}
}
func runtimeMessageWithContext(message llm.Message, origin, name string) llm.Message {
	message = message.Clone()
	message.Context = &llm.MessageContext{Origin: origin, Name: name}
	return message
}
