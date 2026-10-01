package dashi_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// SeedContext adds a note before the first model call and not again,
// because the turn record already holds it. The turn here makes two model
// calls, one for a tool call and one for the reply.
func ExampleSeedContext() {
	ctx := context.Background()
	onCall := dashi.SeedContext(dashi.ContextItem{
		Name:   "on-call",
		Origin: dashi.ContextRuntime,
		Message: llm.ReminderMessage(llm.Reminder{
			Name:    "on-call",
			Tier:    llm.ReminderContextual,
			Content: "The on-call engineer this week is Priya.",
		}),
	})
	page := tool.MustFunc("page", "Pages the on-call engineer.",
		func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
			return tool.Succeeded("paged"), nil
		})
	client := dashitest.NewClient(
		dashitest.CallTool("call_1", "page", `{}`),
		dashitest.Reply("Priya has been paged."),
	)
	runner := dashi.Runner{
		Client: client,
		Tools:  tool.List{page},
		Hooks:  dashi.JoinHooks(onCall),
	}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{Model: dashitest.Model},
		Input: []llm.Message{llm.UserMessage(llm.Text("Page on-call."))},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	delivered := 0
	for _, step := range out.Turn.Steps {
		if step.Context == nil {
			continue
		}
		for _, item := range step.Context.Items {
			if item.Name == "on-call" {
				delivered++
			}
		}
	}
	fmt.Println("model calls:", len(client.Inputs()))
	fmt.Println("deliveries:", delivered)
	// Output:
	// model calls: 2
	// deliveries: 1
}

// A BeforeFinish hook asks for a next step at most once per turn. It keeps
// no state of its own: HookActed reads its earlier answer from the turn
// record, so a turn resumed in another process does not ask again.
func ExampleTurnView_HookActed() {
	ctx := context.Background()
	nextStep := dashi.FinishHook{Name: "next-step", Func: func(
		_ context.Context, p dashi.FinishPlan) (dashi.FinishDecision, error) {
		if p.Turn.HookActed(dashi.HookBeforeFinish, "next-step") ||
			strings.Contains(p.Reply, "Next step:") {
			fmt.Println("accepted:", p.Reply)
			return dashi.FinishDecision{}, nil
		}
		fmt.Println("nudged:", p.Reply)
		return dashi.FinishDecision{AddMessages: []dashi.ContextItem{{
			Name:    "next-step",
			Origin:  dashi.ContextRuntime,
			Message: llm.UserMessage(llm.Text("End with a next step.")),
		}}}, nil
	}}
	runner := dashi.Runner{
		Client: dashitest.NewClient(
			dashitest.Reply("Disk is 91% full."),
			dashitest.Reply("Disk is 91% full. Rotate the logs."),
		),
		Hooks: dashi.JoinHooks(nextStep),
	}
	_, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{Model: dashitest.Model},
		Input: []llm.Message{llm.UserMessage(llm.Text("Check the disk."))},
	})
	if err != nil {
		fmt.Println(err)
	}
	// Output:
	// nudged: Disk is 91% full.
	// accepted: Disk is 91% full. Rotate the logs.
}
