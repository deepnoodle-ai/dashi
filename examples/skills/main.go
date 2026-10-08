// Command skills gives an agent skills and slash commands loaded from
// folders. A skill is a Markdown file of instructions that the model loads
// only when it needs them. A slash command is a saved prompt: here
// "/summarize INC-42" expands into the request. Set ANTHROPIC_API_KEY before
// running it; ANTHROPIC_MODEL optionally overrides the default model. Run it
// from the repository root, or point -dir at this directory.
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
		return errors.New("no summarize command; check -dir")
	}
	// The bundle gives the model a list of the skills and a tool to load
	// one. It is added as tools and as hooks.
	bundle, err := set.Bundle(skill.Options{})
	if err != nil {
		return err
	}
	lookup := tool.MustFunc(
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
	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
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
		return err
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Reply)
	return nil
}
