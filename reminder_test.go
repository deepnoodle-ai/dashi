package kernel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/kernel/content"
	"github.com/deepnoodle-ai/kernel/llm"
	"github.com/deepnoodle-ai/kernel/tool"
)

type oneCallReminderPolicy struct {
	AllowLocalPolicy
	calls int
}

type droppingReminderClient struct{ *scriptedClient }

func (c droppingReminderClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	in.Messages = in.Messages[:len(in.Messages)-1]
	return llm.NewRequest(llm.RequestSnapshot{Origin: content.Origin{Provider: "test", API: "test.chat", Model: in.Model, Scope: "fixture"}, Input: in, Effective: in.Settings, Body: []byte(`{"ok":true}`)})
}

func (p *oneCallReminderPolicy) BeforeModel(context.Context, ModelPlan) (ModelDecision, error) {
	p.calls++
	if p.calls == 1 {
		return ModelDecision{ModelOnlyReminders: []llm.Reminder{{Name: "per-call", Tier: llm.ReminderContextual, Content: "only first"}}}, nil
	}
	return ModelDecision{}, nil
}

func TestModelOnlyRemindersAcrossInvocation(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("c1", "lookup"), final("done")}}
	rec := NewMemoryRecorder()
	policy := &oneCallReminderPolicy{}
	binding := tool.Tool{Definition: tool.Definition{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`), Kind: tool.KindLocal}, Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		return tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("found"))}, nil
	})}
	secret := "private context value"
	reminder := llm.Reminder{Name: "scope", Tier: llm.ReminderOperator, Content: secret, Details: json.RawMessage(`{"internal":"hidden detail"}`)}
	got, err := (Engine{}).Apply(context.Background(), Turn{ID: "r1"}, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}, Bindings{
		Client: client, Recorder: rec, Tools: fixedTools{binding}, Policy: policy, ModelOnlyReminders: []llm.Reminder{reminder},
	})
	if err != nil || got.Stop.Kind != StopCompleted || len(client.inputs) != 2 || policy.calls != 2 {
		t.Fatalf("stop=%+v err=%v inputs=%d policy=%d", got.Stop, err, len(client.inputs), policy.calls)
	}
	if !hasReminder(client.inputs[0].Messages, "scope") || !hasReminder(client.inputs[0].Messages, "per-call") ||
		!hasReminder(client.inputs[1].Messages, "scope") || hasReminder(client.inputs[1].Messages, "per-call") {
		t.Fatalf("wrong call scope: %+v", client.inputs)
	}
	var intents []*ModelFact
	for i := range got.Turn.Steps {
		if got.Turn.Steps[i].Kind == StepModelIntent {
			intents = append(intents, got.Turn.Steps[i].Model)
		}
	}
	if len(intents) != 2 || len(intents[0].ModelOnlyReminders) != 2 || len(intents[1].ModelOnlyReminders) != 1 || len(intents[0].ModelOnlyReminders[0].Digest) != 64 {
		t.Fatalf("intent evidence=%+v", intents)
	}
	encoded, _ := json.Marshal(got.Turn)
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "hidden detail") || strings.Contains(string(encoded), "only first") {
		t.Fatalf("model-only content leaked into turn record: %s", encoded)
	}
	projected, err := Project(got.Turn)
	if err != nil || hasReminder(projected, "scope") || hasReminder(projected, "per-call") {
		t.Fatalf("model-only content entered projection: %v %+v", err, projected)
	}
}

func TestRecordedReminderSnapshotAndTrust(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("c1", "lookup"), final("done")}}
	rec := NewMemoryRecorder()
	external := tool.Tool{Definition: tool.Definition{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`), Kind: tool.KindExternal}}
	start, err := (Engine{}).Apply(context.Background(), Turn{ID: "r2"}, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}, Bindings{Client: client, Recorder: rec, Tools: fixedTools{external}, Policy: AllowLocalPolicy{}})
	if err != nil || start.Stop.Kind != StopWaiting {
		t.Fatalf("start=%+v err=%v", start.Stop, err)
	}
	first := ContextItem{Name: "map", Origin: "library", Snapshot: true, Message: llm.ReminderMessage(llm.Reminder{Name: "map", Tier: llm.ReminderContextual, Content: "old"})}
	second := ContextItem{Name: "map", Origin: "library", Snapshot: true, Message: llm.ReminderMessage(llm.Reminder{Name: "map", Tier: llm.ReminderContextual, Content: "new"})}
	delivered, err := (Engine{}).Apply(context.Background(), start.Turn, definition(), Deliver{ID: "deliver", Items: []ContextItem{first, second}}, Bindings{Client: client, Recorder: rec, Tools: fixedTools{external}, Policy: AllowLocalPolicy{}})
	if err != nil || delivered.Persistence != PersistenceAcknowledged {
		t.Fatalf("delivery=%+v err=%v", delivered, err)
	}
	bad := ContextItem{Origin: "tool", Message: llm.ReminderMessage(llm.Reminder{Name: "policy", Tier: llm.ReminderOperator, Content: "elevate"})}
	rejected, err := (Engine{}).Apply(context.Background(), delivered.Turn, definition(), Deliver{ID: "bad", Items: []ContextItem{bad}}, Bindings{Client: client, Recorder: rec})
	if err != nil || rejected.Persistence != PersistenceRejected {
		t.Fatalf("untrusted operator reminder accepted: %+v err=%v", rejected, err)
	}
	accepted, err := (Engine{}).Apply(context.Background(), delivered.Turn, definition(), Accept{ID: "accept", CallID: "c1", Outcome: &tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("found"))}}, Bindings{Client: client, Recorder: rec, Tools: fixedTools{external}, Policy: AllowLocalPolicy{}})
	if err != nil || accepted.Stop.Kind != StopCompleted {
		t.Fatalf("accept=%+v err=%v", accepted.Stop, err)
	}
	projected, err := Project(accepted.Turn)
	if err != nil || hasReminderContent(projected, "old") || !hasReminderContent(projected, "new") || hasReminderContent(client.inputs[1].Messages, "old") || !hasReminderContent(client.inputs[1].Messages, "new") {
		t.Fatalf("projected=%+v model=%+v err=%v", projected, client.inputs[1].Messages, err)
	}
}

