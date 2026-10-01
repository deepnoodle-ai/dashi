// Command example runs one agent turn with the tools of the MCP servers in a
// .mcp.json file. It asks before it starts each server, because a project
// file can run any program. Set ANTHROPIC_API_KEY before running it;
// ANTHROPIC_MODEL optionally overrides the catalog's default model.
//
// To try it without a server of your own, build the test server and list it:
//
//	go build -o /tmp/testserver ./internal/testserver/cmd/testserver
//	echo '{"mcpServers":{"test":{"command":"/tmp/testserver"}}}' > .mcp.json
//	go run ./example "Use the add tool to add 2 and 3."
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/mcp"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	prompt := "List the tools you can use from MCP servers."
	if len(os.Args) > 1 {
		prompt = strings.Join(os.Args[1:], " ")
	}

	// A missing file is normal. Any other problem is a warning: the agent
	// still runs without that file.
	project, err := mcp.LoadProjectFile(".mcp.json")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("warning: %v", err)
	}
	ts, err := mcp.Open(ctx, project, mcp.Options{Approve: ask})
	if err != nil {
		return err
	}
	defer ts.Close()

	for _, s := range ts.Status() {
		fmt.Printf("%-12s %-9s %d tools", s.Name, s.State, s.Tools)
		if s.Err != nil {
			fmt.Printf("  %v", s.Err)
		}
		fmt.Println()
	}

	client, err := messages.New(messages.Config{})
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	// MCP calls pass the same hooks as any tool. This one asks before each.
	approve := dashi.ToolHook{Name: "example-approve", Func: func(
		_ context.Context,
		p dashi.ToolPlan,
	) (dashi.ToolDecision, error) {
		info, ok := ts.Info(p.Call.Name)
		if !ok {
			return dashi.ToolDecision{}, nil
		}
		fmt.Printf("Call %s on %s with %s?\n", info.Remote, info.Server, p.Call.Args)
		if !yes() {
			return dashi.Deny("the user declined the call"), nil
		}
		return dashi.Allow("the user approved the call"), nil
	}}
	runner := dashi.Runner{
		Client: client,
		Tools:  tool.Join(ts),
		Hooks:  approve.Hooks(),
	}
	outcome, err := runner.Run(ctx, dashi.NewTurn("mcp-example"), dashi.Start{
		Agent: dashi.Agent{Model: os.Getenv("ANTHROPIC_MODEL")},
		Input: []llm.Message{llm.UserMessage(llm.Text(prompt))},
	})
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	fmt.Println(outcome.Reply)
	return nil
}

// ask shows the raw entry, so the user sees the exact program and the
// variables it reads.
func ask(
	_ context.Context,
	name string,
	s mcp.Server,
	_ string,
) (bool, error) {
	fmt.Printf("Start server %q from %s?\n  %s %s%s\n",
		name, s.Source, s.Command, strings.Join(s.Args, " "), s.URL)
	return yes(), nil
}

var stdin = bufio.NewReader(os.Stdin)

func yes() bool {
	fmt.Print("[y/N] ")
	line, _ := stdin.ReadString('\n')
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y")
}
