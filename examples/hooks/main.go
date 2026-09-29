// Command hooks runs one turn with three hooks and prints a line each time
// one fires: a BeforeModel hook delivers on-call context once, a BeforeTool
// hook denies shell commands that touch prod, and a BeforeFinish hook asks
// the model to continue once if its answer has no "Next step:" line. Set
// ANTHROPIC_API_KEY before running it; ANTHROPIC_MODEL optionally overrides
// the catalog's default model.
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
		return fmt.Errorf("hooks: create client: %w", err)
	}
	// A fake shell: it only reports a canned result.
	shell := tool.MustFunc("run_shell", "Runs a shell command on the ops host.",
		func(_ context.Context, _ tool.Call, in shellArgs) (tool.Outcome, error) {
			fmt.Printf("   run_shell: %s\n", in.Command)
			return tool.Succeeded("Filesystem  Use%\n/dev/sda1   91%"), nil
		})
	if err != nil {
		return fmt.Errorf("hooks: %w", err)
	}

	// BeforeModel runs before every model call, so it delivers the on-call
	// note only when the turn does not hold it yet. The note is recorded in
	// the turn and stays in the model's context from then on.
	onCall := dashi.ModelHook{Name: "on-call", Func: func(
		_ context.Context, p dashi.ModelPlan) (dashi.ModelDecision, error) {
		if _, ok := p.Turn.Context(dashi.ContextRuntime, "on-call"); ok {
			return dashi.ModelDecision{}, nil
		}
		fmt.Println("[before_model] delivering on-call context")
		return dashi.ModelDecision{Deliver: []dashi.ContextItem{{
			Name:   "on-call",
			Origin: dashi.ContextRuntime,
			Message: llm.ReminderMessage(llm.Reminder{
				Name:    "on-call",
				Tier:    llm.ReminderContextual,
				Content: "The on-call engineer this week is Priya (pager: 555-0142).",
			}),
		}}}, nil
	}}

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

	// BeforeFinish: if the answer lacks a next step, ask once more. Deliver
	// alone is a nudge; a Reason would veto the answer instead. The turn
	// records the nudge; naming it lets the hook find it there, so a resumed
	// turn does not nudge again.
	finish := dashi.FinishHook{Name: "next-step", Func: func(
		_ context.Context, p dashi.FinishPlan) (dashi.FinishDecision, error) {
		_, nudged := p.Turn.Context(dashi.ContextRuntime, "next-step")
		if nudged || strings.Contains(p.Answer, "Next step:") {
			fmt.Println("[before_finish] answer accepted")
			return dashi.FinishDecision{}, nil
		}
		fmt.Println("[before_finish] asking the model to continue")
		return dashi.FinishDecision{Deliver: []dashi.ContextItem{{
			Name:   "next-step",
			Origin: dashi.ContextRuntime,
			Message: llm.UserMessage(llm.Text(
				"End with one line that starts with \"Next step:\".",
			)),
		}}}, nil
	}}

	runner := dashi.Runner{
		Client: client,
		Tools:  tool.List{shell},
		Hooks:  dashi.JoinHooks(onCall.Hooks(), guard.Hooks(), finish.Hooks()),
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
	fmt.Printf("\nStop: %s\n%s\n", out.Stop.Kind, out.Answer)
	return nil
}
