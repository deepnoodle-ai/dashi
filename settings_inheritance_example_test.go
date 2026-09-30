package dashi_test

import (
	"context"
	"fmt"
	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
)

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
