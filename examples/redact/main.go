// Command redact runs a turn whose lookup tool returns a customer record
// holding an API key. An AfterTool hook masks the key in what the model
// sees, so the model never reads it, while the turn's record keeps the raw
// result. To keep a secret out of the record as well, remove it in the
// tool. Set ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally
// overrides the catalog's default model.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"

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
	client, err := messages.New(messages.Config{})
	if err != nil {
		log.Fatal(err)
	}
	lookup, err := tool.Func(
		"lookup_customer",
		"Returns a customer's account record.",
		func(_ context.Context, _ tool.Call, in lookupArgs) (tool.Outcome, error) {
			record := fmt.Sprintf(
				"name: %s\nplan: enterprise\napi_key: sk-live4f9a2c81\nstatus: active",
				in.Customer,
			)
			return tool.Succeeded(record), nil
		},
	)
	if err != nil {
		log.Fatal(err)
	}
	// Returning a replacement every time is fine: one that changes nothing
	// counts as no opinion and is not recorded.
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
		Agent: dashi.Agent{
			Model:        os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "You are a support agent. Quote account records exactly.",
		},
		Client: client,
		Tools:  tool.List{lookup},
		Hooks:  redact.Hooks(),
	}
	out, err := runner.Run(context.Background(), dashi.Turn{}, dashi.Start{
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Show me Ada Lovelace's full account record.")),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Answer)
	for _, v := range out.Tools {
		fmt.Printf("\nRecorded output kept the key: %v\n",
			apiKey.MatchString(v.Outcome.Output.Content[0].Text.Text))
	}
}
