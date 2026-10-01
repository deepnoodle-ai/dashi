package dashi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

type droppingReminderClient struct{ *scriptedClient }

func (c droppingReminderClient) Prepare(
	_ context.Context,
	in llm.Input,
) (llm.Request, error) {
	in.Messages = in.Messages[:len(in.Messages)-1]
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    in.Model,
			Scope:    "fixture",
		},
		Input:     in,
		Effective: in.Settings,
		Body:      []byte(`{"ok":true}`),
	})
}

func TestRecordedReminderAcrossToolIterations(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{callResult("c1", "lookup"), final("done")},
	}
	rec := NewMemoryRecorder()
	binding := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("found")),
			}, nil
		}),
	}
	reminder := llm.Reminder{
		Name:    "scope",
		Tier:    llm.ReminderContextual,
		Content: "persistent context",
	}
	got, err := (Runner{
		Client:   client,
		Recorder: rec,
		Tools:    tool.List{binding},
	}).Run(
		t.Context(),
		Turn{ID: "r1"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input: []llm.Message{
				llm.UserMessage(llm.Text("go")),
				llm.ReminderMessage(reminder),
			},
		},
	)
	if err != nil || got.Stop.Kind != StopCompleted ||
		len(client.inputs) != 2 {
		t.Fatalf("stop=%+v err=%v inputs=%d", got.Stop, err, len(client.inputs))
	}
	if !hasReminder(client.inputs[0].Messages, "scope") ||
		!hasReminder(client.inputs[1].Messages, "scope") {
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
	client := &scriptedClient{
		results: []llm.Result{callResult("c1", "lookup"), final("done")},
	}
	rec := NewMemoryRecorder()
	external := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindExternal,
		},
	}
	start, err := (Runner{
		Client:   client,
		Recorder: rec,
		Tools:    tool.List{external},
	}).Run(
		t.Context(),
		Turn{ID: "r2"},
		Start{CommandID: "start", Agent: testAgent(), Input: []llm.Message{llm.UserMessage(llm.Text("go"))}},
	)
	if err != nil || start.Stop.Kind != StopWaiting {
		t.Fatalf("start=%+v err=%v", start.Stop, err)
	}
	first := ContextItem{
		Name:     "map",
		Origin:   ContextRuntime,
		Snapshot: true,
		Message: llm.ReminderMessage(llm.Reminder{
			Name:    "map",
			Tier:    llm.ReminderContextual,
			Content: "old",
		}),
	}
	second := ContextItem{
		Name:     "map",
		Origin:   ContextRuntime,
		Snapshot: true,
		Message: llm.ReminderMessage(llm.Reminder{
			Name:    "map",
			Tier:    llm.ReminderContextual,
			Content: "new",
		}),
	}
	delivered, err := (Runner{
		Client:   client,
		Recorder: rec,
		Tools:    tool.List{external},
	}).Run(
		t.Context(),
		start.Turn,
		AddMessages{CommandID: "deliver", Items: []ContextItem{first, second}},
	)
	if err != nil {
		t.Fatalf("delivery=%+v err=%v", delivered, err)
	}
	bad := ContextItem{
		Origin: "tool",
		Message: llm.ReminderMessage(llm.Reminder{
			Name:    "policy",
			Tier:    llm.ReminderOperator,
			Content: "elevate",
		}),
	}
	rejected, err := (Runner{
		Client:   client,
		Recorder: rec,
	}).Run(
		t.Context(),
		delivered.Turn,
		AddMessages{CommandID: "bad", Items: []ContextItem{bad}},
	)
	// AddMessages checks its items before recording anything.
	if persistence(err) != "other" ||
		len(rejected.Turn.Steps) != len(delivered.Turn.Steps) {
		t.Fatalf("untrusted operator reminder accepted: %+v err=%v", rejected, err)
	}
	accepted, err := (Runner{
		Client:   client,
		Recorder: rec,
		Tools:    tool.List{external},
	}).Run(
		t.Context(),
		delivered.Turn,
		Resolve{
			CommandID: "accept",
			CallID:    "c1",
			Outcome: &tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("found")),
			},
		},
	)
	if err != nil || !accepted.ReadyToContinue || client.sends != 1 {
		t.Fatalf("accept=%+v err=%v sends=%d", accepted, err, client.sends)
	}
	completed, err := (Runner{
		Client:   client,
		Recorder: rec,
		Tools:    tool.List{external},
	}).Run(t.Context(), accepted.Turn, Continue{CommandID: "continue"})
	if err != nil || completed.Stop.Kind != StopCompleted {
		t.Fatalf("continue=%+v err=%v", completed, err)
	}
	// A replaced snapshot stays in place, ahead of its replacement, so the
	// prompt prefix is stable (design/prompt-cache.md).
	projected, err := Project(completed.Turn)
	if err != nil || !reminderBefore(projected, "old", "new") ||
		!reminderBefore(client.inputs[1].Messages, "old", "new") {
		t.Fatalf(
			"projected=%+v model=%+v err=%v",
			projected,
			client.inputs[1].Messages,
			err,
		)
	}
}

