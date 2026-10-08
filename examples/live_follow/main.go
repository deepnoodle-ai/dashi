// Command live_follow runs one agent turn while a second goroutine follows
// it, as a browser tab would. The follower prints each step as it is
// recorded, plus the model's text as it streams. Set ANTHROPIC_API_KEY
// before running it; ANTHROPIC_MODEL optionally overrides the default model.
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

	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
	}
	weather := tool.MustFunc(
		"check_weather",
		"Returns the current weather for a city.",
		func(_ context.Context, _ tool.Call, in cityArgs) (tool.Outcome, error) {
			time.Sleep(time.Second) // slow, so the turn is visibly in progress
			return tool.Succeeded(in.City + ": 18°C and clear"), nil
		},
	)
	// The runner records each step of the turn in store. The hub passes
	// along the model's text while it streams, which is never recorded.
	store := dashi.NewMemoryRecorder()
	var hub feed.Hub
	runner := dashi.Runner{
		Client:   client,
		Tools:    tool.List{weather},
		Recorder: store,
		Observer: hub.Observe,
	}

	// The follower reads the turn from step 0 until the turn ends.
	followCtx, stopFollowing := context.WithCancel(ctx)
	defer stopFollowing()
	var wg sync.WaitGroup
	wg.Go(func() { follow(followCtx, &hub, store) })

	out, err := runner.Run(ctx, dashi.Turn{ID: turnID}, dashi.Start{
		Agent: dashi.Agent{
			Model:        os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "Use check_weather for each city you are asked about.",
		},
		Input: []llm.Message{
			llm.UserMessage(llm.Text("What is the weather in Lisbon and in Oslo?")),
		},
	})
	// A turn that paused rather than ended could resume later, so the
	// follower would keep waiting. Tell it to stop.
	if err != nil || !out.Stop.Ended {
		stopFollowing()
	}
	wg.Wait()
	if err != nil {
		return err
	}
	fmt.Printf("Stop: %s\n", out.Stop.Kind)
	return nil
}

// follow prints each recorded step of the turn and the model's text as it
// streams.
func follow(ctx context.Context, hub *feed.Hub, store dashi.StepReader) {
	midLine := false
	for e, err := range feed.Follow(ctx, hub, store, turnID, 0) {
		if err != nil {
			fmt.Println("follow error:", err)
			return
		}
		switch {
		case e.Gap:
			// A slow reader can miss pieces; the recorded message is complete.
			fmt.Print("[…some streamed text was dropped…]")
			midLine = true
		case e.Delta != nil && e.Delta.Kind == llm.DeltaText:
			fmt.Print(e.Delta.Text) // a piece of the reply as it streams
			midLine = true
		case e.Step != nil:
			if midLine {
				fmt.Println()
				midLine = false
			}
			fmt.Printf("[step %d] %s", e.Step.Seq, e.Step.Kind)
			if e.Step.Tool != nil {
				fmt.Print(" ", e.Step.Tool.Call.Name)
			}
			fmt.Println()
		}
	}
}
