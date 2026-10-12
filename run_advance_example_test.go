package dashi_test

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// A host that runs turns as background work saves the Start at admission,
// puts everything that arrives later in the turn's inbox, and lets the
// worker that holds the turn's lease call Advance. Advance derives the
// Continue it runs from the record, so the host keeps no outbox.
func ExampleRunner_Advance() {
	ctx := context.Background()
	write, _ := tool.Func("write", "Write a file.",
		func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
			return tool.Succeeded("ok"), nil
		})
	inbox := dashi.NewMemoryInbox()
	runner := dashi.Runner{
		Client: dashitest.NewClient(
			dashitest.CallTool("c1", "write", "{}"), dashitest.Reply("done")),
		Tools:    tool.List{write},
		Hooks:    dashi.RequireApproval(),
		Recorder: dashi.NewMemoryRecorder(),
		Inbox:    inbox,
	}
	owner := "worker-1"

	// Admission: save the Start without running it.
	start := dashi.Start{
		Agent: dashi.Agent{Model: "fixture"},
		Input: []llm.Message{llm.UserMessage(llm.Text("write it"))},
	}
	runner.Record(ctx, dashi.NewTurn("t1"), start, dashi.WithOwner(owner))

	// The worker: run until the turn must wait.
	out, _ := runner.Advance(ctx, "t1", dashi.WithOwner(owner))
	fmt.Println(out.Stop.Kind, out.Next())

	// Anywhere, any process: a person approves the call.
	inbox.Put("t1", out.Pending[0].Approve())

	// The worker again, woken by the inbox.
	out, _ = runner.Advance(ctx, "t1", dashi.WithOwner(owner))
	fmt.Println(out.Stop.Kind, out.Next(), out.Reply)
	// Output:
	// waiting resolve_pending
	// completed none done
}

// Drive is Advance in a loop that waits for the inbox between passes. It
// returns once the turn has ended, or when ctx ends, as it does when the
// worker loses its lease.
func ExampleRunner_Drive() {
	ctx := context.Background()
	inbox := dashi.NewMemoryInbox()
	rec := dashi.NewMemoryRecorder()
	runner := dashi.Runner{
		Client: dashitest.NewClient(
			dashitest.CallTool("c1", "write", "{}"), dashitest.Reply("done")),
		// An external tool: the host runs it and reports the result.
		Tools: tool.List{{Definition: tool.Definition{
			Name:   "write",
			Schema: []byte(`{"type":"object"}`),
			Kind:   tool.KindExternal,
		}}},
		Recorder: rec,
		Inbox:    inbox,
	}
	start := dashi.Start{
		Agent: dashi.Agent{Model: "fixture"},
		Input: []llm.Message{llm.UserMessage(llm.Text("write it"))},
	}
	runner.Record(ctx, dashi.NewTurn("t1"), start)

	// Another process supplies the external tool's result once the turn
	// waits for it.
	go func() {
		for view := range dashi.FollowStops(ctx, rec, "t1") {
			if view.Next() == dashi.NextResolvePending {
				inbox.Put("t1", view.Pending[0].Result(tool.Succeeded("written")))
				return
			}
		}
	}()

	out, err := runner.Drive(ctx, "t1")
	fmt.Println(out.Stop.Kind, out.Reply, err)
	// Output:
	// completed done <nil>
}

// FollowStops shows a turn's state only at its stops, so a watcher never
// acts on an intermediate state, and ends after the stop that ends the
// turn.
func ExampleFollowStops() {
	ctx := context.Background()
	rec := dashi.NewMemoryRecorder()
	runner := dashi.Runner{
		Client:   dashitest.NewClient(dashitest.Reply("done")),
		Recorder: rec,
	}
	start := dashi.Start{
		Agent: dashi.Agent{Model: "fixture"},
		Input: []llm.Message{llm.UserMessage(llm.Text("hello"))},
	}
	runner.Record(ctx, dashi.NewTurn("t1"), start)
	for view, err := range dashi.FollowStops(ctx, rec, "t1") {
		if err != nil {
			fmt.Println(err)
			return
		}
		fmt.Println(view.LastSeq, view.Next())
		if view.Next() == dashi.NextContinue {
			runner.Advance(ctx, "t1")
		}
	}
	// Output:
	// 1 continue
	// 5 none
}
