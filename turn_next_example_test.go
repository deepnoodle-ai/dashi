package dashi_test

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// A host drives a turn by switching on Next rather than decoding Stop. Here
// the model asks to call a tool that needs approval.
func ExampleTurnView_Next() {
	ctx := context.Background()
	write, _ := tool.Func("write", "Write a file.",
		func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
			return tool.Succeeded("ok"), nil
		})
	runner := dashi.Runner{
		Client: &approvalClient{},
		Tools:  tool.List{write},
		Hooks:  dashi.RequireApproval(),
	}
	out, _ := runner.Run(ctx, dashi.NewTurn("t1"), dashi.Start{
		Agent: dashi.Agent{Model: "fixture"},
		Input: []llm.Message{llm.UserMessage(llm.Text("write it"))},
	})
	for {
		fmt.Println(out.Stop.Kind, "->", out.Next())
		switch out.Next() {
		case dashi.NextResolvePending:
			out, _ = runner.Run(ctx, out.Turn, out.Pending[0].Approve())
		case dashi.NextContinue:
			out, _ = runner.Run(ctx, out.Turn, dashi.Continue{})
		default:
			return
		}
	}
	// Output:
	// waiting -> resolve_pending
	// waiting -> continue
	// completed -> none
}
