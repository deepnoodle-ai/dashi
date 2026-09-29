package dashi_test

import (
	"context"
	"fmt"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/skill"
)

// An unchanged seed and skill catalog appear once across Session turns.
func ExampleSeedContext_session() {
	bundle, _ := skill.New([]skill.Definition{{Name: "review", Instructions: "Check the code."}}, skill.Options{})
	client := dashitest.NewClient(dashitest.Reply("one"), dashitest.Reply("two"), dashitest.Reply("three"))
	seed := dashi.SeedContext(dashi.ContextItem{Name: "on-call", Message: llm.UserMessage(llm.Text("Priya is on call."))})
	session := &dashi.Session{Runner: dashi.Runner{Client: client, Hooks: dashi.JoinHooks(seed, bundle)}, Agent: dashi.Agent{Model: dashitest.Model}}
	for range 3 {
		if _, err := session.Send(context.Background(), "Who is on call?"); err != nil {
			fmt.Println(err)
			return
		}
	}
	for i, input := range client.Inputs() {
		seeds, catalogs := 0, 0
		for _, m := range input.Messages {
			if m.Context != nil && m.Context.Name == "on-call" {
				seeds++
			}
			if m.Context != nil && m.Context.Name == "skills" {
				catalogs++
			}
		}
		fmt.Printf("request %d: seed=%d catalog=%d\n", i+1, seeds, catalogs)
	}
	// Output:
	// request 1: seed=1 catalog=1
	// request 2: seed=1 catalog=1
	// request 3: seed=1 catalog=1
}
