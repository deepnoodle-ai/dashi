package delegate_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// A background task's result reaches the session as a queued notice.
// Whether it arrives before the first turn ends (Send then runs the queue
// turn itself) or after (RunUntilQuiet waits for it), one queue turn reads
// it, and RunUntilQuiet returns once no task is left. The scripted clients
// need no API key.
func ExampleRunUntilQuiet() {
	ctx := context.Background()
	parent := dashitest.NewClient(
		dashitest.CallTool("c1", delegate.ToolName,
			`{"subagent":"explore","prompt":"Who owns billing?","description":"billing owner"}`),
		dashitest.Reply("I started a search; the answer follows."),
		dashitest.Reply("Priya owns the billing service."),
	)
	child := dashitest.NewClient(dashitest.Reply("ownership.md: billing - Priya"))

	s := &dashi.Session{Agent: dashi.Agent{Model: dashitest.Model}}
	tasks := delegate.NewTasks(delegate.Config{
		Subagents: []delegate.Subagent{{
			Name:        "explore",
			Description: "Searches the notes.",
			Agent:       dashi.Agent{Model: dashitest.Model},
		}},
		Runner: dashi.Runner{Client: child},
		OnTask: func(e delegate.TaskEvent) {
			if e.Kind == delegate.TaskEnded {
				s.EnqueueItem(ctx, e.Task.Notice().Item())
			}
		},
	})
	defer tasks.Close(ctx)
	s.Runner = dashi.Runner{Client: parent, Tools: tasks.Tools()}

	if _, err := s.Send(ctx, "Who owns billing?"); err != nil {
		fmt.Println(err)
		return
	}
	out, err := delegate.RunUntilQuiet(ctx, s, tasks)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(out.Answer)
	fmt.Println("turn 2:", out.Turn.ID == dashi.TurnID(s.ID, 2))
	fmt.Println("origin:", out.Origin)
	// Output:
	// Priya owns the billing service.
	// turn 2: true
	// origin: scheduled
}

// A host can withhold a resumable stop, inspect its exact record, and settle
// the external effect without running that effect again. A settlement alone
// does not continue the child; the second explicit command does that.
func ExampleTasks_Run() {
	ctx := context.Background()
	recorder := dashi.NewMemoryRecorder()
	parent := dashitest.NewClient(
		dashitest.CallTool("c1", delegate.ToolName, `{"subagent":"worker","prompt":"Apply the change","description":"change"}`),
		dashitest.Reply("The child is working."),
	)
	child := dashitest.NewClient(dashitest.CallTool("write-1", "external_write", `{}`), dashitest.Reply("The change is verified."))
	var writes, notices atomic.Int32
	tasks := delegate.NewTasks(delegate.Config{
		Subagents: []delegate.Subagent{{Name: "worker", Description: "Applies a change", Agent: dashi.Agent{Model: dashitest.Model}}},
		Runner:    dashi.Runner{Client: child, Recorder: recorder},
		NoticeReady: func(v dashi.TurnView) bool {
			return v.Stop.Terminal || v.Stop.Kind == dashi.StopCompleted || v.Stop.Kind == dashi.StopCanceled
		},
		OnTask: func(e delegate.TaskEvent) {
			if e.Kind == delegate.TaskEnded {
				notices.Add(1)
			}
		},
	})
	defer tasks.Close(ctx)
	external := tool.Tool{
		Definition: tool.Definition{Name: "external_write", Description: "Start an external change", Schema: []byte(`{"type":"object"}`)},
		Executor:   exampleExternalWrite{writes: &writes},
	}
	runner := dashi.Runner{Client: parent, Recorder: recorder, Tools: tool.Join(tool.List{external}, tasks.Tools())}
	if _, err := runner.Run(ctx, dashi.Turn{ID: "parent"}, dashi.Start{Agent: dashi.Agent{Model: dashitest.Model}, Input: []llm.Message{llm.UserMessage(llm.Text("Apply the change"))}}); err != nil {
		fmt.Println(err)
		return
	}
	_, err := tasks.Wait(ctx)
	var attention *delegate.AttentionError
	if !errors.As(err, &attention) {
		fmt.Println(err)
		return
	}
	task := attention.Tasks[0]
	recorded, err := recorder.Load(ctx, task.TurnID)
	if err != nil {
		fmt.Println(err)
		return
	}
	view, err := dashi.Replay(recorded)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("child:", task.State)
	// This action follows external verification by the operator. It does not
	// invent the external worker's uncaptured output.
	command := view.Pending[0].Result(tool.Succeeded("Operator verified this exact call completed; its output was not captured."))
	out, err := tasks.Run(ctx, delegate.TaskTarget{ID: task.ID, TurnID: task.TurnID, Revision: view.Revision}, command)
	if err != nil {
		fmt.Println(err)
		return
	}
	out, err = tasks.Run(ctx, delegate.TaskTarget{ID: task.ID, TurnID: task.TurnID, Revision: out.Revision}, dashi.Continue{CommandID: "finish-verified-child"})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(out.Answer)
	fmt.Println("writes:", writes.Load(), "final notices:", notices.Load())
	// Output:
	// child: waiting
	// The change is verified.
	// writes: 1 final notices: 1
}

type exampleExternalWrite struct{ writes *atomic.Int32 }

func (e exampleExternalWrite) Execute(context.Context, tool.Call) (tool.Outcome, error) {
	e.writes.Add(1)
	return tool.Waiting("external change is in progress"), nil
}
