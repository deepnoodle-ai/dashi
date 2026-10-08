// Command durable_approval approves tool calls the way a server would. Each
// deploy call needs approval, so the turn stops and waits instead of
// blocking. The program records each answer with its own Run call, then
// continues the turn with another. Because the turn is saved between calls,
// a server could make each call in a different request. Set
// ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally overrides
// the default model.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/examples/internal/prompt"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

type deployArgs struct {
	Environment string `json:"environment" description:"staging or production"`
	Version     string `json:"version" description:"Version tag to deploy"`
}

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

// run sets no timeout, because the turn waits for a person to answer.
func run(ctx context.Context) error {
	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
	}
	deploy := tool.MustFunc(
		"deploy",
		"Deploys a version of the application to an environment.",
		func(_ context.Context, _ tool.Call, in deployArgs) (tool.Outcome, error) {
			text := fmt.Sprintf("Deployed %s to %s.", in.Version, in.Environment)
			fmt.Println(text)
			return tool.Succeeded(text), nil
		},
		tool.WithStrictArguments(),
	)
	runner := dashi.Runner{
		Client: client,
		Tools:  tool.List{deploy},
		// Every deploy call stops the turn until someone answers.
		Hooks: dashi.RequireApproval("deploy"),
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{
			Model:        os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "You are a release manager. Use the deploy tool to deploy.",
		},
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Deploy v1.4.2 to production.")),
		},
	})
	if err != nil {
		return err
	}
	// The model may ask again after a decision, so repeat until the turn
	// stops for another reason.
	stdin := prompt.Stdin()
	for out.Stop.Kind == dashi.StopWaiting {
		for _, p := range out.Pending {
			if p.Kind != dashi.PendingApproval {
				continue // this program answers approvals only
			}
			ok, err := stdin.Confirm(ctx,
				fmt.Sprintf("Approve %s %s?", p.Name, p.Args))
			if err != nil {
				return err
			}
			// Approve and Deny only record the decision; nothing runs yet.
			answer := p.Deny("The operator declined this deploy.")
			if ok {
				answer = p.Approve()
			}
			if out, err = runner.Run(ctx, out.Turn, answer); err != nil {
				return err
			}
		}
		if out, err = runner.Run(ctx, out.Turn, dashi.Continue{}); err != nil {
			return err
		}
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Reply)
	return nil
}
