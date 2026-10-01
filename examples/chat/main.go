// Command chat holds a short conversation on a session.Session: two messages
// with no history code, a tool call, and a call the user denies. It needs
// no API key: a scripted dashitest client stands in for the model, so the
// replies are fixed. With a real client, the session code stays the same.
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
			// Bookings wait for the user; weather lookups just run.
			Hooks: dashi.RequireApproval("book_flight"),
		},
		// The scripted client has no catalog default model.
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
		for _, w := range out.SessionWarnings {
			fmt.Printf("  (%s: %s)\n", w.Code, w.Message)
		}
		if out.Next() == dashi.NextAnswer {
			p := out.Pending[0]
			fmt.Printf("  %s %s? no\n", p.Name, p.Args)
			out, err = s.Answer(ctx, []dashi.Settle{p.Deny("the user declined")})
			if err != nil {
				return err
			}
		}
		fmt.Println(out.Answer)
	}
	st, err := s.Open(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\nsession %s: %d turns, next: %s\n", s.ID, st.Turns, st.Next)
	return nil
}
