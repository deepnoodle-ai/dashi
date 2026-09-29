// Command live_follow runs one agent turn while two readers follow it with
// feed.Follow, as two browser tabs would. Tab A follows from the start and
// also shows the model's text as it streams, through a feed.Hub set as the
// runner's Observer. Tab B follows steps only: it joins late from a cursor
// and leaves early, which does not affect the turn. Set ANTHROPIC_API_KEY
// before running it; ANTHROPIC_MODEL optionally overrides the catalog's
// default model.
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
	"github.com/deepnoodle-ai/dashi/feed"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

const turnID = "live-follow-demo"

type cityArgs struct {
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

	// A slow tool, so the readers see the turn in progress.
	weather := tool.MustFunc(
		"check_weather",
		"Returns the current weather for a city.",
		func(ctx context.Context, _ tool.Call, in cityArgs) (tool.Outcome, error) {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return tool.Outcome{}, ctx.Err()
			}
			return tool.Succeeded(in.City + ": 18°C and clear"), nil
		},
	)
	client, err := messages.New(messages.Config{})
	if err != nil {
		return fmt.Errorf("live_follow: create client: %w", err)
	}
	// Readers follow the store the runner commits steps to; a server would
	// use a durable recorder that implements dashi.StepReader. The hub
	// carries the live text previews, which are never recorded.
	store := dashi.NewMemoryRecorder()
	var hub feed.Hub
	runner := dashi.Runner{
		Client:   client,
		Tools:    tool.List{weather},
		Recorder: store,
		Observer: hub.Observe,
	}

	// Follow waits for a turn that stopped without ending to resume, so the
	// readers get their own context, which run cancels in that case.
	followCtx, stopFollowing := context.WithCancel(ctx)
	defer stopFollowing()
	var wg sync.WaitGroup
	// Tab A follows until the turn ends. Once it has read seq 2, tab B opens
	// from that cursor, reads three steps, and leaves.
	wg.Go(func() {
		follow(followCtx, &hub, store, "A", 0, 0, func(seq int64) {
			if seq == 2 {
				wg.Go(func() {
					follow(followCtx, nil, store, "B", 2, 3, nil)
				})
			}
		})
	})

	out, err := runner.Run(ctx, dashi.Turn{ID: turnID}, dashi.Start{
		Agent: dashi.Agent{
			Model:        os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "Use check_weather for each city you are asked about.",
		},
		Input: []llm.Message{
			llm.UserMessage(llm.Text("What is the weather in Lisbon and in Oslo?")),
		},
	})
	if err != nil || !out.Stop.Terminal {
		stopFollowing()
	}
	wg.Wait()
	if err != nil {
		return fmt.Errorf("live_follow: run: %w", err)
	}
	// Tab A has already shown the answer as it streamed.
	fmt.Printf("Stop: %s\n", out.Stop.Kind)
	return nil
}

// follow prints the turn's steps after the cursor and, given a hub, the
// model's text as it streams. It leaves after limit steps when limit is
// positive, and calls onStep with each Seq it reads.
func follow(
	ctx context.Context,
	hub *feed.Hub,
	store dashi.StepReader,
	tab string,
	after int64,
	limit int,
	onStep func(seq int64),
) {
	read := 0
	streaming := false
	for e, err := range feed.Follow(ctx, hub, store, turnID, after) {
		if err != nil {
			fmt.Printf("[%s] error: %v\n", tab, err)
			return
		}
		switch {
		case e.Gap:
			fmt.Print("[…]") // lost deltas; the settled message has the text
			continue
		case e.Delta != nil:
			if e.Delta.Kind != llm.DeltaText {
				continue // show text only; hide reasoning and tool arguments
			}
			if !streaming {
				fmt.Printf("[%s] streaming: ", tab)
				streaming = true
			}
			fmt.Print(e.Delta.Text)
			continue
		case e.Step == nil:
			continue // tool progress
		}
		if streaming {
			fmt.Println()
			streaming = false
		}
		step := *e.Step
		fmt.Printf("[%s] seq %d %s%s\n", tab, step.Seq, step.Kind, detail(step))
		if onStep != nil {
			onStep(step.Seq)
		}
		if read++; limit > 0 && read == limit {
			fmt.Printf("[%s] leaves at seq %d\n", tab, step.Seq)
			return
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
