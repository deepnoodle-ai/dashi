// Command budget runs a turn under a cost cap too small for even one model
// call. Dashi checks each call's worst-case cost before sending it, so the
// turn stops limited without spending anything; the host then authorizes
// more, and a Continue with a raised Budget finishes the turn. Set
// ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally overrides
// the catalog's default model.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

type stockArgs struct {
	SKU string `json:"sku" description:"Product SKU"`
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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
	}
	stock := tool.MustFunc("check_stock", "Returns units in stock for a SKU.",
		func(_ context.Context, _ tool.Call, in stockArgs) (tool.Outcome, error) {
			return tool.Succeeded("42 units"), nil
		})
	if err != nil {
		return err
	}
	runner := dashi.Runner{Client: client, Tools: tool.List{stock}}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		// A call's worst case counts its full output allowance, so MaxTokens
		// keeps it small enough that the raised budget covers a few calls.
		Agent: dashi.Agent{
			Model:    os.Getenv("ANTHROPIC_MODEL"),
			Settings: llm.Settings{MaxTokens: 1024},
		},
		Input: []llm.Message{llm.UserMessage(llm.Text(
			"How many units of SKU A-100 are in stock? Use the tool.",
		))},
		Budget: &dashi.Budget{MaxCost: usd("0.001")},
	})
	report(out)
	if out.Stop.Kind != dashi.StopLimited || out.Stop.Terminal {
		return nil
	}

	// The application authorizes the increase, then passes the complete new
	// budget. Continue can raise caps but never lower or add them.
	out, err = runner.Run(ctx, out.Turn, dashi.Continue{
		Budget: &dashi.Budget{MaxCost: usd("0.10")},
	})
	if err != nil {
		return err
	}
	report(out)
	fmt.Println(out.Answer)
	return nil
}

func usd(amount string) *dashi.Money {
	return &dashi.Money{Amount: money.MustParseAmount(amount), Currency: "USD"}
}

func report(out dashi.Outcome) {
	fmt.Printf("Stop: %s; model calls: %d; spent: %s USD\n",
		out.Stop.Kind, out.ModelAttempts, out.Spend.Cost["USD"])
	// Limit carries the numbers, so a host need not parse Reason.
	if l := out.Stop.Limit; l != nil && l.Kind == dashi.LimitCost {
		fmt.Printf("  %s\n  cap %s, used %s, next call at risk %s %s\n",
			out.Stop.Reason, l.CostLimit, l.CostUsed, l.CostAtRisk, l.Currency)
	}
}
