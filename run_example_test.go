package dashi_test

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// hostOutbox stands in for a host's own command store: every command of one
// turn, saved before it runs.
type hostOutbox struct{ saved [][]byte }

func (o *hostOutbox) save(c dashi.Command) {
	data, _ := dashi.MarshalCommand(c)
	o.saved = append(o.saved, data)
}

// unapplied returns the first saved command that recorded no step: the one a
// restarted worker runs again.
func (o *hostOutbox) unapplied(turn dashi.Turn) dashi.Command {
	applied := map[string]bool{}
	for _, step := range turn.Steps {
		applied[step.CommandID] = true
	}
	for _, data := range o.saved {
		c, _ := dashi.UnmarshalCommand(data)
		if !applied[dashi.CommandIDOf(c)] {
			return c
		}
	}
	return nil
}

// plan maps a stored turn to the host's next command, or nil when there is
// nothing to run now. The CommandID comes from the turn ID and revision, so
// planning again after a crash gives the same command.
func plan(turn dashi.Turn, view dashi.TurnView, history []llm.Message) dashi.Command {
	id := fmt.Sprintf("%s@%d", turn.ID, view.Revision)
	switch view.Next() {
	case dashi.NextContinue, dashi.NextResume: // Resume once the old worker is gone.
		return dashi.Continue{CommandID: id, History: history}
	case dashi.NextAnswer:
		// The host settles each pending item when its answer arrives; here a
		// person approves the call.
		return view.Pending[0].Approve()
	case dashi.NextRaiseBudget, dashi.NextRaiseAttempts:
		// Or Continue with a raised Budget or ModelAttempts.
		return dashi.Cancel{CommandID: id, Reason: "limit reached"}
	case dashi.NextResolve:
		// Host-specific: fix the cause and Continue, or Cancel.
		return dashi.Cancel{CommandID: id, Reason: view.Stop.Reason}
	}
	return nil // NextNone: done.
}

// A host that stores its own turns admits each turn, then loops: load the
// turn, rerun a saved command that never applied, or plan, save, and run
// the next one.
func Example_hostTurnLoop() {
	ctx := context.Background()
	write, _ := tool.Func("write", "Write a file.",
		func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
			return tool.Succeeded("ok"), nil
		})
	rec := dashi.NewMemoryRecorder()
	runner := dashi.Runner{
		Client: dashitest.NewClient(
			dashitest.CallTool("c1", "write", "{}"), dashitest.Reply("done")),
		Tools:    tool.List{write},
		Hooks:    dashi.RequireApproval(),
		Recorder: rec,
	}
	owner := "worker-1"
	var outbox hostOutbox
	var history []llm.Message // Earlier conversation, kept by the host.

	// Admit: save the Start, then record it without a model call.
	start := dashi.Start{
		CommandID: "t1@0",
		Agent:     dashi.Agent{Model: "fixture"},
		Input:     []llm.Message{llm.UserMessage(llm.Text("write it"))},
	}
	outbox.save(start)
	admitted := make(chan struct{})
	close(admitted)
	runner.Run(ctx, dashi.NewTurn("t1"), start,
		dashi.WithOwner(owner), dashi.WithPause(admitted))

	// A worker plans and saves the next command, then crashes before
	// running it.
	turn, _ := rec.Load(ctx, "t1")
	view, _ := dashi.Replay(turn)
	outbox.save(plan(turn, view, history))

	for {
		turn, _ := rec.Load(ctx, "t1")
		cmd := outbox.unapplied(turn)
		if cmd == nil {
			view, _ := dashi.Replay(turn)
			if cmd = plan(turn, view, history); cmd == nil {
				break
			}
			outbox.save(cmd)
		}
		out, err := runner.Run(ctx, turn, cmd, dashi.WithOwner(owner))
		if err != nil {
			fmt.Println(err) // Reload and back off on ErrRevisionConflict.
			return
		}
		fmt.Println(dashi.CommandIDOf(cmd), "->", out.Stop.Kind, out.Next())
	}
	// Output:
	// t1@2 -> waiting answer
	// approval:c1 -> waiting continue
	// t1@8 -> completed none
}

// A host admits a turn without running it: a Start with an already-closed
// pause records the request and stops before any model request. A later
// Continue runs it; a Cancel, which needs no client, withdraws it.
func ExampleWithPause() {
	ctx := context.Background()
	rec := dashi.NewMemoryRecorder()
	paused := make(chan struct{})
	close(paused)
	client := dashitest.NewClient(dashitest.Reply("done"))
	runner := dashi.Runner{Client: client, Recorder: rec}
	start := dashi.Start{
		Agent: dashi.Agent{Model: "fixture"},
		Input: []llm.Message{llm.UserMessage(llm.Text("hello"))},
	}
	out, _ := runner.Run(ctx, dashi.Turn{}, start, dashi.WithPause(paused))
	fmt.Println(out.Stop.Kind, "->", out.Next())
	fmt.Println("model requests:", len(client.Inputs()))

	out, _ = runner.Run(ctx, out.Turn, dashi.Continue{})
	fmt.Println(out.Stop.Kind, "->", out.Next())

	// Withdrawing an admitted turn needs only the recorder.
	out, _ = runner.Run(ctx, dashi.Turn{}, start, dashi.WithPause(paused))
	clientless := dashi.Runner{Recorder: rec}
	out, _ = clientless.Run(ctx, out.Turn, dashi.Cancel{Reason: "withdrawn"})
	fmt.Println(out.Stop.Kind, "->", out.Next())
	// Output:
	// interrupted -> continue
	// model requests: 0
	// completed -> none
	// canceled -> none
}
