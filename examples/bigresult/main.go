// Command bigresult runs a turn whose tool returns a long log. Dashi shortens
// the result in the projected history, so the model reads a bounded text with
// a marker, while the turn's record keeps the full log. Set ANTHROPIC_API_KEY
// before running it; ANTHROPIC_MODEL optionally overrides the catalog's
// default model.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

type logArgs struct {
	Service string `json:"service" description:"Service name"`
}

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	client, err := messages.New(messages.Config{})
	if err != nil {
		return fmt.Errorf("bigresult: create client: %w", err)
	}
	fetch, err := tool.Func(
		"fetch_log",
		"Returns the log of a service.",
		func(_ context.Context, _ tool.Call, in logArgs) (tool.Outcome, error) {
			var b strings.Builder
			for i := range 20000 {
				fmt.Fprintf(&b, "%05d %s: request handled\n", i, in.Service)
			}
			b.WriteString("FATAL: out of disk space\n")
			return tool.Succeeded(b.String()), nil
		},
	)
	if err != nil {
		return fmt.Errorf("bigresult: create tool: %w", err)
	}
	runner := dashi.Runner{
		Agent: dashi.Agent{
			Model:        os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "You are an operations agent. Report why the service failed.",
			// The default is 64 KiB. Set a lower limit for a small context.
			Limits: dashi.Limits{ToolResultBytes: 16 << 10},
		},
		Client: client,
		Tools:  tool.List{fetch},
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Why did the billing service fail?")),
		},
	})
	if err != nil {
		return fmt.Errorf("bigresult: run: %w", err)
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Answer)
	// Result is what the model saw; Outcome is what the tool returned.
	for _, v := range out.Tools {
		seen := v.Result().Output.Content[0].Text.Text
		kept := v.Outcome.Output.Content[0].Text.Text
		fmt.Printf("\nThe model saw %d bytes.\n", len(seen))
		fmt.Printf("The record kept %d bytes.\n", len(kept))
		if s := v.Shortened(); s != nil {
			fmt.Printf(
				"Shortened: %d of %d bytes shown, limit %d.\n",
				s.ShownBytes,
				s.OriginalBytes,
				s.Limit,
			)
		}
	}
	return nil
}
