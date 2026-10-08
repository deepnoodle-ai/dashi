// Command redact runs a turn whose lookup tool returns a customer record
// holding an API key. A hook masks the key before the model sees the
// result, while the turn's record keeps the original. To keep a secret out
// of the record too, remove it in the tool. Set ANTHROPIC_API_KEY before
// running it; ANTHROPIC_MODEL optionally overrides the default model.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

type lookupArgs struct {
	Customer string `json:"customer" description:"Customer name"`
}

var apiKey = regexp.MustCompile(`sk-[A-Za-z0-9]+`)

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
	lookup := tool.MustFunc(
		"lookup_customer",
		"Returns a customer's account record.",
		func(_ context.Context, _ tool.Call, in lookupArgs) (tool.Outcome, error) {
			return tool.Succeeded(fmt.Sprintf(
				"name: %s\nplan: enterprise\napi_key: sk-live4f9a2c81\nstatus: active",
				in.Customer,
			)), nil
		},
	)
	// A result hook runs after each tool call. Replace is the result the
	// model will see instead, here with any API key masked.
	redact := dashi.ResultHook{Name: "redact", Func: func(
		_ context.Context,
		p dashi.ResultPlan,
	) (dashi.ResultDecision, error) {
		r := p.Result
		r.Output = r.Output.MapText(func(s string) string {
			return apiKey.ReplaceAllString(s, "[redacted]")
		})
		return dashi.ResultDecision{Replace: &r}, nil
	}}
	runner := dashi.Runner{
		Client: client,
		Tools:  tool.List{lookup},
		Hooks:  redact.Hooks(),
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{
			Model:        os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "You are a support agent. Quote account records exactly.",
		},
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Show me Ada Lovelace's full account record.")),
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Reply)
	// Result is what the model saw; Outcome is what the tool returned.
	for _, v := range out.Tools {
		fmt.Printf("\nThe model saw: %q\n", v.Result().Output.Content[0].Text.Text)
		fmt.Printf("The record kept: %q\n", v.Outcome.Output.Content[0].Text.Text)
	}
	return nil
}
