package dashi_test

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/tool"
)

// A first program: one handle, two messages, and no history code. The
// second turn is sent the first turn's messages. The scripted client needs
// no API key; with a real client, Agent may stay empty, since the catalog
// names each provider's default model.
func ExampleSession() {
	ctx := context.Background()
	client := dashitest.NewClient(
		dashitest.Reply("It is 22°C and sunny in Tokyo."),
		dashitest.Reply("That is about 72°F."),
	)
	s := &dashi.Session{
		Runner: dashi.Runner{Client: client},
		Agent:  dashi.Agent{Model: dashitest.Model},
	}
	out, err := s.Send(ctx, "What's the weather in Tokyo?")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(out.Answer)
	fmt.Println("warning:", out.SessionWarnings[0].Code)

	out, err = s.Send(ctx, "And in Fahrenheit?")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(out.Answer)
	fmt.Println("turn 2:", out.Turn.ID == dashi.TurnID(s.ID, 2))
	fmt.Println("messages sent:", len(client.Inputs()[1].Messages))
	// Output:
	// It is 22°C and sunny in Tokyo.
	// warning: memory_only
	// That is about 72°F.
	// turn 2: true
	// messages sent: 3
}

// Steer reaches the running turn at its next model call. Here a tool
// stands in for a person typing while the agent works.
func ExampleSession_Steer() {
	ctx := context.Background()
	client := dashitest.NewClient(
		dashitest.CallTool("c1", "deploy", `{}`),
		dashitest.Reply("Deployed to staging."),
	)
	s := &dashi.Session{
		Runner: dashi.Runner{Client: client},
		Agent:  dashi.Agent{Model: dashitest.Model},
	}
	deploy, _ := tool.Func("deploy", "Deploy the app.",
		func(ctx context.Context, _ tool.Call, _ struct{}) (tool.Outcome, error) {
			q, err := s.Steer(ctx, "Use the staging database.")
			if err != nil {
				return tool.Outcome{}, err
			}
			fmt.Println("steer reached turn 1:", q.TurnID == dashi.TurnID(s.ID, 1))
			return tool.Succeeded("deployed"), nil
		})
	s.Runner.Tools = tool.List{deploy}

	out, err := s.Send(ctx, "Deploy the app.")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(out.Answer)
	last := client.Inputs()[1].Messages
	fmt.Println("model saw:", last[len(last)-1].Text())
	// Output:
	// steer reached turn 1: true
	// Deployed to staging.
	// model saw: Use the staging database.
}

// Enqueue holds input for the next turn. When the running turn ends, the
// same Send starts a queue turn from it and returns that turn's outcome.
func ExampleSession_Enqueue() {
	ctx := context.Background()
	client := dashitest.NewClient(
		dashitest.CallTool("c1", "build", `{}`),
		dashitest.Reply("Built."),
		dashitest.Reply("All tests pass."),
	)
	s := &dashi.Session{
		Runner: dashi.Runner{Client: client},
		Agent:  dashi.Agent{Model: dashitest.Model},
	}
	build, _ := tool.Func("build", "Build the app.",
		func(ctx context.Context, _ tool.Call, _ struct{}) (tool.Outcome, error) {
			q, err := s.Enqueue(ctx, "Now run the tests.")
			if err != nil {
				return tool.Outcome{}, err
			}
			fmt.Println("queued for the next turn:", q.TurnID == "")
			return tool.Succeeded("built"), nil
		})
	s.Runner.Tools = tool.List{build}

	out, err := s.Send(ctx, "Build the app.")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(out.Answer)
	fmt.Println("turn 2:", out.Turn.ID == dashi.TurnID(s.ID, 2))
	fmt.Println("input:", out.Turn.Steps[0].Started.Input[0].Text())
	// Output:
	// queued for the next turn: true
	// All tests pass.
	// turn 2: true
	// input: Now run the tests.
}
