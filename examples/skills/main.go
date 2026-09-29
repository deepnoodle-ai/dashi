// Command skills loads skills from a folder and gives them to an agent. Set
// ANTHROPIC_API_KEY before running; ANTHROPIC_MODEL optionally overrides the
// default model. Run it from this directory, or pass a skills folder and a
// commands folder as arguments.
//
// The skill files here are project text, not operator text. The host gives
// the catalog to the model as contextual runtime context (bundle.Hooks) and
// keeps every tool decision in its own code.
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
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/skill"
	"github.com/deepnoodle-ai/dashi/tool"
)

type lookupInput struct {
	Incident string `json:"incident" description:"Incident ID, such as INC-42"`
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

	dirs := []string{"skills", "commands"}
	if len(os.Args) > 1 {
		dirs = os.Args[1:]
	}
	set, warnings := skill.LoadDirs(dirs...)
	for _, w := range warnings {
		log.Printf("skill: %s: %s", w.Path, w.Message)
	}
	bundle, err := set.Bundle(skill.Options{})
	if err != nil {
		return err
	}
	lookup, err := lookupTool()
	if err != nil {
		return err
	}
	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
	}
	runner := dashi.Runner{
		Agent:  dashi.Agent{Model: os.Getenv("ANTHROPIC_MODEL")},
		Client: client,
		Tools:  tool.Join(tool.List{lookup}, bundle),
		Hooks:  bundle.Hooks(),
	}

	// A person typed "/summarize INC-42": the expanded text is the prompt.
	prompt, ok := set.Expand("summarize", "INC-42")
	if !ok {
		prompt = "Use incident-review to summarize INC-42."
	}
	outcome, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Input: []llm.Message{llm.UserMessage(llm.Text(prompt))},
	})
	if err != nil {
		return err
	}
	fmt.Printf("Stop: %s\n%s\n", outcome.Stop.Kind, outcome.Answer)
	return nil
}

func lookupTool() (tool.Tool, error) {
	return tool.Func(
		"incident_lookup",
		"Look up a fictional incident.",
		func(
			_ context.Context,
			_ tool.Call,
			in lookupInput,
		) (tool.Outcome, error) {
			if in.Incident != "INC-42" {
				return tool.Failed("incident not found"), nil
			}
			return tool.Succeeded(
				"INC-42: API requests failed for 12 minutes; service recovered.",
			), nil
		},
	)
}
