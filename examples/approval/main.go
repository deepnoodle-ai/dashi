// Command approval runs a turn whose deploy tool needs a person's approval.
// The policy defers each deploy call, so the turn stops waiting; the terminal
// asks y/n, the answer is recorded, and the turn continues with it. Set
// ANTHROPIC_API_KEY and ANTHROPIC_MODEL before running it.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/tool"
)

// approvalRevision names the rule that deferred a call. An answer echoes it,
// so an approval given under one rule cannot authorize a call under another.
const approvalRevision = "deploy-approval-v1"

type deployArgs struct {
	Environment string `json:"environment" description:"staging or production"`
	Version     string `json:"version" description:"Version tag to deploy"`
}

// approvalPolicy asks a person before every deploy and allows other tools.
type approvalPolicy struct {
	dashi.AllowLocalPolicy
}

func (p approvalPolicy) AuthorizeTool(
	ctx context.Context,
	call tool.Call,
	view dashi.TurnView,
) (dashi.ToolDecision, error) {
	if call.Name == "deploy" {
		return dashi.ToolDecision{
			Kind:     dashi.DecisionDefer,
			Revision: approvalRevision,
		}, nil
	}
	return p.AllowLocalPolicy.AuthorizeTool(ctx, call, view)
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
	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" || os.Getenv("ANTHROPIC_API_KEY") == "" {
		return errors.New("approval: set ANTHROPIC_MODEL and ANTHROPIC_API_KEY")
	}
	// The timeout includes the time a person takes to answer.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	client, err := messages.New(messages.Config{})
	if err != nil {
		return fmt.Errorf("approval: create client: %w", err)
	}
	deploy, err := tool.Func(
		"deploy",
		"Deploys a version of the application to an environment.",
		func(_ context.Context, _ tool.Call, in deployArgs) (tool.Outcome, error) {
			fmt.Printf("Deploying %s to %s.\n", in.Version, in.Environment)
			text := fmt.Sprintf("Deployed %s to %s.", in.Version, in.Environment)
			return tool.Succeeded(text), nil
		},
	)
	if err != nil {
		return fmt.Errorf("approval: create tool: %w", err)
	}
	runner := dashi.Runner{
		Agent: dashi.Agent{
			ID:           "release-manager",
			Revision:     "1",
			Instructions: "You are a release manager. Use the deploy tool to deploy.",
			Model:        model,
		},
		Client:   client,
		Tools:    tool.List{deploy},
		Recorder: dashi.NewMemoryRecorder(),
		Policy:   approvalPolicy{},
	}
	outcome, err := apply(
		ctx,
		runner,
		dashi.NewTurn("deploy-turn"),
		dashi.Start{
			CommandID: "start",
			Input: []llm.Message{
				llm.UserMessage(llm.Text("Deploy v1.4.2 to production.")),
			},
		},
	)
	if err != nil {
		return err
	}

	// The model may ask again after an answer, so repeat until the turn
	// stops for another reason.
	answers := bufio.NewReader(os.Stdin)
	for round := 1; outcome.Stop.Kind == dashi.StopWaiting; round++ {
		view, err := dashi.Replay(outcome.Turn)
		if err != nil {
			return fmt.Errorf("approval: replay: %w", err)
		}
		for _, id := range outcome.Pending {
			call := view.Tools[id]
			if call.Status != dashi.ToolApprovalPending {
				return fmt.Errorf("approval: call %s is %s", id, call.Status)
			}
			approve, err := ask(answers, call.Call)
			if err != nil {
				return err
			}
			// The digest and revision bind the answer to exactly the
			// arguments shown and the rule that asked.
			outcome, err = apply(ctx, runner, outcome.Turn, dashi.Settle{
				CommandID: "answer-" + id,
				CallID:    id,
				Approval: &dashi.ApprovalFact{
					CallID:         id,
					Approve:        approve,
					ArgsDigest:     call.ArgsDigest,
					PolicyRevision: call.Decision.Revision,
				},
			})
			if err != nil {
				return err
			}
			if approve {
				continue
			}
			// A denied call reaches the model only as "not executed", so
			// record why before the model sees it.
			note := "I denied the " + call.Call.Name + " call. Do not retry it."
			outcome, err = apply(ctx, runner, outcome.Turn, dashi.Deliver{
				CommandID: "note-" + id,
				Items: []dashi.ContextItem{
					{
						Origin:  "user",
						Message: llm.UserMessage(llm.Text(note)),
					},
				},
			})
			if err != nil {
				return err
			}
		}
		if !outcome.ReadyToContinue {
			return fmt.Errorf("approval: turn cannot continue: %s", outcome.Stop.Kind)
		}
		// Settle only records the answers; Continue runs the approved call
		// and lets the model respond.
		outcome, err = apply(ctx, runner, outcome.Turn, dashi.Continue{
			CommandID: fmt.Sprintf("continue-%d", round),
		})
		if err != nil {
			return err
		}
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
		return outcome, fmt.Errorf("approval: run command: %w", err)
	}
	if err := outcome.RecordingErr(); err != nil {
		return outcome, fmt.Errorf("approval: %w", err)
	}
	return outcome, nil
}

// ask shows a call and reads y or n. No answer, such as at the end of
// input, denies the call.
func ask(answers *bufio.Reader, call tool.Call) (bool, error) {
	fmt.Printf("Approve %s %s? [y/N] ", call.Name, call.Args)
	line, err := answers.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("approval: read answer: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
