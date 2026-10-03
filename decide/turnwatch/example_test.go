package turnwatch_test

import (
	"context"
	"fmt"
	"log"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/turnwatch"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide/decidetest"
)

// An agent calls a failing tool twice with the same arguments. The watcher
// asks Decide, which says the agent is repeating itself, and the next model
// call sees the watcher's reminder. A fake Decide server and a scripted
// model stand in for Jev and an LLM, so no API key is needed.
func ExampleWatcher() {
	srv, err := decidetest.Start()
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	srv.Answer("repeating", decidetest.NoulAnswer(0.94))
	srv.Answer("off_request", decidetest.NoulAnswer(0.08))
	judge, err := srv.NewClient()
	if err != nil {
		log.Fatal(err)
	}

	watcher := &turnwatch.Watcher{Client: judge, OnCheck: func(c turnwatch.Check) {
		fmt.Printf("check: %s -> %s\n", c.Trigger, c.Action)
	}}
	runTests := tool.MustFunc("run_tests", "Runs the tests.",
		func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
			return tool.Failed("FAIL TestParseArrow"), nil
		})
	model := dashitest.NewClient(
		dashitest.CallTool("call-1", "run_tests", `{}`),
		dashitest.CallTool("call-2", "run_tests", `{}`),
		dashitest.Reply("TestParseArrow fails the same way each time; the lexer needs a fix."),
	)
	runner := dashi.Runner{
		Client: model,
		Tools:  tool.List{runTests},
		Hooks:  watcher.Hook().Hooks(),
	}
	out, err := runner.Run(context.Background(), dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{Model: dashitest.Model},
		Input: []llm.Message{llm.UserMessage(llm.Text("Make the parser tests pass."))},
	})
	if err != nil {
		log.Fatal(err)
	}

	item, _ := out.Context(dashi.ContextRuntime, turnwatch.Name)
	fmt.Println("reminder:", item.Message.Content[0].Reminder.Content)
	for _, step := range out.Turn.Steps {
		for _, r := range step.HookResults {
			fmt.Printf("record: %s %s, %s\n", r.Verdict, r.RulesVersion, r.Reason)
		}
	}
	// Output:
	// check: failed_again -> remind
	// reminder: A call you made keeps failing in the same way. Do not repeat it unchanged. Read the error, work out why it fails, and try a different approach.
	// record: change jev-1.13.0, failed_again: run_tests, 2 times; repeating 0.94, off_request 0.08 (request req_1)
}
