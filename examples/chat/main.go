// Command chat holds a short conversation through a session, which keeps
// the conversation's history between messages for you. One message makes the
// model call a tool, and another asks for a booking that the user turns
// down. It needs no API key: a scripted test client plays the model, so the
// replies are fixed.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

type weatherArgs struct {
	City string `json:"city"`
}

type bookingArgs struct {
	To string `json:"to"`
}

func run(ctx context.Context) error {
	weather := tool.MustFunc(
		"weather",
		"Get the current weather in a city.",
		func(_ context.Context, _ tool.Call, a weatherArgs) (tool.Outcome, error) {
			return tool.Succeeded(a.City + ": 22°C, sunny"), nil
		},
		tool.ReadOnly(),
	)
	book := tool.MustFunc(
		"book_flight",
		"Book a flight.",
		func(_ context.Context, _ tool.Call, a bookingArgs) (tool.Outcome, error) {
			return tool.Succeeded("booked a flight to " + a.To), nil
		},
	)
	// The script is what a model would answer to the messages below.
	client := dashitest.NewClient(
		dashitest.CallTool("call-1", "weather", `{"city":"Tokyo"}`),
		dashitest.Reply("It is 22°C and sunny in Tokyo."),
		dashitest.Reply("That is about 72°F."),
		dashitest.CallTool("call-2", "book_flight", `{"to":"Tokyo"}`),
		dashitest.Reply("OK, I have not booked anything."),
	)
	s := &session.Session{
		Runner: dashi.Runner{
			Client: client,
			Tools:  tool.List{weather, book},
			// A booking waits for the user's answer; a weather lookup runs.
			Hooks: dashi.RequireApproval("book_flight"),
		},
		// The scripted client has no default model, so name its one model.
		Agent: dashi.Agent{Model: dashitest.Model},
	}

	for _, text := range []string{
		"What's the weather in Tokyo?",
		"And in Fahrenheit?",
		"Book me a flight there.",
	} {
		fmt.Println("> " + text)
		out, err := s.Send(ctx, text)
		if err != nil {
			return err
		}
		// The turn pauses at the booking. This user always says no.
		if out.Next() == dashi.NextResolvePending {
			p := out.Pending[0]
			fmt.Printf("  %s %s? no\n", p.Name, p.Args)
			out, err = s.Resolve(ctx, []dashi.Resolve{p.Deny("the user declined")})
			if err != nil {
				return err
			}
		}
		fmt.Println(out.Reply)
	}
	return nil
}
