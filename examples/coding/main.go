// Command coding runs a coding agent in one directory. The agent reads and
// edits files with the workspace tools. Each write, edit, and shell command
// waits for your answer at the terminal. Set OPENAI_API_KEY before running
// it; OPENAI_MODEL optionally overrides the catalog's default model.
//
//	go run ./examples/coding -dir /path/to/repo "Fix the failing test."
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/examples/internal/prompt"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/workspace"
)

func main() {
	log.SetFlags(0)
	dir := flag.String("dir", ".", "workspace directory")
	flag.Parse()
	if flag.NArg() == 0 {
		log.Fatal("usage: coding [-dir DIR] TASK")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *dir, strings.Join(flag.Args(), " ")); err != nil {
		log.Fatal(err)
	}
}

// run sets no timeout, because the turn waits for a person to answer.
func run(ctx context.Context, dir, task string) error {
	stdin := prompt.Stdin()
	ws, err := workspace.New(dir)
	if err != nil {
		return err
	}
	// WithAsker adds the ask_user tool, so the model can ask you a question.
	tools := ws.Tools(workspace.WithAsker(
		func(ctx context.Context, q workspace.Question) (workspace.Answer, error) {
			fmt.Printf("\n? %s\n", q.Prompt)
			for _, c := range q.Choices {
				fmt.Printf("  - %s: %s\n", c.Label, c.Description)
			}
			line, err := stdin.Ask(ctx, "> ")
			if err != nil {
				return workspace.Answer{}, err
			}
			return workspace.Answer{Text: line}, nil
		},
	))
	// Reads run without a prompt. Writes and shell commands ask. Answering
	// "a" allows the same kind of call again while the program runs.
	policy := ws.Policy(func(
		ctx context.Context,
		r workspace.ApprovalRequest,
	) (workspace.Approval, error) {
		line, err := stdin.Ask(ctx, "\n"+describe(r)+"Allow? [y/N/a] ")
		if err != nil {
			return workspace.Approval{}, err
		}
		switch strings.ToLower(line) {
		case "y":
			return workspace.Approval{Allow: true}, nil
		case "a":
			return workspace.Approval{Allow: true, Remember: true}, nil
		}
		return workspace.Approval{}, nil
	})
	client, err := responses.New(responses.Config{})
	if err != nil {
		return err
	}
	runner := dashi.Runner{
		Client: client,
		Tools:  tools,
		Hooks:  policy.Hooks(),
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{
			Model: os.Getenv("OPENAI_MODEL"),
			Instructions: "You are a coding agent. Work in the workspace " +
				"with the tools. Read code before you change it. Run the " +
				"tests to check your work.",
		},
		Input: []llm.Message{llm.UserMessage(llm.Text(task))},
	})
	if err != nil {
		return err
	}
	fmt.Printf("\nStop: %s\n%s\n", out.Stop.Kind, out.Answer)
	return nil
}

// describe renders an approval request for the terminal. A shell command
// appears in full, with its directory, because approval is the only guard.
// The request text comes from the model, so it goes through Printable first.
// Raw, it could hold escape sequences that rewrite the prompt.
func describe(r workspace.ApprovalRequest) string {
	r = r.Printable()
	if r.Tool == workspace.ToolBash {
		return fmt.Sprintf("Run in %s:\n%s\n", r.Dir, r.Command)
	}
	return r.Summary + "\n" + r.Preview + "\n"
}
