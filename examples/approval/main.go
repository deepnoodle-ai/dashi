// Command approval runs a turn whose deploy tool needs a person's approval.
// The policy asks at the terminal before each deploy call and allows or
// denies it, so the whole turn is one Run; a denial's reason is what the
// model sees as the call's result. A server that cannot wait for a person
// uses the deferred form in examples/durable_approval instead. Set
// ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally overrides
// the catalog's default model.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
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

// askPolicy asks the person at the terminal before every deploy and allows
// other tools. The run waits while the person answers.
type askPolicy struct {
	dashi.AllowLocalPolicy
	answers *bufio.Reader
}

func (p askPolicy) AuthorizeTool(
	ctx context.Context,
	call tool.Call,
	view dashi.TurnView,
) (dashi.ToolDecision, error) {
	if call.Name != "deploy" {
		return p.AllowLocalPolicy.AuthorizeTool(ctx, call, view)
	}
	fmt.Printf("Approve %s %s? [y/N] ", call.Name, call.Args)
	line, err := p.answers.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return dashi.ToolDecision{}, err
	}
	if answer := strings.ToLower(strings.TrimSpace(line)); answer == "y" ||
		answer == "yes" {
		return dashi.ToolDecision{Kind: dashi.DecisionAllow}, nil
	}
	return dashi.ToolDecision{
		Kind:   dashi.DecisionDeny,
		Reason: "The operator declined this deploy. Do not retry it.",
	}, nil
}

func main() {
	log.SetFlags(0)
	client, err := messages.New(messages.Config{})
	if err != nil {
		log.Fatal(err)
	}
	deploy, err := tool.Func(
		"deploy",
		"Deploys a version of the application to an environment.",
		func(_ context.Context, _ tool.Call, in deployArgs) (tool.Outcome, error) {
			text := fmt.Sprintf("Deployed %s to %s.", in.Version, in.Environment)
			fmt.Println(text)
			return tool.Succeeded(text), nil
		},
	)
	if err != nil {
		log.Fatal(err)
	}
	runner := dashi.Runner{
		Agent: dashi.Agent{
			Model:        os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "You are a release manager. Use the deploy tool to deploy.",
		},
		Client: client,
		Tools:  tool.List{deploy},
		Policy: askPolicy{answers: bufio.NewReader(os.Stdin)},
	}
	// If the model asks again after an answer, the policy asks again, all
	// within this one Run.
	out, err := runner.Run(context.Background(), dashi.Turn{}, dashi.Start{
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Deploy v1.4.2 to production.")),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Answer)
}
