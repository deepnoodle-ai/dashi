package feed_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/feed"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

func ExampleTail() {
	store, runner, canceled := exampleCanceledTurn()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for step, err := range feed.Tail(ctx, store, canceled.Turn.ID, 0) {
		if err != nil {
			panic(err)
		}
		if step.Stop != nil && step.Stop.Ended {
			fmt.Println("execution:", step.Stop.Kind)
			_, err := runner.Run(context.Background(), canceled.Turn, dashi.Resolve{
				CommandID: "late-result",
				CallID:    "call-late",
				Outcome:   new(tool.Succeeded("Finished late.")),
			})
			if err != nil {
				panic(err)
			}
		}
		if step.Kind == dashi.StepToolFinished {
			fmt.Println("later record:", step.Kind)
			cancel() // This ends the reader, independently of execution.
		}
	}
	fmt.Println("reader canceled:", ctx.Err() != nil)
	// Output:
	// execution: canceled
	// later record: tool_finished
	// reader canceled: true
}

// The fixture starts an application-run tool, then cancels the agent before
// the application supplies its result. It uses only a scripted model.
func exampleCanceledTurn() (*dashi.MemoryRecorder, dashi.Runner, dashi.RunResult) {
	store := dashi.NewMemoryRecorder()
	runner := dashi.Runner{
		Client:   dashitest.NewClient(dashitest.CallTool("call-late", "remote", "{}")),
		Recorder: store,
		Tools: tool.List{
			{
				Definition: tool.Definition{
					Name:        "remote",
					Description: "Run in the application.",
					Kind:        tool.KindExternal,
					Schema:      json.RawMessage(`{"type":"object"}`),
				},
			},
		},
	}
	first, err := runner.Run(context.Background(), dashi.Turn{ID: "late"}, dashi.Start{
		CommandID: "start",
		Agent: dashi.Agent{
			Provider: dashitest.Provider,
			API:      dashitest.API,
			Model:    dashitest.Model,
		},
		Input: []llm.Message{llm.UserMessage(llm.Text("Run remote."))},
	})
	if err != nil {
		panic(err)
	}
	canceled, err := runner.Run(
		context.Background(), first.Turn, dashi.Cancel{CommandID: "cancel"},
	)
	if err != nil {
		panic(err)
	}
	return store, runner, canceled
}
