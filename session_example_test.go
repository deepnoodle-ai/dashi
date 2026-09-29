package dashi_test

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
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
