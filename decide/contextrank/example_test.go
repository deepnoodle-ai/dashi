package contextrank_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/contextrank"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

// A ranker adds the saved notes that help answer the user's message. A
// decidetest server stands in for the decision model, answering with
// keyword rules, and a scripted client stands in for the agent's model, so
// the example needs no API key.
func ExampleRanker() {
	ctx := context.Background()
	notes := []contextrank.Candidate{
		{ID: "note_seat", Content: "Prefers an aisle seat on flights."},
		{ID: "note_coffee", Content: "Takes coffee black, no sugar."},
		{ID: "note_passport", Content: "Passport expires in March 2027."},
	}
	srv, err := decidetest.Start()
	if err != nil {
		fmt.Println(err)
		return
	}
	defer srv.Close()
	srv.Respond(func(req *decide.Request) (*decide.Response, error) {
		resp := &decide.Response{Answers: map[string]decide.Answer{}}
		for key, q := range req.Questions {
			text, _ := q.(*decide.NoulQuestion).Instructions.(string)
			p := 0.1
			if strings.Contains(text, "seat") || strings.Contains(text, "Passport") {
				p = 0.9
			}
			resp.Answers[key] = decidetest.NoulAnswer(p)
		}
		return resp, nil
	})
	judge, err := srv.NewClient()
	if err != nil {
		fmt.Println(err)
		return
	}

	ranker := &contextrank.Ranker{
		Client: judge,
		Source: func(context.Context, string) ([]contextrank.Candidate, error) {
			return notes, nil // a real source narrows with embeddings or search
		},
		OnSelection: func(sel contextrank.Selection) {
			for _, j := range sel.Judged {
				fmt.Printf("%s %.2f %s\n", j.ID, j.Probability, j.Cause)
			}
		},
	}
	model := dashitest.NewClient(dashitest.Reply("Booked an aisle seat."))
	runner := dashi.Runner{Client: model, Hooks: ranker.Hook().Hooks()}
	out, err := runner.Run(ctx, dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{Model: dashitest.Model},
		Input: []llm.Message{llm.UserMessage(llm.Text("Book my flight to Tokyo"))},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, step := range out.Turn.Steps {
		for _, h := range step.HookResults {
			fmt.Println("recorded:", h.Reason)
		}
	}
	fmt.Println(out.Reply)
	// Output:
	// note_seat 0.90 chosen
	// note_coffee 0.10 below_minimum
	// note_passport 0.90 chosen
	// recorded: chose 2 of 3 (note_seat 0.90, note_passport 0.90); request req_1
	// Booked an aisle seat.
}
