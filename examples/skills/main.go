// Command skills loads skills and slash commands from folders and gives them
// to an agent. It expands the command "/summarize INC-42" into the prompt,
// and the model can load the incident-review skill to answer it. Skill text
// reaches the model as context, never as a grant: the host decides which
// tools the agent has. Set
// ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally overrides
// the catalog's default model. Run it from the repository root, or point -dir
// at this directory.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/skill"
	"github.com/deepnoodle-ai/dashi/tool"
)

type lookupArgs struct {
	Incident string `json:"incident" description:"Incident ID, such as INC-42"`
}

func main() {
	log.SetFlags(0)
	dir := flag.String(
		"dir",
		"examples/skills",
		"directory holding the skills and commands folders",
	)
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *dir); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	// A bad skill file is a warning, not an error: the others still load.
	set, warnings := skill.LoadDirs(
		filepath.Join(dir, "skills"),
		filepath.Join(dir, "commands"),
	)
	for _, w := range warnings {
		log.Printf("skills: %s: %s", w.Path, w.Message)
	}
	// A person typed "/summarize INC-42": the expanded command is the prompt.
	prompt, ok := set.Expand("summarize", "INC-42")
	if !ok {
		return errors.New("skills: no summarize command; check -dir")
	}
	// The bundle is both a toolset, holding the tool that loads a skill, and
	// the source of hooks that show the model the skill catalog.
	bundle, err := set.Bundle(skill.Options{})
	if err != nil {
		return fmt.Errorf("skills: bundle: %w", err)
	}
	lookup := lookupTool()
	client, err := messages.New(messages.Config{})
	if err != nil {
		return fmt.Errorf("skills: create client: %w", err)
	}
	runner := dashi.Runner{
		Client: client,
		Tools:  tool.Join(tool.List{lookup}, bundle),
		Hooks:  bundle.Hooks(),
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{Model: os.Getenv("ANTHROPIC_MODEL")},
		Input: []llm.Message{llm.UserMessage(llm.Text(prompt))},
	})
	if err != nil {
		return fmt.Errorf("skills: run: %w", err)
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Answer)
	return nil
}

func lookupTool() tool.Tool {
	return tool.MustFunc(
		"incident_lookup",
		"Look up a fictional incident.",
		func(_ context.Context, _ tool.Call, in lookupArgs) (tool.Outcome, error) {
			if in.Incident != "INC-42" {
				return tool.Failed("incident not found"), nil
			}
			return tool.Succeeded(
				"INC-42: API requests failed for 12 minutes; service recovered.",
			), nil
		},
	)
}
