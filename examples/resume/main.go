// Command resume runs one turn across two processes, the way a real email
// tool waits for a delivery webhook. The first run asks the model to send an
// email; the send_email tool hands it off and returns at once, so the turn
// stops waiting and the process exits. The second run plays the webhook: it
// loads the saved turn, records the delivery as the tool's result, and lets
// the model finish its reply. A filerecorder saves the turn to a file, which
// is what lets it outlive the first process.
//
// Set ANTHROPIC_API_KEY (and optionally ANTHROPIC_MODEL), then run:
//
//	go run ./examples/resume -mode=start
//	go run ./examples/resume -mode=resume
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
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

// turnID is fixed so the second process can find the turn. Commands carry
// fixed IDs too, so repeating one returns its recorded result.
const turnID = "email-1"

type emailArgs struct {
	To      string `json:"to" description:"Recipient email address"`
	Subject string `json:"subject" description:"Email subject"`
	Body    string `json:"body" description:"Email body"`
}

func main() {
	log.SetFlags(0)
	mode := flag.String("mode", "start", "start or resume")
	dir := flag.String("dir", filepath.Join(os.TempDir(), "dashi-resume"),
		"directory holding recorded turns")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *mode, *dir); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, mode, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	recorder, err := filerecorder.New(filerecorder.Config{Dir: dir})
	if err != nil {
		return err
	}
	runner, err := newRunner(recorder)
	if err != nil {
		return err
	}
	var out dashi.RunResult
	switch mode {
	case "start":
		// The turn records its agent, so the resume process needs none.
		out, err = runner.Run(ctx, dashi.NewTurn(turnID), dashi.Start{
			CommandID: "start",
			Agent: dashi.Agent{
				ID:           "ops-bot",
				Instructions: "You are an ops bot. Use send_email to notify people.",
				Model:        os.Getenv("ANTHROPIC_MODEL"),
			},
			Input: []llm.Message{
				llm.UserMessage(llm.Text(
					"Email alice@example.com with subject 'Nightly report' " +
						"and body 'All jobs green.'",
				)),
			},
		})
	case "resume":
		out, err = deliver(ctx, runner, recorder)
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	if err != nil {
		return err
	}
	if out.Stop.Kind == dashi.StopWaiting {
		for _, p := range out.Pending {
			fmt.Printf("Waiting for %s\n", p.HandleID())
		}
		fmt.Println("Stop: waiting. Run again with -mode=resume.")
		return nil
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Reply)
	return nil
}

// newRunner builds the same client and tools in both processes.
func newRunner(recorder dashi.Recorder) (dashi.Runner, error) {
	client, err := messages.New(messages.Config{})
	if err != nil {
		return dashi.Runner{}, err
	}
	sendEmail := tool.MustFunc("send_email",
		"Sends an email. Delivery is confirmed later by the mail service.",
		func(_ context.Context, call tool.Call, in emailArgs) (tool.Outcome, error) {
			// Detached says the result comes later. Its handle stands in
			// for the mail service's message ID, which the delivery
			// webhook would quote.
			fmt.Printf("Queued email to %s: %q\n", in.To, in.Subject)
			return tool.Detached("msg-" + call.ID), nil
		},
		tool.WithStrictArguments(),
	)
	return dashi.Runner{
		Client:   client,
		Tools:    tool.List{sendEmail},
		Recorder: recorder,
	}, nil
}

// deliver plays the delivery webhook: it loads the saved turn, records each
// delivery as the waiting call's result, and continues the turn.
func deliver(ctx context.Context, runner dashi.Runner,
	recorder dashi.Loader) (dashi.RunResult, error) {
	turn, err := recorder.Load(ctx, turnID)
	if errors.Is(err, dashi.ErrTurnNotFound) {
		return dashi.RunResult{}, errors.New("no saved turn; run -mode=start first")
	}
	if err != nil {
		return dashi.RunResult{}, err
	}
	view, err := turn.View()
	if err != nil {
		return dashi.RunResult{}, err
	}
	if view.Stop.Kind != dashi.StopWaiting {
		return dashi.RunResult{Turn: turn, TurnView: view}, nil // already done
	}
	for _, p := range view.Pending {
		fmt.Printf("Webhook: %s delivered\n", p.HandleID())
		// Result records the tool's result; the model runs again only on
		// Continue, below.
		delivered := tool.Succeeded("Delivered as " + p.HandleID() + ".")
		out, err := runner.Run(ctx, turn, p.Result(delivered))
		if err != nil {
			return out, err
		}
		turn = out.Turn
	}
	return runner.Run(ctx, turn, dashi.Continue{CommandID: "continue"})
}
