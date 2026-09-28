// Command steering is a small chat CLI in which you can type while the agent
// works. A line typed during a turn is a steer: Dashi delivers it before the
// agent's next model call, or, if the agent was about to answer, asks it
// again with the new message. A line typed too late for the turn starts the
// next one. Set ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL
// optionally overrides the catalog's default model.
//
// Try it interactively, or pipe a question and a correction:
//
//	printf 'Research the tallest mountains.\nOnly those in Europe.\n' |
//	    go run ./examples/steering
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

type searchArgs struct {
	Topic string `json:"topic" description:"What to look up"`
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
	client, err := messages.New(messages.Config{})
	if err != nil {
		return fmt.Errorf("steering: create client: %w", err)
	}
	search, err := tool.Func(
		"slow_search",
		"Looks up a topic. Takes a few seconds.",
		func(ctx context.Context, _ tool.Call, in searchArgs) (tool.Outcome, error) {
			fmt.Printf("  (searching %q...)\n", in.Topic)
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return tool.Outcome{}, ctx.Err()
			}
			return tool.Succeeded("Notes on " + in.Topic + ": see your own " +
				"knowledge; this demo search returns no data."), nil
		},
	)
	if err != nil {
		return fmt.Errorf("steering: %w", err)
	}
	agent := dashi.Agent{
		Model: os.Getenv("ANTHROPIC_MODEL"),
		Instructions: "Answer briefly. Use slow_search once before " +
			"answering. The user may send new messages while you work; " +
			"follow the latest one.",
	}
	// Steering can cost extra model calls, so allow more than the default.
	agent.Limits.ModelAttempts = 12
	runner := dashi.Runner{Agent: agent, Client: client, Tools: tool.List{search}}

	lines := make(chan string)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	inbox := dashi.NewInbox()
	var history, input []llm.Message
	fmt.Println("Ask something. Type while the agent works to steer it.")
	for {
		if len(input) == 0 {
			line, ok := <-lines
			if !ok {
				return nil
			}
			input = []llm.Message{llm.UserMessage(llm.Text(line))}
		}
		// While the turn runs, every line typed is a steer.
		done := make(chan struct{})
		var forwarding sync.WaitGroup
		forwarding.Go(func() {
			for {
				select {
				case line, ok := <-lines:
					if !ok {
						return
					}
					if _, err := inbox.SendText(line); err == nil {
						fmt.Println("  (steer queued)")
					}
				case <-done:
					return
				}
			}
		})
		out, err := runner.Run(
			ctx,
			dashi.Turn{},
			dashi.Start{Input: input, History: history},
			dashi.WithSteering(inbox),
		)
		close(done)
		forwarding.Wait()
		if err != nil {
			return fmt.Errorf("steering: %w", err)
		}
		steers := 0
		for _, step := range out.Turn.Steps {
			steers += len(step.Items())
		}
		fmt.Printf("[%s, %d steer(s) delivered]\n%s\n", out.Stop.Kind, steers, out.Answer)
		if !out.Stop.Terminal {
			// A resumable stop, such as a limit: this CLI gives the turn up.
			fmt.Println("  (turn left unfinished)")
		}
		turn, err := dashi.Project(out.Turn)
		if err != nil {
			return fmt.Errorf("steering: %w", err)
		}
		history = append(history, turn...)
		// Messages the model never saw start the next turn.
		left, err := inbox.Leftovers(out.Turn)
		if err != nil {
			return fmt.Errorf("steering: %w", err)
		}
		input = input[:0]
		for _, steer := range left {
			input = append(input, steer.Item.Message)
		}
	}
}
