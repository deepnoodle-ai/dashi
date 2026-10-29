package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Tasks.Run delivers mail as the task's own runs do: the explicit run
// reads the steer waiting in the child's inbox, and a queued message it
// leaves unread starts the next turn, so no message is stranded.
func TestTaskRunDeliversAndPassesOnMail(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	recorder := dashi.NewMemoryRecorder()
	ref := dashi.CallRef{TurnID: "parent", CallID: "task-call"}
	sessionID := ChildID(ref.TurnID, ref.CallID)
	child := session.TurnID(sessionID, 1)
	client := dashitest.NewClient(
		dashitest.CallTool("c1", "read", `{}`),
		dashitest.Reply("done"),
		dashitest.Reply("second"),
	)
	runner := dashi.Runner{Recorder: recorder, Client: client, Tools: tool.List{{
		Definition: tool.Definition{Name: "read", Description: "Read", Schema: []byte(`{"type":"object"}`)},
		Executor:   internalSucceeds{},
	}}}
	agent := dashi.Agent{Model: dashitest.Model}
	agent.Limits.ModelCalls = 1
	out, err := runner.Run(ctx, dashi.Turn{ID: child}, dashi.Start{
		Parent: &ref, Agent: agent,
		Input: []llm.Message{llm.UserMessage(llm.Text("look"))},
	})
	if err != nil || out.Stop.Kind != dashi.StopLimited {
		t.Fatalf("stop=%+v err=%v", out.Stop, err)
	}
	ts := NewTasks(Config{})
	defer ts.Close(context.Background())
	rec, err := loadRecord(ctx, recorder, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := ts.register(ctx, Subagent{Name: "worker"}, ref, "look", runner, rec, nil)
	if err != nil {
		t.Fatal(err)
	}
	sender := dashi.CallRef{TurnID: "parent-2", CallID: "send"}
	steer := Message{ID: MessageID("parent-2", "steer"), From: "parent", Text: "also check tests"}
	queued := Message{ID: MessageID("parent-2", "queued"), From: "parent", Text: "then summarize", Delivery: DeliverQueue}
	if _, err := task.inbox.Send(steer.Steer()); err != nil {
		t.Fatal(err)
	}
	if _, err := task.session.EnqueueItem(ctx, queued.Item()); err != nil {
		t.Fatal(err)
	}
	ts.mu.Lock()
	task.mail = append(task.mail, &mail{msg: steer, call: sender}, &mail{msg: queued, call: sender})
	ts.mu.Unlock()

	target := TaskTarget{ID: task.id, TurnID: child, LastSeq: out.LastSeq}
	if _, err := ts.Run(ctx, target, dashi.Continue{CommandID: "raise", ModelCalls: 2}); err != nil {
		t.Fatal(err)
	}
	for {
		idle, err := ts.Wait(ctx)
		if err != nil {
			t.Fatalf("wait: %v; the mail is stranded", err)
		}
		if idle {
			break
		}
	}
	first, err := recorder.Load(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	read := false
	for _, step := range first.Steps {
		for _, item := range step.Items() {
			read = read || item.ID == steer.ID
		}
	}
	if !read {
		t.Fatal("the explicit run did not read the steer in the child's inbox")
	}
	second, err := recorder.Load(ctx, session.TurnID(sessionID, 2))
	if err != nil {
		t.Fatalf("the queued message started no turn: %v", err)
	}
	if id := second.Steps[0].CommandID; id != queued.ID {
		t.Fatalf("turn 2 command ID %q; want the queued message", id)
	}
	ts.mu.Lock()
	left := len(task.mail)
	ts.mu.Unlock()
	if left != 0 {
		t.Fatalf("mail left = %d", left)
	}
}

type internalSucceeds struct{}

func (internalSucceeds) Execute(context.Context, tool.Call) (tool.Outcome, error) {
	return tool.Succeeded("read it"), nil
}

// The task hash of an approval with rules matches the hash the runner
// records, which leaves the rules out, so Tasks.Run sees a retry as one.
func TestCommandHashMatchesRunnerForApprovalRules(t *testing.T) {
	recorder := dashi.NewMemoryRecorder()
	client := dashitest.NewClient(dashitest.CallTool("c1", "read", `{}`))
	runner := dashi.Runner{Recorder: recorder, Client: client, Hooks: dashi.RequireApproval(), Tools: tool.List{{
		Definition: tool.Definition{Name: "read", Description: "Read", Schema: []byte(`{"type":"object"}`)},
		Executor:   internalSucceeds{},
	}}}
	out, err := runner.Run(t.Context(), dashi.Turn{ID: "t1"}, dashi.Start{
		Agent: dashi.Agent{Model: dashitest.Model},
		Input: []llm.Message{llm.UserMessage(llm.Text("look"))},
	})
	if err != nil || len(out.Pending) != 1 {
		t.Fatalf("pending=%+v err=%v", out.Pending, err)
	}
	cmd := out.Pending[0].Approve().WithRules("decide/jev-1", "severe 9% (request r_1)")
	out, err = runner.Run(t.Context(), out.Turn, cmd)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := commandHash(cmd)
	if err != nil {
		t.Fatal(err)
	}
	last := out.Turn.Steps[len(out.Turn.Steps)-1]
	if last.CommandID != cmd.CommandID || last.CommandHash != hash {
		t.Fatalf("recorded %s %s; task hash %s", last.CommandID, last.CommandHash, hash)
	}
	// The hash blanks the rules on a copy, never the caller's decision.
	if cmd.Approval.RulesVersion != "decide/jev-1" || cmd.Approval.RulesFinding != "severe 9% (request r_1)" {
		t.Fatalf("hashing changed the caller's decision: %+v", *cmd.Approval)
	}
}
