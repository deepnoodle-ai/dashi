package dashi_test

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

// A host saves a turn without running it, before any paid request. Record
// needs no Client when the agent names its provider, API, and model. A later
// Continue runs the turn; a Cancel ends it.
func ExampleRunner_Record() {
	ctx := context.Background()
	rec := dashi.NewMemoryRecorder()
	saver := dashi.Runner{Recorder: rec}
	start := dashi.Start{
		Agent: dashi.Agent{
			Provider: dashitest.Provider,
			API:      dashitest.API,
			Model:    "fixture",
		},
		Input: []llm.Message{llm.UserMessage(llm.Text("hello"))},
	}
	out, _ := saver.Record(ctx, dashi.Turn{}, start)
	fmt.Println("steps:", len(out.Turn.Steps), "next:", out.Next())

	client := dashitest.NewClient(dashitest.Reply("done"))
	runner := dashi.Runner{Client: client, Recorder: rec}
	out, _ = runner.Run(ctx, out.Turn, dashi.Continue{})
	fmt.Println(out.Stop.Kind, "->", out.Next(), "resumes:", out.Resumes)

	// Withdrawing a saved turn needs only the recorder.
	out, _ = saver.Record(ctx, dashi.Turn{}, start)
	out, _ = saver.Run(ctx, out.Turn, dashi.Cancel{Reason: "withdrawn"})
	fmt.Println(out.Stop.Kind, "->", out.Next())
	// Output:
	// steps: 1 next: continue
	// completed -> none resumes: 0
	// canceled -> none
}
