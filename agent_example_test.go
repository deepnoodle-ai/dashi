package dashi_test

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// One Runner holds the services, and each turn's agent goes on its Start.
// Agent.Tools selects tools by name from Runner.Tools, so two turns on the
// same runner can offer different tools, as a /model command can change one
// setting without rebuilding the runner.
func ExampleAgent_tools() {
	ctx := context.Background()
	noop := func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
		return tool.Succeeded("ok"), nil
	}
	read, _ := tool.Func("read", "Read a file.", noop)
	write, _ := tool.Func("write", "Write a file.", noop)
	runner := dashi.Runner{
		Client: offeredClient{},
		Tools:  tool.List{read, write},
	}
	agent := dashi.Agent{Model: "fixture", Tools: []string{"read"}}
	out, _ := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: agent,
		Input: []llm.Message{llm.UserMessage(llm.Text("look around"))},
	})
	fmt.Println(out.Answer)

	agent.Tools = nil // every tool the runner offers
	out, _ = runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: agent,
		Input: []llm.Message{llm.UserMessage(llm.Text("fix it"))},
	})
	fmt.Println(out.Answer)
	// Output:
	// offered [read]
	// offered [read write]
}

// offeredClient answers with the names of the tools it was offered.
type offeredClient struct{}

func (offeredClient) Provider() string { return "test" }

func (offeredClient) API() string { return "test.chat" }

func (offeredClient) ModelInfo(string) (llm.ModelInfo, bool) {
	return llm.ModelInfo{}, false
}

func (offeredClient) Prepare(_ context.Context, in llm.Input) (llm.Request, error) {
	return llm.NewRequest(llm.RequestSnapshot{
		Origin: content.Origin{
			Provider: "test",
			API:      "test.chat",
			Model:    in.Model,
			Scope:    "fixture",
		},
		Input:     in,
		Effective: in.Settings,
		Body:      []byte(`{}`),
	})
}

func (offeredClient) Send(_ context.Context, call llm.Call) (llm.Result, error) {
	var names []string
	for _, d := range call.Request.Snapshot().Input.Tools {
		names = append(names, d.Name)
	}
	return llm.Result{
		Message:   llm.AssistantMessage(llm.Text(fmt.Sprint("offered ", names))),
		Stop:      llm.Stop{Kind: llm.StopFinished},
		Requested: llm.ModelRef{Provider: "test", Model: "fixture"},
	}, nil
}

func ExampleAgent_NoInheritedSettings() {
	client := dashitest.NewClient(dashitest.Reply("first"), dashitest.Reply("next"))
	session := &dashi.Session{Runner: dashi.Runner{Client: client}, Agent: dashi.Agent{Model: dashitest.Model, Settings: llm.Settings{Temperature: new(0.7), Reasoning: &llm.ReasoningSettings{Effort: "high"}, MaxTokens: 1234}}}
	if _, err := session.Send(context.Background(), "Use these controls."); err != nil {
		panic(err)
	}
	session.Agent = dashi.Agent{NoInheritedSettings: true}
	out, err := session.Send(context.Background(), "Use provider defaults.")
	if err != nil {
		panic(err)
	}
	fmt.Println("temperature omitted:", out.Agent.Settings.Temperature == nil)
	fmt.Println("reasoning omitted:", out.Agent.Settings.Reasoning == nil)
	fmt.Println("default output allowance:", out.Agent.Settings.MaxTokens == 0)
	// Output:
	// temperature omitted: true
	// reasoning omitted: true
	// default output allowance: true
}
