package dashi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

type activeExampleClient struct{ *dashitest.Client }

func (c activeExampleClient) Prepare(ctx context.Context, in llm.Input) (llm.Request, error) {
	req, err := c.Client.Prepare(ctx, in)
	if err != nil {
		return llm.Request{}, err
	}
	snapshot := req.Snapshot()
	snapshot.Body, err = json.Marshal(in)
	if err != nil {
		return llm.Request{}, err
	}
	return llm.NewRequest(snapshot)
}

func ExampleActiveCompactionPolicy() {
	effects := 0
	read := tool.MustFunc("read", "Read evidence", func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
		effects++
		return tool.Succeeded(strings.Repeat("evidence ", 1000)), nil
	})
	client := activeExampleClient{dashitest.NewClient(dashitest.CallTool("read-1", "read", "{}"), dashitest.CallTool("read-2", "read", "{}"), dashitest.Reply("<summary>Original objective retained. Both evidence reads finished.</summary>"), dashitest.Reply("Fixed and verified."))}
	runner := dashi.Runner{Client: client, Tools: tool.List{read}}
	out, err := runner.Run(context.Background(), dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{Model: dashitest.Model, Settings: llm.Settings{MaxTokens: 256}, ActiveCompaction: &dashi.ActiveCompactionPolicy{Window: 16000, MaxTokens: 256}},
		Input: []llm.Message{llm.UserMessage(llm.Text("Investigate, fix, and verify the original objective."))},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	raw, _ := dashi.Project(out.Turn)
	view, _ := dashi.ProjectContext(out.Turn)
	fmt.Println("answer:", out.Reply)
	fmt.Println("completed effects:", effects)
	fmt.Println("recorded summaries:", len(out.ContextCompactions))
	fmt.Println("ordinary history messages:", len(raw))
	fmt.Println("model context messages:", len(view))
	// Output:
	// answer: Fixed and verified.
	// completed effects: 2
	// recorded summaries: 1
	// ordinary history messages: 6
	// model context messages: 3
}
