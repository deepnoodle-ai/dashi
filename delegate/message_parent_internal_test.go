package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// message_parent is bound to its task: a call from a turn of another
// session fails and sends nothing.
func TestMessageParentRunsOnlyInItsTasksTurns(t *testing.T) {
	ctx := t.Context()
	ts := NewTasks(Config{OnTask: func(TaskEvent) {}})
	defer ts.Close(ctx)
	task := newTask(Subagent{Name: "scout"}, dashi.CallRef{TurnID: "p", CallID: "c"}, "")
	ts.mu.Lock()
	ts.addLocked(task)
	ts.mu.Unlock()
	runner := dashi.Runner{
		Recorder: dashi.NewMemoryRecorder(),
		Client: dashitest.NewClient(
			dashitest.CallTool("m1", MessageParentToolName, `{"message":"hi"}`),
			dashitest.Reply("done"),
		),
		Tools: childTools{ts: ts, taskID: task.id},
	}
	out, err := runner.Run(ctx, dashi.Turn{ID: "other-1"}, dashi.Start{
		Agent: dashi.Agent{Model: dashitest.Model},
		Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	tv := out.Tools["m1"]
	if tv.Outcome == nil || tv.Outcome.State == tool.StateSucceeded {
		t.Fatalf("outcome=%+v", tv.Outcome)
	}
	text := ""
	for _, b := range tv.Result().Output.Content {
		if b.Text != nil {
			text += b.Text.Text
		}
	}
	if !strings.Contains(text, "runs only in the turns of task "+task.id) ||
		out.Stop.Kind != dashi.StopFailed {
		t.Fatalf("result=%q stop=%+v", text, out.Stop)
	}
	if len(ts.parentKnown) != 0 {
		t.Fatalf("sent %v", ts.parentKnown)
	}
}

// staticTools is a fixed toolset.
type staticTools []tool.Tool

func (s staticTools) Tools(context.Context) ([]tool.Tool, error) { return s, nil }

// Without Config.Session and Config.OnTask, nothing can deliver a message
// to the parent: a child does not get message_parent, and a call that
// arrives anyway is a tool error that sends nothing, never a success.
func TestMessageParentWithoutARouteIsAToolError(t *testing.T) {
	ctx := t.Context()
	ts := NewTasks(Config{})
	defer ts.Close(ctx)
	task := newTask(Subagent{Name: "scout"}, dashi.CallRef{TurnID: "p", CallID: "c"}, "")
	ts.mu.Lock()
	ts.addLocked(task)
	ts.mu.Unlock()

	offered, err := childTools{ts: ts, taskID: task.id}.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range offered {
		if tl.Definition.Name == MessageParentToolName {
			t.Fatal("a child got message_parent with no route to its parent")
		}
	}

	runner := dashi.Runner{
		Recorder: dashi.NewMemoryRecorder(),
		Client: dashitest.NewClient(
			dashitest.CallTool("m1", MessageParentToolName, `{"message":"hi"}`),
			dashitest.Reply("done"),
		),
		Tools: staticTools{ts.messageParentTool(ctx, task.id)},
	}
	out, err := runner.Run(ctx, dashi.Turn{ID: task.sessionID + "-1"}, dashi.Start{
		Agent: dashi.Agent{Model: dashitest.Model},
		Input: []llm.Message{llm.UserMessage(llm.Text("go"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	tv := out.Tools["m1"]
	if tv.Outcome == nil || tv.Outcome.State == tool.StateSucceeded {
		t.Fatalf("outcome=%+v, want a tool error", tv.Outcome)
	}
	text := ""
	for _, b := range tv.Result().Output.Content {
		if b.Text != nil {
			text += b.Text.Text
		}
	}
	if strings.Contains(text, "Sent to your parent") ||
		!strings.Contains(text, "put it in your final answer") {
		t.Fatalf("result=%q", text)
	}
	if out.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("stop=%+v, want the turn to go on", out.Stop)
	}
	if len(ts.parentKnown) != 0 || ts.reports != 0 {
		t.Fatalf("sent %v, reports %d", ts.parentKnown, ts.reports)
	}
}
