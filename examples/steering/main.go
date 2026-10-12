// Command steering is a small chat CLI in which you can type while the agent
// works. A line typed during a turn is a steer: a message the agent reads
// before its next step. A line starting with ! is urgent and also cuts short
// the reply being written. Set ANTHROPIC_API_KEY before running it;
// ANTHROPIC_MODEL optionally overrides the default model.
//
// Try it interactively, or pipe a question and a correction:
//
//	printf 'Research the tallest mountains.\nOnly those in Europe.\n' |
//	    go run ./examples/steering
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/examples/internal/prompt"
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

// run sets no timeout, because the chat waits for a person to type.
func run(ctx context.Context) error {
	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
	}
	search := tool.MustFunc(
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
	agent := dashi.Agent{
		Model: os.Getenv("ANTHROPIC_MODEL"),
		Instructions: "Answer briefly. Use slow_search once before " +
			"answering. The user may send new messages while you work; " +
			"follow the latest one.",
		// Each steer can cost an extra model call, so allow more calls.
		Limits: dashi.Limits{ModelCalls: 12},
	}
	runner := dashi.Runner{Client: client, Tools: tool.List{search}}

	lines := prompt.Stdin().Lines()

	// The inbox carries steers from the keyboard into the running turn.
	inbox := dashi.NewMemoryInbox()
	var history, input []llm.Message
	fmt.Println("Ask something. Type while the agent works to steer it; " +
		"start a line with ! to interrupt the answer.")
	for {
		if len(input) == 0 {
			select {
			case line, ok := <-lines:
				if !ok {
					return nil
				}
				input = []llm.Message{llm.UserMessage(llm.Text(line))}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// While the turn runs, every line typed is a steer.
		done := make(chan struct{})
		var forwarding sync.WaitGroup
		forwarding.Go(func() { forwardSteers(lines, inbox, done) })
		out, err := runner.Run(
			ctx,
			dashi.Turn{},
			dashi.Start{Agent: agent, Input: input, History: history},
			dashi.WithInbox(inbox),
		)
		close(done)
		forwarding.Wait()
		if err != nil {
			return err
		}
		fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Reply)

		turn, err := dashi.TurnMessagesFull(out.Turn)
		if err != nil {
			return err
		}
		history = append(history, turn...)
		// A steer that arrived too late for this turn starts the next one.
		left, err := inbox.Leftovers(out.Turn)
		if err != nil {
			return err
		}
		input = nil
		for _, steer := range left {
			input = append(input, steer.Item.Message)
		}
	}
}

// forwardSteers sends each line to the inbox until done is closed. A line
// starting with ! is sent as urgent.
func forwardSteers(
	lines <-chan string,
	inbox *dashi.MemoryInbox,
	done <-chan struct{},
) {
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return
			}
			send, note := inbox.SendText, "  (steer queued)"
			if text, ok := strings.CutPrefix(line, "!"); ok {
				line = text
				send, note = inbox.SendUrgent, "  (urgent steer sent)"
			}
			if _, err := send(line); err != nil {
				fmt.Printf("  (steer not sent: %v)\n", err)
			} else {
				fmt.Println(note)
			}
		case <-done:
			return
		}
	}
}