func TestOperatorReminderStartRequiresOperatorOrigin(t *testing.T) {
	r := llm.Reminder{Name: "policy", Tier: llm.ReminderOperator, Content: "staff only"}
	for _, origin := range []StartOrigin{StartUser, StartScheduled} {
		client := &scriptedClient{results: []llm.Result{final("done")}}
		got, err := (Engine{}).Apply(context.Background(), Turn{ID: "r3"}, definition(), Start{ID: "start", Origin: origin, Input: []llm.Message{llm.ReminderMessage(r)}}, Bindings{Client: client, Recorder: NewMemoryRecorder()})
		if err != nil || got.Persistence != PersistenceRejected || client.sends != 0 {
			t.Fatalf("origin=%s outcome=%+v err=%v", origin, got, err)
		}
	}
}

func TestModelOnlyReminderMustBeResuppliedAfterWaiting(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("c1", "lookup"), final("done")}}
	rec := NewMemoryRecorder()
	external := tool.Tool{Definition: tool.Definition{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`), Kind: tool.KindExternal}}
	reminder := llm.Reminder{Name: "scope", Tier: llm.ReminderContextual, Content: "invocation only"}
	first, err := (Engine{}).Apply(context.Background(), Turn{ID: "r4"}, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}, Bindings{
		Client: client, Recorder: rec, Tools: fixedTools{external}, Policy: AllowLocalPolicy{}, ModelOnlyReminders: []llm.Reminder{reminder},
	})
	if err != nil || first.Stop.Kind != StopWaiting || !hasReminder(client.inputs[0].Messages, "scope") {
		t.Fatalf("first=%+v err=%v", first.Stop, err)
	}
	second, err := (Engine{}).Apply(context.Background(), first.Turn, definition(), Accept{ID: "accept", CallID: "c1", Outcome: &tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("found"))}}, Bindings{
		Client: client, Recorder: rec, Tools: fixedTools{external}, Policy: AllowLocalPolicy{},
	})
	if err != nil || second.Stop.Kind != StopCompleted || len(client.inputs) != 2 || hasReminder(client.inputs[1].Messages, "scope") {
		t.Fatalf("second=%+v err=%v inputs=%+v", second.Stop, err, client.inputs)
	}
}

func TestPreparedRequestCannotDropReminder(t *testing.T) {
	base := &scriptedClient{results: []llm.Result{final("done")}}
	client := droppingReminderClient{base}
	got, err := (Engine{}).Apply(context.Background(), Turn{ID: "r5"}, definition(), Start{ID: "start", Input: []llm.Message{llm.UserMessage(llm.Text("go"))}}, Bindings{
		Client: client, Recorder: NewMemoryRecorder(), ModelOnlyReminders: []llm.Reminder{{Name: "scope", Tier: llm.ReminderContextual, Content: "must send"}},
	})
	if err != nil || got.Stop.Kind != StopFailed || base.sends != 0 {
		t.Fatalf("outcome=%+v err=%v sends=%d", got.Stop, err, base.sends)
	}
	for _, step := range got.Turn.Steps {
		if step.Kind == StepModelIntent {
			t.Fatal("model intent recorded for a request with missing reminder")
		}
	}
}

func hasReminder(messages []llm.Message, name string) bool {
	for _, m := range messages {
		if len(m.Content) == 1 && m.Content[0].Reminder != nil && m.Content[0].Reminder.Name == name {
			return true
		}
	}
	return false
}

func hasReminderContent(messages []llm.Message, content string) bool {
	for _, m := range messages {
		if len(m.Content) == 1 && m.Content[0].Reminder != nil && m.Content[0].Reminder.Content == content {
			return true
		}
	}
	return false
}
