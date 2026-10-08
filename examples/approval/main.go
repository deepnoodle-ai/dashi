// Command approval runs a turn whose deploy tool needs a person's approval.
// A hook, a function the runner calls at set points in the turn, asks at the
// terminal before each deploy runs. A server that cannot wait at a terminal
// should see examples/durable_approval instead. Set ANTHROPIC_API_KEY before
// running it; ANTHROPIC_MODEL optionally overrides the default model.
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
	stdin := prompt.Stdin()
	// OnTool builds a hook for one tool, with its arguments already decoded.
	// When the hook denies a call, the model sees the reason as the result.
	ask := dashi.OnTool("deploy", func(
		ctx context.Context,
		_ dashi.ToolPlan,
		in deployArgs,
	) (dashi.ToolDecision, error) {
		ok, err := stdin.Confirm(ctx,
			fmt.Sprintf("Deploy %s to %s?", in.Version, in.Environment))
		if err != nil {
			return dashi.ToolDecision{}, err
		}
		if ok {
			return dashi.Allow("operator approved"), nil
		}
		return dashi.Deny("The operator declined this deploy. Do not retry it."), nil
	})
	runner := dashi.Runner{
		Client: client,
		Tools:  tool.List{deploy},
		Hooks:  ask.Hooks(),
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
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Reply)
	return nil
}
