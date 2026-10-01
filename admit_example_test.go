package dashi_test

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

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