func TestOperatorReminderStartRequiresOperatorOrigin(t *testing.T) {
	r := llm.Reminder{
		Name:    "policy",
		Tier:    llm.ReminderOperator,
		Content: "staff only",
	}
	for _, origin := range []StartOrigin{StartUser, StartScheduled} {
		client := &scriptedClient{results: []llm.Result{final("done")}}
		got, err := (Runner{
			Client:   client,
			Recorder: NewMemoryRecorder(),
		}).Run(
			t.Context(),
			Turn{ID: "r3"},
			Start{
				CommandID: "start",
				Agent:     testAgent(),
				Origin:    origin,
				Input:     []llm.Message{llm.ReminderMessage(r)},
			},
		)
		if persistence(err) != PersistenceRejected || client.sends != 0 {
			t.Fatalf("origin=%s outcome=%+v err=%v", origin, got, err)
		}
	}
}

func TestPreparedRequestCannotDropReminder(t *testing.T) {
	base := &scriptedClient{results: []llm.Result{final("done")}}
	client := droppingReminderClient{base}
	got, err := (Runner{
		Client:   client,
		Recorder: NewMemoryRecorder(),
	}).Run(
		t.Context(),
		Turn{ID: "r5"},
		Start{
			CommandID: "start",
			Agent:     testAgent(),
			Input: []llm.Message{
				llm.UserMessage(llm.Text("go")),
				llm.ReminderMessage(llm.Reminder{
					Name:    "scope",
					Tier:    llm.ReminderContextual,
					Content: "must send",
				}),
			},
		},
	)
	if err != nil || got.Stop.Kind != StopFailed || base.sends != 0 {
		t.Fatalf("outcome=%+v err=%v sends=%d", got.Stop, err, base.sends)
	}
	for _, step := range got.Turn.Steps {
		if step.Kind == StepModelCallStarted {
			t.Fatal("model intent recorded for a request with missing reminder")
		}
	}
}

