// Command approval runs a turn whose deploy tool needs a person's approval.
// A BeforeTool hook asks at the terminal before each deploy call and allows
// or denies it, so the whole turn is one Run. A server that cannot wait for
// a person uses the deferred form in examples/durable_approval instead. Set
// ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally overrides
// the catalog's default model.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

	"github.com/deepnoodle-ai/dashi"
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
		return fmt.Errorf("approval: create client: %w", err)
	}
	deploy, err := tool.Func(
		"deploy",
		"Deploys a version of the application to an environment.",
		func(_ context.Context, _ tool.Call, in deployArgs) (tool.Outcome, error) {
			text := fmt.Sprintf("Deployed %s to %s.", in.Version, in.Environment)
			fmt.Println(text)
			return tool.Succeeded(text), nil
		},
		tool.WithStrictArguments(),
	)
	if err != nil {
		return fmt.Errorf("approval: create tool: %w", err)
	}
	// OnTool decodes the call's arguments into the tool's own type. A
	// denial's reason is what the model sees as the call's result.
	lines := make(chan string)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	ask := dashi.OnTool("deploy", func(
		ctx context.Context,
		_ dashi.ToolPlan,
		in deployArgs,
	) (dashi.ToolDecision, error) {
		fmt.Printf("Deploy %s to %s? [y/N] ", in.Version, in.Environment)
		line, err := readLine(ctx, lines)
		if err != nil {
			return dashi.ToolDecision{}, err
		}
		if strings.ToLower(strings.TrimSpace(line)) == "y" {
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
		return fmt.Errorf("approval: run: %w", err)
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Answer)
	return nil
}

// readLine returns the next line, "" at end of input, or ctx's error on Ctrl-C.
func readLine(ctx context.Context, lines <-chan string) (string, error) {
	select {
	case line := <-lines:
		return line, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
