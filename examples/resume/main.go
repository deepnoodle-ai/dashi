// Command resume runs one turn across two processes. Its send_email tool
// hands the message to a mail service and returns before delivery is known;
// the result arrives later, as a delivery webhook would bring it. The first
// run starts the turn and exits while it waits. The second run records the
// delivery result and continues the turn to its answer. Both runs share a
// filerecorder, a durable recorder that keeps each turn in a file, which is
// what lets the turn outlive the process.
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

// turnID is fixed so the second process can find the turn. A durable turn
// needs IDs the host chose, because a retry after a crash must reuse them;
// the same holds for each command's CommandID below.
const turnID = "email-1"

type emailArgs struct {
	To      string `json:"to" description:"Recipient email address"`
	Subject string `json:"subject" description:"Email subject"`
	Body    string `json:"body" description:"Email body"`
}

func main() {
	log.SetFlags(0)
	mode := flag.String("mode", "start", "start or resume")
	dir := flag.String(
		"dir",
		filepath.Join(os.TempDir(), "dashi-resume"),
		"directory holding recorded turns",
	)
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
		// Running start again repeats the same command, which returns the
		// recorded outcome instead of starting over. The turn records its
		// agent, so the resume process needs none.
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
			fmt.Printf("Waiting for %s\n", messageID(p))
		}
		fmt.Println("Stop: waiting. Run again with -mode=resume.")
		return nil
	}
	fmt.Printf("Stop: %s\n%s\n", out.Stop.Kind, out.Reply)
	return nil
}

// newRunner builds the same client and tools in both processes. Continue
// runs with the agent the turn recorded at Start.
func newRunner(recorder dashi.Recorder) (dashi.Runner, error) {
	client, err := messages.New(messages.Config{})
	if err != nil {
		return dashi.Runner{}, err
	}
	sendEmail := tool.MustFunc(
		"send_email",
		"Sends an email. Delivery is confirmed later by the mail service.",
		func(_ context.Context, call tool.Call, in emailArgs) (tool.Outcome, error) {
			// A real tool would pass call.ID to the mail service as an
			// idempotency key and use the message ID it returns. Detached
			// leaves the call waiting, holding that ID as its handle.
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
func deliver(
	ctx context.Context,
	runner dashi.Runner,
	recorder dashi.Loader,
) (dashi.RunResult, error) {
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
		fmt.Printf("Webhook: %s delivered\n", messageID(p))
		// Result only records the outcome; nothing runs until Continue.
		out, err := runner.Run(ctx, turn, p.Result(
			tool.Succeeded("Delivered as "+messageID(p)+"."),
		))
		if err != nil {
			return out, err
		}
		turn = out.Turn
	}
	return runner.Run(ctx, turn, dashi.Continue{CommandID: "continue"})
}

// messageID returns the handle a detached call returned: here, the mail
// service's message ID.
func messageID(p dashi.Pending) string {
	if p.Tool != nil && p.Tool.Outcome != nil && p.Tool.Outcome.Handle != nil {
		return p.Tool.Outcome.Handle.ID
	}
	return p.ID
}