// TestSystemPromptStableAcrossReminders checks that the reminder
// instruction is always sent, so the system prompt, and with it the
// provider's prompt cache, does not change when a reminder appears on one
// call and drops out on the next.
func TestSystemPromptStableAcrossReminders(t *testing.T) {
	const tag = "<system-reminder>"
	client := &scriptedClient{
		results: []llm.Result{
			callResult("c1", "lookup"),
			callResult("c2", "lookup"),
			final("done"),
		},
	}
	binding := tool.Tool{
		Definition: tool.Definition{
			Name:   "lookup",
			Schema: json.RawMessage(`{"type":"object"}`),
			Kind:   tool.KindLocal,
		},
		Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
			return tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("found")),
			}, nil
		}),
	}
	// The hook delivers a one-call reminder on the second call only, so the
	// first call has none, the second has one, and the third has none again.
	calls := 0
	hooks := Hooks{BeforeModel: []ModelHook{{
		Name: "once",
		Func: func(context.Context, ModelPlan) (ModelDecision, error) {
			calls++
			if calls != 2 {
				return ModelDecision{}, nil
			}
			return ModelDecision{AddMessages: []ContextItem{{
				Origin: ContextRuntime,
				Message: llm.ReminderMessage(llm.Reminder{
					Name:    "scope",
					Tier:    llm.ReminderContextual,
					Content: "ctx",
				}),
				NextCallOnly: true,
			}}}, nil
		},
	}}}
	agent := testAgent()
	agent.Limits.ModelCalls = 5
	got, err := (Runner{
		Client:   client,
		Recorder: NewMemoryRecorder(),
		Tools:    tool.List{binding},
		Hooks:    hooks,
	}).Run(
		t.Context(),
		Turn{ID: "r6"},
		Start{
			CommandID: "start",
			Agent:     agent,
			Input:     []llm.Message{llm.UserMessage(llm.Text("go"))},
		},
	)
	if err != nil || got.Stop.Kind != StopCompleted || len(client.inputs) != 3 {
		t.Fatalf("stop=%+v err=%v inputs=%d", got.Stop, err, len(client.inputs))
	}
	if hasReminder(client.inputs[0].Messages, "scope") ||
		!hasReminder(client.inputs[1].Messages, "scope") ||
		hasReminder(client.inputs[2].Messages, "scope") {
		t.Fatal("the reminder should reach only the second call")
	}
	first := client.inputs[0].System
	if !strings.Contains(first, tag) {
		t.Fatalf("reminder instruction missing: %q", first)
	}
	for i, in := range client.inputs {
		if in.System != first {
			t.Fatalf("system prompt of call %d differs:\n%q\n%q", i+1, in.System, first)
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

// reminderBefore reports whether reminder content first appears before
// content second.
func reminderBefore(messages []llm.Message, first, second string) bool {
	at := func(text string) int {
		for i, m := range messages {
			if hasReminderContent([]llm.Message{m}, text) {
				return i
			}
		}
		return -1
	}
	i, j := at(first), at(second)
	return i >= 0 && j > i
}

// unsettledCallsTurn records one model result with three calls that end
// started, waiting, and uncertain, and an uncertain stop.
func unsettledCallsTurn() Turn {
	def := tool.Definition{
		Name:   "work",
		Kind:   tool.KindLocal,
		Schema: []byte(`{"type":"object"}`),
	}
	intent := interruptionModelIntent("model-1")
	intent.Model.Tools = []tool.Definition{def}
	var parts []llm.Content
	var calls []tool.Call
	for _, id := range []string{"started", "waiting", "uncertain"} {
		call := tool.Call{ID: id, Name: "work", Args: []byte(`{}`)}
		calls = append(calls, call)
		parts = append(parts, llm.Content{Kind: llm.ContentToolCall, ToolCall: &call})
	}
	result := llm.Result{
		Message:   llm.AssistantMessage(parts...),
		Stop:      llm.Stop{Kind: llm.StopToolUse},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}
	steps := []Step{
		interruptionStart(testAgent()),
		intent,
		{Kind: StepModelCallFinished, Model: &ModelFact{
			ModelCallID: "model-1",
			MessageID:   "message-1",
			Result:      &result,
		}},
	}
	for _, call := range calls {
		steps = append(steps,
			Step{Kind: StepToolDecision, Tool: &ToolFact{
				Call:    call,
				Verdict: ToolVerdict{Kind: DecisionAllow},
			}},
			Step{Kind: StepToolStarted, Tool: &ToolFact{Call: call}},
		)
	}
	wait := tool.Outcome{State: tool.StateWaiting, Wait: &tool.Wait{Reason: "external"}}
	steps = append(steps,
		Step{Kind: StepToolWaiting, Tool: &ToolFact{Call: calls[1], Outcome: &wait}},
		Step{Kind: StepToolFinished, Tool: &ToolFact{Call: calls[2]}},
		Step{Kind: StepTurnStopped, Stop: &Stop{Kind: StopResultUnknown}},
	)
	return interruptionRecord(steps...)
}

func TestCanceledTurnProjectsUnsettledCallsAndFreezes(t *testing.T) {
	turn := unsettledCallsTurn()
	if _, err := Project(turn); err == nil {
		t.Fatal("an open turn with unsettled calls projected")
	}
	runner := Runner{Client: &scriptedClient{}}
	canceled, err := runner.Run(t.Context(), turn, Cancel{CommandID: "close"})
	if err != nil || !canceled.Stop.Terminal {
		t.Fatalf("cancel=%+v err=%v", canceled.Stop, err)
	}
	messages, err := Project(canceled.Turn)
	if err != nil {
		t.Fatal(err)
	}
	// The input, the model's calls, then one error result per call.
	if len(messages) != 5 {
		t.Fatalf("projected %d messages, want 5", len(messages))
	}
	for i, id := range []string{"started", "waiting", "uncertain"} {
		r := messages[2+i].Content[0].ToolResult
		if r == nil || r.CallID != id || !r.IsError ||
			!canonjson.SameValue(r.Output, tool.TextOutput(endedToolText)) {
			t.Fatalf("result %d = %+v", i, r)
		}
	}
	before, _ := json.Marshal(messages)

	// A late result is recorded and counted, but the projection is frozen.
	late, err := runner.Run(t.Context(), canceled.Turn, Resolve{
		CommandID: "late",
		CallID:    "started",
		Outcome:   new(tool.Succeeded("done late")),
	})
	if err != nil || late.Tools["started"].Status != ToolFinished {
		t.Fatalf("late=%+v err=%v", late.Tools["started"], err)
	}
	after, err := Project(late.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(after); !bytes.Equal(raw, before) {
		t.Fatalf("late result changed the projection:\n%s\n%s", before, raw)
	}
}

// A canceled turn with a model attempt in flight projects its input only.
func TestCanceledTurnDropsPendingModelAttempt(t *testing.T) {
	turn := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelIntent("model-1"),
	)
	canceled, err := (Runner{Client: &scriptedClient{}}).Run(
		t.Context(), turn, Cancel{CommandID: "close"},
	)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := Project(canceled.Turn)
	if err != nil || len(messages) != 1 || messages[0].Role != llm.RoleUser {
		t.Fatalf("messages=%+v err=%v", messages, err)
	}
}

// Continue on a turn with no recorded stop tells the next model call that
// the worker stopped mid-turn; the call after it no longer sees that.
func TestCrashResumeCarriesReminder(t *testing.T) {
	crashed := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelIntent("model-1"),
	)
	client := &scriptedClient{results: []llm.Result{
		callResult("call-1", "lookup"),
		final("done"),
	}}
	runner := Runner{
		Client: client,
		Tools:  tool.List{localTool("lookup", execFunc(lookup42))},
	}
	out, err := runner.Run(t.Context(), crashed, Continue{CommandID: "resume"})
	if err != nil || out.Stop.Kind != StopCompleted || len(client.inputs) != 2 {
		t.Fatalf("resume=%+v err=%v inputs=%d", out.Stop, err, len(client.inputs))
	}
	if n := countReminders(client.inputs[0].Messages); n != 1 {
		t.Fatalf("first call saw %d restart reminders, want 1", n)
	}
	if n := countReminders(client.inputs[1].Messages); n != 0 {
		t.Fatalf("second call saw %d restart reminders, want 0", n)
	}

	// A Continue after a recorded stop adds nothing.
	paused := make(chan struct{})
	close(paused)
	stopped, err := (Runner{Client: client}).Run(
		t.Context(), Turn{ID: "p"}, limitsStart(testAgent()), WithPause(paused),
	)
	if err != nil || stopped.Stop.Kind != StopInterrupted {
		t.Fatalf("pause=%+v err=%v", stopped.Stop, err)
	}
	client.results = append(client.results, final("ok"))
	if _, err := (Runner{Client: client}).Run(
		t.Context(), stopped.Turn, Continue{},
	); err != nil {
		t.Fatal(err)
	}
	last := client.inputs[len(client.inputs)-1]
	if n := countReminders(last.Messages); n != 0 {
		t.Fatalf("resume after a pause saw %d restart reminders", n)
	}
}

func countReminders(messages []llm.Message) int {
	n := 0
	for _, m := range messages {
		for _, part := range m.Content {
			if part.Reminder != nil && part.Reminder.Name == "worker-restarted" {
				n++
			}
		}
	}
	return n
}

// A late model result after a cancel counts in Spend, but Answer, like
// Project, stays fixed at the terminal stop.
func TestLateModelResultKeepsEndedAnswer(t *testing.T) {
	client := &scriptedClient{
		results: []llm.Result{{}},
		errors:  []error{errors.New("connection lost")},
	}
	first, err := start(t, client, writeTool(), NewMemoryRecorder(), Hooks{})
	if err != nil || first.Stop.Kind != StopResultUnknown {
		t.Fatalf("start: stop=%+v err=%v", first.Stop, err)
	}
	runner := Runner{Client: client}
	canceled, err := runner.Run(t.Context(), first.Turn, Cancel{CommandID: "close"})
	if err != nil || !canceled.Stop.Terminal {
		t.Fatalf("cancel=%+v err=%v", canceled.Stop, err)
	}
	before, err := Project(canceled.Turn)
	if err != nil {
		t.Fatal(err)
	}
	late := final("late")
	late.Usage = &llm.Usage{InputTokens: 5, OutputTokens: 2}
	out, err := runner.Run(t.Context(), canceled.Turn, Resolve{
		CommandID:   "late",
		CallID:      "t1:model:1",
		ModelResult: &late,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Spend.Usage.OutputTokens != 2 {
		t.Fatalf("late result not counted: spend=%+v", out.Spend.Usage)
	}
	if out.Reply != "" || out.Next() != NextNone {
		t.Fatalf("answer=%q next=%s", out.Reply, out.Next())
	}
	view, err := Replay(out.Turn)
	if err != nil || view.Reply != "" {
		t.Fatalf("replayed answer=%q err=%v", view.Reply, err)
	}
	after, err := Project(out.Turn)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if !bytes.Equal(a, b) {
		t.Fatalf("late result changed the projection:\n%s\n%s", a, b)
	}
}

// A second crash before any model call reaches the provider does not add a
// second reminder: the next request carries one.
func TestRepeatedCrashResumeCarriesOneReminder(t *testing.T) {
	crashed := interruptionRecord(
		interruptionStart(testAgent()),
		interruptionModelIntent("model-1"),
	)
	// The first resume records its Continue, then its worker crashes
	// before the model call: keep the record only up to that Continue.
	client := &scriptedClient{results: []llm.Result{final("unused")}}
	runner := Runner{Client: client}
	first, err := runner.Run(t.Context(), crashed, Continue{CommandID: "resume-1"})
	if err != nil {
		t.Fatal(err)
	}
	cut := slices.IndexFunc(first.Turn.Steps, func(s Step) bool {
		return s.Kind == StepTurnContinued
	})
	if cut < 0 {
		t.Fatal("no continuation step recorded")
	}
	crashedAgain := cloneTurn(first.Turn)
	crashedAgain.Steps = crashedAgain.Steps[:cut+1]
	if view, err := Replay(crashedAgain); err != nil || view.Next() != NextResume {
		t.Fatalf("second crash: next=%s err=%v", view.Next(), err)
	}

	client = &scriptedClient{results: []llm.Result{final("done")}}
	runner.Client = client
	out, err := runner.Run(t.Context(), crashedAgain, Continue{CommandID: "resume-2"})
	if err != nil || out.Stop.Kind != StopCompleted || len(client.inputs) != 1 {
		t.Fatalf("resume=%+v err=%v inputs=%d", out.Stop, err, len(client.inputs))
	}
	if n := countReminders(client.inputs[0].Messages); n != 1 {
		t.Fatalf("request saw %d restart reminders, want 1", n)
	}
}
