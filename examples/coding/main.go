// Command coding runs a coding agent in one directory. The agent reads and
// edits files with the workspace tools. Each write, edit, and shell command
// waits for your answer at the terminal. Set OPENAI_API_KEY before running
// it; OPENAI_MODEL optionally overrides the catalog's default model.
//
//	go run ./examples/coding -dir /path/to/repo "Fix the failing test."
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
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

func run(ctx context.Context, dir, task string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	answers := bufio.NewReader(os.Stdin)
	ws, err := workspace.New(dir)
	if err != nil {
		return err
	}
	tools := ws.Tools(workspace.WithAsker(
		func(_ context.Context, q workspace.Question) (workspace.Answer, error) {
			fmt.Printf("\n? %s\n", q.Prompt)
			for _, c := range q.Choices {
				fmt.Printf("  - %s: %s\n", c.Label, c.Description)
			}
			fmt.Print("> ")
			line, _ := answers.ReadString('\n')
			return workspace.Answer{Text: strings.TrimSpace(line)}, nil
		},
	))
	// Reads run without a prompt. Writes and shell commands ask. Answering
	// "a" allows the same kind of call again while the program runs.
	policy := ws.Policy(
		func(_ context.Context, r workspace.ApprovalRequest) (workspace.Approval, error) {
			fmt.Printf("\n%s\n%s\nAllow? [y/N/a] ", r.Summary, r.Preview)
			line, _ := answers.ReadString('\n')
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "y":
				return workspace.Approval{Allow: true}, nil
			case "a":
				return workspace.Approval{Allow: true, Remember: true}, nil
			}
			return workspace.Approval{}, nil
		},
	)
	client, err := responses.New(responses.Config{})
	if err != nil {
		return fmt.Errorf("coding: create client: %w", err)
	}
	runner := dashi.Runner{
		Agent: dashi.Agent{
			Model: os.Getenv("OPENAI_MODEL"),
			Instructions: "You are a coding agent. Work in the workspace " +
				"with the tools. Read code before you change it. Run the " +
				"tests to check your work.",
		},
		Client: client,
		Tools:  tools,
		Hooks:  policy.Hooks(),
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Input: []llm.Message{llm.UserMessage(llm.Text(task))},
	})
	if err != nil {
		return fmt.Errorf("coding: %w", err)
	}
	fmt.Printf("\nStop: %s\n%s\n", out.Stop.Kind, out.Answer)
	return nil
}
