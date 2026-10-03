package listtool_test

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/listtool"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

type Ticket struct{ ID, Text string }

type Contact struct{ ID, Name string }

// The agent finds the urgent billing tickets with judge_items, then the
// customer's contact ID with pick_item. A scripted model plays the agent
// and a decidetest server plays Jev, answering by keywords, so the example
// needs no API key.
func Example() {
	srv, err := decidetest.Start()
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	srv.Respond(keywordAnswers)
	judge, err := srv.NewClient()
	if err != nil {
		log.Fatal(err)
	}

	tickets := listtool.List[Ticket]{
		Name:        "tickets",
		Description: "Open support tickets. The query is ignored.",
		Items: func(context.Context, string) ([]Ticket, error) {
			return []Ticket{
				{"T-1", "How do I export a report?"},
				{"T-2", "Charged twice, refund today please, payroll runs tonight."},
				{"T-3", "Please fix the invoice address for next month."},
			}, nil
		},
		ID:   func(t Ticket) string { return t.ID },
		Text: func(t Ticket) string { return t.Text },
	}
	contacts := listtool.List[Contact]{
		Name:        "contacts",
		Description: "Customer contacts. The query is ignored.",
		Items: func(context.Context, string) ([]Contact, error) {
			return []Contact{{"c_1", "Dana Lee, Acme"}, {"c_2", "Dana Li, Globex"}}, nil
		},
		ID:   func(c Contact) string { return c.ID },
		Text: func(c Contact) string { return c.Name },
	}
	cfg := listtool.Config{Client: judge, Lists: []listtool.AnyList{tickets, contacts}}
	judgeItems, err := listtool.JudgeItems(cfg)
	if err != nil {
		log.Fatal(err)
	}
	pickItem, err := listtool.PickItem(cfg)
	if err != nil {
		log.Fatal(err)
	}

	model := dashitest.NewClient(
		dashitest.CallTool("call-1", "judge_items", `{"list": "tickets",
			"questions": [
				{"key": "billing", "type": "yes_no", "text": "Is this ticket about billing?"},
				{"key": "urgent", "type": "yes_no", "text": "Does it need a reply today?"}],
			"find": {"billing": "yes", "urgent": "yes"}}`),
		dashitest.CallTool("call-2", "pick_item", `{"list": "contacts", "looking_for": "Dana Lee at Acme"}`),
		dashitest.Reply("T-2 is the urgent billing ticket; Dana Lee is c_1."),
	)
	runner := dashi.Runner{Client: model, Tools: tool.List{judgeItems, pickItem}}
	out, err := runner.Run(context.Background(), dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{Model: dashitest.Model},
		Input: []llm.Message{llm.UserMessage(llm.Text("Which billing tickets are urgent? Email Dana at Acme."))},
	})
	if err != nil {
		log.Fatal(err)
	}
	judged := out.Tools["call-1"].Result().Output.Content[0].Text.Text
	_, judged, _ = strings.Cut(judged, "\n") // skip the first line, which has the time taken
	fmt.Println(judged)
	fmt.Println(out.Tools["call-2"].Result().Output.Content[0].Text.Text)
	// Output:
	// Most likely answers: billing yes 2, no 1; urgent yes 1, no 2.
	// Matching billing=yes, urgent=yes: 1 item, best first:
	//   T-2  billing 0.90  urgent 0.90
	// {"status":"ranked","none_probability":0.05,"candidates":[{"id":"c_1","text":"Dana Lee, Acme","probability":0.85},{"id":"c_2","text":"Dana Li, Globex","probability":0.1}]}
}

// keywordAnswers stands in for Jev: P(yes) is 0.9 when the item has a
// keyword for the question, and a pick prefers the contact whose name
// appears in looking_for.
func keywordAnswers(req *decide.Request) (*decide.Response, error) {
	state, _ := req.State.(string)
	state = strings.ToLower(state)
	keywords := map[string][]string{"billing": {"invoice", "charged"}, "urgent": {"today"}}
	resp := &decide.Response{Answers: map[string]decide.Answer{}}
	for key, q := range req.Questions {
		switch q := q.(type) {
		case *decide.NoulQuestion:
			p := 0.1
			for _, k := range keywords[key] {
				if strings.Contains(state, k) {
					p = 0.9
				}
			}
			resp.Answers[key] = decidetest.NoulAnswer(p)
		case *decide.ChoiceQuestion:
			lookingFor, _, _ := strings.Cut(state, "\n")
			probs := []float64{}
			for _, o := range q.Criteria {
				p := 0.1
				if o.Key == "none" {
					p = 0.05
				} else if name, _, _ := strings.Cut(fmt.Sprint(o.Description), ","); strings.Contains(lookingFor, strings.ToLower(name)) {
					p = 0.85
				}
				probs = append(probs, p)
			}
			resp.Answers[key] = decidetest.ChoiceFor(q, probs...)
		}
	}
	return resp, nil
}
