package compaction_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

func ExampleValidateReplacement() {
	start, source, reloaded := exampleSavedSummary()
	active := source // activation has not committed
	replacement, err := compaction.ValidateReplacement(start, source, reloaded)
	if err != nil {
		panic(err) // required summarization leaves the next request waiting
	}
	fmt.Println("old history still active:", len(active) == 2)
	// The application checks source revision and commits activation atomically.
	active = replacement.History
	fmt.Println("saved summary reused:", replacement.Summary)
	fmt.Println("active messages:", len(active))
	// Output:
	// old history still active: true
	// saved summary reused: Project: Heron.
	// active messages: 1
}

func exampleSavedSummary() (dashi.Start, []llm.Message, dashi.Turn) {
	ctx := context.Background()
	source := []llm.Message{
		llm.UserMessage(llm.Text("My project is Heron.")),
		llm.AssistantMessage(llm.Text("Noted.")),
	}
	start, err := (&compaction.Compactor{
		Model: dashitest.Model, TailFraction: -1,
	}).Start(
		ctx,
		source,
	)
	if err != nil {
		panic(err)
	}
	start.CommandID = "summary:heron"
	runner := dashi.Runner{
		Client: dashitest.NewClient(dashitest.Reply("<summary>Project: Heron.</summary>")),
	}
	out, err := runner.Run(ctx, dashi.NewTurn("summary-heron"), start)
	if err != nil {
		panic(err)
	}
	// The application's saved job holds start, source, and the bound Turn.
	saved, _ := json.Marshal(out.Turn)
	var reloaded dashi.Turn
	if err := json.Unmarshal(saved, &reloaded); err != nil {
		panic(err)
	}
	return start, source, reloaded
}
