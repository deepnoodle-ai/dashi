// Command hooks runs one turn with three hooks: functions the runner calls
// at set points in the turn. The first adds a note for the model, the second
// blocks shell commands that touch prod, and the third asks the model to
// keep going if its reply lacks a "Next step:" line. Set ANTHROPIC_API_KEY
// before running it; ANTHROPIC_MODEL optionally overrides the default model.
package main

import (
	"context"
	"fmt"
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

type shellArgs struct {
	Command string `json:"command" description:"Shell command to run"`
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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	client, err := messages.New(messages.Config{})
	if err != nil {
		return err
	}
	// A fake shell: it only reports a canned result.
	shell := tool.MustFunc("run_shell", "Runs a shell command on the ops host.",
		func(_ context.Context, _ tool.Call, in shellArgs) (tool.Outcome, error) {
			fmt.Printf("   run_shell: %s\n", in.Command)
			return tool.Succeeded("Filesystem  Use%\n/dev/sda1   91%"), nil
		})

	// Hook 1, before each model call: SeedContext adds the on-call note
	// once, and it stays in the model's context for the rest of the turn.
	onCall := dashi.SeedContext(dashi.ContextItem{
		Name:   "on-call",
		Origin: dashi.ContextRuntime,
		Message: llm.ReminderMessage(llm.Reminder{
			Name:    "on-call",
			Tier:    llm.ReminderContextual,
			Content: "The on-call engineer this week is Priya (pager: 555-0142).",
		}),
	})

	// Hook 2, before each run_shell call: a plain rule allows or denies it.
	// The model sees the denial's reason as the call's result.
	guard := dashi.OnTool("run_shell",
		func(_ context.Context, _ dashi.ToolPlan, in shellArgs) (dashi.ToolDecision, error) {
			if strings.Contains(in.Command, "prod") {
				fmt.Printf("[before_tool] denied: %s\n", in.Command)
				return dashi.Deny("Commands that touch prod are not allowed from this agent."), nil
			}
			fmt.Printf("[before_tool] allowed: %s\n", in.Command)
			return dashi.Allow("not prod"), nil
		})

	// Hook 3, before the turn finishes: if the reply lacks a next step, add
	// a message asking for one, and the model replies again. HookActed
	// reports whether this hook already acted, so it asks only once.
	finish := dashi.FinishHook{Name: "next-step", Func: func(
		_ context.Context, p dashi.FinishPlan) (dashi.FinishDecision, error) {
		nudged := p.Turn.HookActed(dashi.HookBeforeFinish, "next-step")
		if nudged || strings.Contains(p.Reply, "Next step:") {
			fmt.Println("[before_finish] answer accepted")
			return dashi.FinishDecision{}, nil
		}
		fmt.Println("[before_finish] asking the model to continue")
		return dashi.FinishDecision{AddMessages: []dashi.ContextItem{{
			Origin: dashi.ContextRuntime,
			Message: llm.UserMessage(llm.Text(
				"End with one line that starts with \"Next step:\".",
			)),
		}}}, nil
	}}

	runner := dashi.Runner{
		Client: client,
		Tools:  tool.List{shell},
		Hooks:  dashi.JoinHooks(onCall, guard, finish),
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{
			Model:        os.Getenv("ANTHROPIC_MODEL"),
			Instructions: "You are an ops assistant. Answer in two or three sentences.",
		},
		Input: []llm.Message{llm.UserMessage(llm.Text(
			"Check disk usage with `df -h` on staging, then on prod-db-1.",
		))},
	})
	if err != nil {
		return err
	}
	fmt.Printf("\nStop: %s\n%s\n", out.Stop.Kind, out.Reply)
	return nil
}
