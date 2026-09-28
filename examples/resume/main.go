// Command resume runs one turn across two processes. Its send_email tool
// hands the message to a mail service and returns before delivery is known;
// the result arrives later, as a delivery webhook would bring it. The first
// run starts the turn and exits while it waits. The second run records the
// delivery result and continues the turn to its answer. Both runs share a
// file-backed recorder, which is what lets the turn outlive the process.
//
// Set ANTHROPIC_API_KEY and ANTHROPIC_MODEL, then run:
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
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

type emailArgs struct {
	To      string `json:"to" description:"Recipient email address"`
	Subject string `json:"subject" description:"Email subject"`
	Body    string `json:"body" description:"Email body"`
}

// tools is a fixed tool.Toolset.
type tools []tool.Tool

func (ts tools) Tools(context.Context) ([]tool.Tool, error) {
	return ts, nil
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
	mode := flag.String("mode", "start", "start or resume")
	dir := flag.String(
		"dir",
		filepath.Join(os.TempDir(), "dashi-resume"),
		"directory holding recorded turns",
	)
	turnID := flag.String("turn", "email-1", "turn ID")
	flag.Parse()

	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		return errors.New("resume: set ANTHROPIC_MODEL and ANTHROPIC_API_KEY")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	recorder, err := newFileRecorder(*dir)
	if err != nil {
		return fmt.Errorf("resume: open recorder: %w", err)
	}
	runner, err := newRunner(model, recorder)
	if err != nil {
		return err
	}
	switch *mode {
	case "start":
		return start(ctx, runner, *turnID)
	case "resume":
		return resume(ctx, runner, recorder, *turnID)
	}
	return fmt.Errorf("resume: unknown mode %q", *mode)
}

// newRunner builds the same agent and tools in both processes. A turn
// records its agent and tool definitions, and Continue refuses a turn whose
// agent has changed.
func newRunner(model string, recorder dashi.Recorder) (dashi.Runner, error) {
	client, err := messages.New(messages.Config{})
	if err != nil {
		return dashi.Runner{}, fmt.Errorf("resume: create client: %w", err)
	}
	sendEmail, err := tool.Func(
		"send_email",
		"Sends an email. Delivery is confirmed later by the mail service.",
		func(_ context.Context, call tool.Call, in emailArgs) (tool.Outcome, error) {
			// A real tool would pass call.ID to the mail service as an
			// idempotency key and use the message ID it returns.
			fmt.Printf("Queued email to %s: %q\n", in.To, in.Subject)
			return tool.Outcome{
				State:  tool.StateDetached,
				Handle: &tool.Handle{ID: "msg-" + call.ID},
			}, nil
		},
	)
	if err != nil {
		return dashi.Runner{}, fmt.Errorf("resume: create tool: %w", err)
	}
	return dashi.Runner{
		Agent: dashi.Agent{
			ID:           "ops-bot",
			Revision:     "1",
			Instructions: "You are an ops bot. Use send_email to notify people.",
			Model:        model,
		},
		Client:   client,
		Tools:    tools{sendEmail},
		Recorder: recorder,
		Policy:   dashi.AllowLocalPolicy{},
	}, nil
}

// start runs the turn until it waits for delivery. Running it again repeats
// the same Start command, which returns the recorded outcome instead of
// starting over.
func start(ctx context.Context, runner dashi.Runner, turnID string) error {
	outcome, err := apply(ctx, runner, dashi.NewTurn(turnID), dashi.Start{
		CommandID: "start",
		Input: []llm.Message{
			llm.UserMessage(llm.Text(
				"Email alice@example.com with subject 'Nightly report' " +
					"and body 'All jobs green.'",
			)),
		},
	})
	if err != nil {
		return err
	}
	if outcome.Stop.Kind != dashi.StopWaiting {
		fmt.Printf("Stop: %s\n%s\n", outcome.Stop.Kind, outcome.Turn.AnswerText())
		return nil
	}
	view, err := dashi.Replay(outcome.Turn)
	if err != nil {
		return fmt.Errorf("resume: replay: %w", err)
	}
	for _, id := range outcome.Pending {
		fmt.Printf("Waiting for %s\n", handleID(view.Tools[id]))
	}
	fmt.Println("Stop: waiting. Run again with -mode=resume.")
	return nil
}

// resume plays the delivery webhook: it loads the saved turn, records each
// delivery as the waiting call's result, and continues the turn.
func resume(
	ctx context.Context,
	runner dashi.Runner,
	recorder dashi.Loader,
	turnID string,
) error {
	turn, err := recorder.Load(ctx, turnID)
	if errors.Is(err, dashi.ErrTurnNotFound) {
		return errors.New("resume: no saved turn; run with -mode=start first")
	}
	if err != nil {
		return fmt.Errorf("resume: load turn: %w", err)
	}
	view, err := dashi.Replay(turn)
	if err != nil {
		return fmt.Errorf("resume: replay: %w", err)
	}
	if view.Stop.Kind != dashi.StopWaiting {
		fmt.Printf("Stop: %s\n%s\n", view.Stop.Kind, turn.AnswerText())
		return nil
	}
	for _, id := range view.Pending {
		message := handleID(view.Tools[id])
		fmt.Printf("Webhook: %s delivered\n", message)
		// Settle only records the result; nothing runs until Continue.
		outcome, err := apply(ctx, runner, turn, dashi.Settle{
			CommandID: "delivered-" + message,
			CallID:    id,
			Outcome: &tool.Outcome{
				State:  tool.StateSucceeded,
				Output: new(tool.TextOutput("Delivered as " + message + ".")),
			},
		})
		if err != nil {
			return err
		}
		turn = outcome.Turn
	}
	outcome, err := apply(ctx, runner, turn, dashi.Continue{CommandID: "continue"})
	if err != nil {
		return err
	}
	fmt.Printf("Stop: %s\n%s\n", outcome.Stop.Kind, outcome.Turn.AnswerText())
	return nil
}

// apply runs one command. Run returns an error only for an invalid command;
// a failed recorder write is reported in the outcome, so it is checked too.
func apply(
	ctx context.Context,
	runner dashi.Runner,
	turn dashi.Turn,
	cmd dashi.Command,
) (dashi.Outcome, error) {
	outcome, err := runner.Run(ctx, turn, cmd)
	if err != nil {
		return outcome, fmt.Errorf("resume: run command: %w", err)
	}
	if err := outcome.RecordingErr(); err != nil {
		return outcome, fmt.Errorf("resume: %w", err)
	}
	return outcome, nil
}

// handleID returns the mail service's message ID for a waiting call, or the
// call ID when the call has no handle.
func handleID(call dashi.ToolView) string {
	if call.Outcome != nil && call.Outcome.Handle != nil {
		return call.Outcome.Handle.ID
	}
	return call.Call.ID
}
