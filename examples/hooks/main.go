// Command hooks runs one turn with three hooks: a BeforeModel hook adds
// on-call context once, a BeforeTool hook denies shell commands that touch
// prod, and a BeforeFinish hook asks the model to continue once if its
// reply has no "Next step:" line. The last two print a line each time they
// fire. Set ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally
// overrides the catalog's default model.
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

	// BeforeModel runs before every model call. SeedContext adds the
	// on-call note only when the turn does not hold it yet; the note is
	// recorded in the turn and stays in the model's context from then on.
	onCall := dashi.SeedContext(dashi.ContextItem{
		Name:   "on-call",
		Origin: dashi.ContextRuntime,
		Message: llm.ReminderMessage(llm.Reminder{
			Name:    "on-call",
			Tier:    llm.ReminderContextual,
			Content: "The on-call engineer this week is Priya (pager: 555-0142).",
		}),
	})

	// BeforeTool: a simple rule, no second model. The model sees the reason.
	guard := dashi.OnTool("run_shell",
		func(_ context.Context, _ dashi.ToolPlan, in shellArgs) (dashi.ToolDecision, error) {
			if strings.Contains(in.Command, "prod") {
				fmt.Printf("[before_tool] denied: %s\n", in.Command)
				return dashi.Deny("Commands that touch prod are not allowed from this agent."), nil
			}
			fmt.Printf("[before_tool] allowed: %s\n", in.Command)
			return dashi.Allow("not prod"), nil
		})

	// BeforeFinish: if the reply lacks a next step, ask once more. AddMessages
	// alone is a nudge; a Reason would veto the reply instead. The turn
	// records the hook's decision, and HookActed reads it there, so a resumed
	// turn does not nudge again.
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
