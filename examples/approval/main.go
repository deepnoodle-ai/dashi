// Command approval runs a turn whose deploy tool needs a person's approval.
// A BeforeTool hook asks at the terminal before each deploy call and allows
// or denies it, so the whole turn is one Run; a denial's reason is what the
// model sees as the call's result. A server that cannot wait for a person
// uses the deferred form in examples/durable_approval instead. Set
// ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally overrides
// the catalog's default model.
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
	// The hook runs before every deploy call and waits for an answer.
	answers := bufio.NewReader(os.Stdin)
	ask := dashi.OnTool("deploy",
		func(_ context.Context, _ dashi.ToolPlan, in deployArgs) (dashi.ToolDecision, error) {
			fmt.Printf("Deploy %s to %s? [y/N] ", in.Version, in.Environment)
			line, _ := answers.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(line)) == "y" {
				return dashi.Allow("operator approved"), nil
			}
			return dashi.Deny("The operator declined this deploy. Do not retry it."), nil
		})
	runner := dashi.Runner{
		Agent: dashi.Agent{
			Model:        os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "You are a release manager. Use the deploy tool to deploy.",
		},
		Client: client,
		Tools:  tool.List{deploy},
		Hooks:  ask.Hooks(),
	}
	// If the model asks again after an answer, the hook asks again, all
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
