// Command bigresult runs a turn whose tool returns a very long log. Dashi
// shortens what the model sees to a size limit, while the turn's record
// keeps the full log. Set ANTHROPIC_API_KEY before running it;
// ANTHROPIC_MODEL optionally overrides the default model.
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
		return err
	}
	fetch := tool.MustFunc(
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
	runner := dashi.Runner{Client: client, Tools: tool.List{fetch}}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{
			Model:        os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "You are an operations agent. Report why the service failed.",
			// The longest tool result the model sees. The default is 64 KiB.
			Limits: dashi.Limits{ToolResultBytes: 16 << 10},
		},
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Why did the billing service fail?")),
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Reply)
	// Shortened reports how much of each result the model was shown. The
	// full result stays in the record, in v.Outcome.
	for _, v := range out.Tools {
		if s := v.Shortened(); s != nil {
			fmt.Printf("\nThe tool returned %d bytes; the model saw %d.\n",
				s.OriginalBytes, s.ShownBytes)
		}
	}
	return nil
}
