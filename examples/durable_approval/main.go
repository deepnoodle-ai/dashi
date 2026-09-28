// Command durable_approval approves tool calls the way a server does. The
// policy defers each deploy call, so the turn stops waiting; each answer is
// recorded with its own Run, and Continue resumes the turn. With a durable
// recorder, each of those Runs could happen in a different request or
// process, given IDs the host chose and saved. Set ANTHROPIC_API_KEY before
// running it; ANTHROPIC_MODEL optionally overrides the catalog's default
// model.
package main

import (
	"bufio"
	"context"
	"fmt"
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

func main() {
	log.SetFlags(0)
	ctx := context.Background()
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
		Policy: dashi.RequireApproval("deploy"),
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Input: []llm.Message{
			llm.UserMessage(llm.Text("Deploy v1.4.2 to production.")),
		},
	})
	// The model may ask again after an answer, so repeat until the turn
	// stops for another reason.
	answers := bufio.NewReader(os.Stdin)
	for err == nil && out.Stop.Kind == dashi.StopWaiting {
		for _, p := range out.Pending {
			if p.Kind != dashi.PendingApproval {
				continue // Continue reports anything left unanswered.
			}
			fmt.Printf("Approve %s %s? [y/N] ", p.Name, p.Args)
			line, _ := answers.ReadString('\n')
			answer := p.Deny("The operator declined this deploy.")
			if strings.TrimSpace(strings.ToLower(line)) == "y" {
				answer = p.Approve()
			}
			if out, err = runner.Run(ctx, out.Turn, answer); err != nil {
				log.Fatal(err)
			}
		}
		out, err = runner.Run(ctx, out.Turn, dashi.Continue{})
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Answer)
}
