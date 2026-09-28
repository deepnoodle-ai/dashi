// Command live_follow runs one agent turn in a background worker while two
// readers follow its record with dashi.Follow, as two browser tabs would.
// The first follows from the start; the second joins late from a cursor and
// leaves early, which does not affect the turn. Set ANTHROPIC_API_KEY before
// running it; ANTHROPIC_MODEL optionally overrides the catalog's default
// model.
package main

import (
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

// CityInput is the argument schema for check_weather.
type CityInput struct {
	City string `json:"city" description:"City to check"`
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

	weather, err := tool.Func(
		"check_weather",
		"Returns the current weather for a city.",
		func(ctx context.Context, _ tool.Call, in CityInput) (tool.Outcome, error) {
			select { // a slow lookup, so the followers see the turn in progress
			case <-time.After(time.Second):
			case <-ctx.Done():
				return tool.Outcome{}, ctx.Err()
			}
			return tool.Succeeded(in.City + ": 18°C and clear"), nil
		},
	)
	if err != nil {
		return err
	}
	client, err := messages.New(messages.Config{})
	if err != nil {
		return fmt.Errorf("live_follow: create client: %w", err)
	}
	// The store the worker commits to is the one readers follow. A server
	// would use a durable recorder that implements dashi.StepReader.
	store := dashi.NewMemoryRecorder()
	runner := dashi.Runner{
		Agent: dashi.Agent{
			Model:        os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "Use check_weather for each city you are asked about.",
		},
		Client:   client,
		Tools:    tool.List{weather},
		Recorder: store,
	}
	const turnID = "live-follow-demo"
	start := dashi.Start{Input: []llm.Message{llm.UserMessage(llm.Text(
		"What is the weather in Lisbon and in Oslo?",
	))}}

	// Followers get their own context: when the worker stops without
	// ending the turn (a failure, a pause), Follow would keep waiting for
	// the turn to resume, so the program cancels them itself.
	followCtx, stopFollowing := context.WithCancel(ctx)
	defer stopFollowing()
	var wg sync.WaitGroup
	// Tab A follows from the beginning until the turn ends. Once it has
	// read seq 2, tab B opens from that cursor, reads three steps, and
	// closes; its leaving ends only its own follow.
	wg.Go(func() {
		follow(followCtx, store, "A", turnID, 0, 0, func(seq int64) {
			if seq == 2 {
				wg.Go(func() { follow(followCtx, store, "B", turnID, 2, 3, nil) })
			}
		})
	})

	// The worker runs the turn under its own context, not a reader's.
	out, runErr := runner.Run(ctx, dashi.Turn{ID: turnID}, start)
	if runErr != nil || !out.Stop.Terminal {
		// The turn did not end, so the followers would wait for it.
		stopFollowing()
	}
	wg.Wait()
	if runErr != nil {
		return fmt.Errorf("live_follow: %w", runErr)
	}
	if !out.Stop.Terminal {
		return fmt.Errorf(
			"live_follow: the worker stopped without ending the turn: %s %s",
			out.Stop.Kind,
			out.Stop.Reason,
		)
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Answer)
	return nil
}

// follow prints the turn's steps after the cursor, leaving after limit steps
// when limit is positive, and calls onStep with each Seq it reads.
func follow(
	ctx context.Context,
	store dashi.StepReader,
	tab, turnID string,
	after int64,
	limit int,
	onStep func(seq int64),
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	read := 0
	for step, err := range dashi.Follow(ctx, store, turnID, after) {
		if err != nil {
			fmt.Printf("[%s] error: %v\n", tab, err)
			return
		}
		fmt.Printf("[%s] seq %d %s%s\n", tab, step.Seq, step.Kind, detail(step))
		if onStep != nil {
			onStep(step.Seq)
		}
		if read++; limit > 0 && read == limit {
			fmt.Printf("[%s] leaves at seq %d\n", tab, step.Seq)
			return
		}
		if step.Stop != nil && step.Stop.Terminal {
			fmt.Printf("[%s] the turn ended\n", tab)
		}
	}
}

func detail(step dashi.Step) string {
	switch {
	case step.Tool != nil:
		return " " + step.Tool.Call.Name
	case step.Stop != nil:
		return " " + string(step.Stop.Kind)
	}
	return ""
}
