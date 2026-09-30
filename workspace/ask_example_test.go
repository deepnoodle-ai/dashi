package workspace_test

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/dashi/workspace"
)

func ExampleWithAsker() {
	ws, err := workspace.New(".")
	if err != nil {
		panic(err)
	}
	tools := ws.Tools(workspace.WithAsker(func(_ context.Context, q workspace.Question) (workspace.Answer, error) {
		// A line UI can translate input 1,2 into these exact labels.
		return workspace.Answer{Selections: []string{q.Choices[0].Label, q.Choices[1].Label}}, nil
	}))
	for _, tl := range tools {
		if tl.Definition.Name != workspace.ToolAskUser {
			continue
		}
		out, err := tl.Executor.Execute(context.Background(), tool.Call{ID: "question", Name: workspace.ToolAskUser,
			Args: []byte(`{"question":"Which?","multiple":true,"choices":[{"label":"a,b"},{"label":"c"}]}`)})
		if err != nil {
			panic(err)
		}
		fmt.Println(out.Output.Content[0].Text.Text)
	}
	// Output:
	// {"selections":["a,b","c"]}
}
