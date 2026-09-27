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

type droppingReminderClient struct{ *scriptedClient }

func (c droppingReminderClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	in.Messages = in.Messages[:len(in.Messages)-1]
	return llm.NewRequest(llm.RequestSnapshot{Origin: content.Origin{Provider: "test", API: "test.chat", Model: in.Model, Scope: "fixture"}, Input: in, Effective: in.Settings, Body: []byte(`{"ok":true}`)})
}

func TestRecordedReminderAcrossToolIterations(t *testing.T) {
	client := &scriptedClient{results: []llm.Result{callResult("c1", "lookup"), final("done")}}
	rec := NewMemoryRecorder()
	binding := tool.Tool{Definition: tool.Definition{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`), Kind: tool.KindLocal}, Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
		return tool.Outcome{State: tool.StateSucceeded, Output: ptr(tool.TextOutput("found"))}, nil
	})}
	reminder := llm.Reminder{Name: "scope", Tier: llm.ReminderContextual, Content: "persistent context"}
	got, err := (Engine{}).Apply(context.Background(), Turn{ID: "r1"}, definition(), Start{ID: "start", Input: []llm.Message{
		llm.UserMessage(llm.Text("go")), llm.ReminderMessage(reminder),
	}}, Bindings{
		Client: client, Recorder: rec, Tools: fixedTools{binding}, Policy: AllowLocalPolicy{},
	})
	if err != nil || got.Stop.Kind != StopCompleted || len(client.inputs) != 2 {
		t.Fatalf("stop=%+v err=%v inputs=%d", got.Stop, err, len(client.inputs))
	}
	if !hasReminder(client.inputs[0].Messages, "scope") || !hasReminder(client.inputs[1].Messages, "scope") {
		t.Fatalf("recorded reminder missing from model calls: %+v", client.inputs)
	}
	encoded, _ := json.Marshal(got.Turn)
	if !strings.Contains(string(encoded), "persistent context") {
		t.Fatalf("recorded reminder missing from turn: %s", encoded)
	}
	projected, err := Project(got.Turn)
	if err != nil || !hasReminder(projected, "scope") {
		t.Fatalf("recorded reminder missing from projection: %v %+v", err, projected)
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

func TestPreparedRequestCannotDropReminder(t *testing.T) {
	base := &scriptedClient{results: []llm.Result{final("done")}}
	client := droppingReminderClient{base}
	got, err := (Engine{}).Apply(context.Background(), Turn{ID: "r5"}, definition(), Start{ID: "start", Input: []llm.Message{
		llm.UserMessage(llm.Text("go")), llm.ReminderMessage(llm.Reminder{Name: "scope", Tier: llm.ReminderContextual, Content: "must send"}),
	}}, Bindings{
		Client: client, Recorder: NewMemoryRecorder(),
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

type reminderDeliveryPolicy struct{ AllowLocalPolicy }

func (reminderDeliveryPolicy) BeforeModel(
	context.Context,
	ModelPlan,
) (ModelDecision, error) {
	reminder := llm.Reminder{
		Name:    "scope",
		Tier:    llm.ReminderContextual,
		Content: "delivered",
	}
	item := ContextItem{
		Origin:  "library",
		Message: llm.ReminderMessage(reminder),
	}
	return ModelDecision{Deliver: []ContextItem{item}}, nil
}

func TestReminderInstructionOnlySentWithReminders(t *testing.T) {
	const tag = "<system-reminder>"
	reminder := llm.Reminder{
		Name:    "scope",
		Tier:    llm.ReminderContextual,
		Content: "ctx",
	}
	run := func(t *testing.T, input []llm.Message, policy Policy) string {
		t.Helper()
		client := &scriptedClient{results: []llm.Result{final("done")}}
		got, err := (Engine{}).Apply(
			context.Background(),
			Turn{ID: "r6"},
			definition(),
			Start{ID: "start", Input: input},
			Bindings{
				Client:   client,
				Recorder: NewMemoryRecorder(),
				Policy:   policy,
			},
		)
		if err != nil {
			t.Fatalf("apply failed: %v", err)
		}
		if got.Stop.Kind != StopCompleted {
			t.Fatalf("turn did not complete: %+v", got.Stop)
		}
		if len(client.inputs) != 1 {
			t.Fatalf("expected one model call, got %d", len(client.inputs))
		}
		return client.inputs[0].System
	}
	plain := []llm.Message{llm.UserMessage(llm.Text("go"))}
	if system := run(t, plain, AllowLocalPolicy{}); strings.Contains(system, tag) {
		t.Fatalf("reminder instruction sent without a reminder: %q", system)
	}
	withReminder := []llm.Message{llm.ReminderMessage(reminder)}
	if system := run(t, withReminder, AllowLocalPolicy{}); !strings.Contains(system, tag) {
		t.Fatalf("reminder instruction missing for start reminder: %q", system)
	}
	delivered := run(t, plain, reminderDeliveryPolicy{})
	if !strings.Contains(delivered, tag) {
		t.Fatalf("reminder instruction missing for delivered reminder: %q", delivered)
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
