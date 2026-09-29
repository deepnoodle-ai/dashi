// Command compaction shows a chat history shortened by a recorded summary.
// It runs three questions, compacts the history, and asks a follow-up that
// needs a fact from the summarized part. Set OPENAI_API_KEY before running it;
// OPENAI_MODEL optionally overrides the catalog's default model. It makes
// about five paid requests.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
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
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	client, err := responses.New(responses.Config{})
	if err != nil {
		return fmt.Errorf("compaction: create client: %w", err)
	}
	model := os.Getenv("OPENAI_MODEL")
	runner := dashi.Runner{Agent: dashi.Agent{Model: model}, Client: client}

	// The host owns three things: the raw history, the index where each Turn
	// starts, and the newest Record.
	var history []llm.Message
	var starts []int
	var record *compaction.Record
	var last llm.Usage

	ask := func(prompt string) (string, error) {
		// One Project call gives the model view.
		view, err := compaction.Project(history, record)
		if errors.Is(err, compaction.ErrCoverageMismatch) {
			fmt.Println("warning:", err, "(using the raw history)")
			record = nil
		}
		out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
			History: view,
			Input:   []llm.Message{llm.UserMessage(llm.Text(prompt))},
		})
		if err != nil {
			return "", err
		}
		if out.Stop.Kind != dashi.StopCompleted {
			return "", fmt.Errorf("turn stopped %s", out.Stop.Kind)
		}
		messages, err := dashi.Project(out.Turn)
		if err != nil {
			return "", err
		}
		starts = append(starts, len(history))
		history = append(history, messages...)
		last = compaction.LastUsage(out.Turn)
		return out.Answer, nil
	}

	for _, prompt := range []string{
		"My project is called Heron and it deploys on port 8443. " +
			"Give me five tips for running a small web service.",
		"Give me five tips for writing good logs.",
		"Give me five tips for reviewing code.",
	} {
		if _, err := ask(prompt); err != nil {
			return fmt.Errorf("compaction: %w", err)
		}
	}

	// One Compact call shortens the history. A real host calls it when
	// Policy.Due(last) is true. This example asks for it at once, and keeps no
	// recent Turns, so the summary alone stands for the history.
	summarizer := compaction.NewSummarizer(client, compaction.SummarizerConfig{
		Model: model,
	})
	next, err := compaction.Compact(ctx, summarizer, history, record,
		compaction.Options{
			TurnStarts:   starts,
			Last:         last,
			TailFraction: 1e-9,
		})
	switch {
	case errors.Is(err, compaction.ErrNoSavings):
		fmt.Println("The summary is not smaller than the history. Kept as is.")
	case err != nil:
		return fmt.Errorf("compaction: %w", err)
	default:
		record = next
		fmt.Printf(
			"Context shortened: %d messages -> summary "+
				"(about %d -> %d tokens, cost $%v)\n",
			len(history),
			next.TokensBefore,
			next.TokensAfter,
			next.Cost["USD"],
		)
	}

	answer, err := ask("What is my project called, and which port does it use?")
	if err != nil {
		return fmt.Errorf("compaction: %w", err)
	}
	fmt.Println(answer)
	return nil
}
