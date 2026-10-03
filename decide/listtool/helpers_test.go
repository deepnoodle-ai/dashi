package listtool_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/decide/listtool"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

// entry is a list item in tests. Its text tells the fake Decide server
// what to answer: "billing=0.95" sets P(yes) for the question billing,
// "tone=calm" makes calm the likely option of the question tone, and
// "fail" makes the request fail.
type entry struct{ id, text string }

// fakeList is a list over entries that records each query it gets.
type fakeList struct {
	name    string
	entries []entry
	err     error

	mu      sync.Mutex
	queries []string
}

func (f *fakeList) list() listtool.List[entry] {
	return listtool.List[entry]{
		Name:        f.name,
		Description: "Test entries. The query is ignored.",
		Items: func(_ context.Context, query string) ([]entry, error) {
			f.mu.Lock()
			f.queries = append(f.queries, query)
			f.mu.Unlock()
			return f.entries, f.err
		},
		ID:   func(e entry) string { return e.id },
		Text: func(e entry) string { return e.text },
	}
}

var (
	noulRule   = regexp.MustCompile(`(\w+)=([0-9.]+)`)
	choiceRule = regexp.MustCompile(`(\w+)=([a-z_]+)`)
)

// fakeDecide is a decidetest server whose answers come from the state's
// text, as entry describes. A pick request's answer comes from weights,
// by option description, with weights["none"] for "none of these".
type fakeDecide struct {
	*decidetest.Server
	mu      sync.Mutex
	weights map[string]float64
}

func newFakeDecide(t *testing.T) (*fakeDecide, *decide.Client) {
	t.Helper()
	f := &fakeDecide{Server: decidetest.NewServer(t)}
	f.Respond(f.answer)
	return f, f.Client(t, decide.WithMaxRetries(0))
}

func (f *fakeDecide) setWeights(w map[string]float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.weights = w
}

func (f *fakeDecide) answer(req *decide.Request) (*decide.Response, error) {
	text, _ := req.State.(string)
	if strings.Contains(text, "fail") {
		return nil, &decide.APIError{StatusCode: 400}
	}
	resp := &decide.Response{
		Answers: map[string]decide.Answer{},
		Usage:   decide.Usage{InputTokens: 10 + len(text)/4, OutputTokens: 2 * len(req.Questions)},
	}
	for key, q := range req.Questions {
		switch q := q.(type) {
		case *decide.NoulQuestion:
			p := 0.05
			for _, m := range noulRule.FindAllStringSubmatch(text, -1) {
				if m[1] == key {
					p, _ = strconv.ParseFloat(m[2], 64)
				}
			}
			resp.Answers[key] = decidetest.NoulAnswer(p)
		case *decide.ChoiceQuestion:
			if key == "pick" {
				resp.Answers[key] = f.pick(q)
				continue
			}
			chosen := 0
			for _, m := range choiceRule.FindAllStringSubmatch(text, -1) {
				for i, o := range q.Criteria {
					if m[1] == key && m[2] == o.Key {
						chosen = i
					}
				}
			}
			probs := make([]float64, len(q.Criteria))
			for i := range probs {
				probs[i] = 0.1 / float64(len(probs)-1)
			}
			probs[chosen] = 0.9
			resp.Answers[key] = decidetest.ChoiceFor(q, probs...)
		}
	}
	return resp, nil
}

// pick spreads probability over the options by weight.
func (f *fakeDecide) pick(q *decide.ChoiceQuestion) *decide.ChoiceAnswer {
	f.mu.Lock()
	defer f.mu.Unlock()
	probs := make([]float64, len(q.Criteria))
	total := 0.0
	for i, o := range q.Criteria {
		key := fmt.Sprint(o.Description)
		if o.Key == "none" {
			key = "none"
		}
		probs[i] = f.weights[key] + 0.001
		total += probs[i]
	}
	for i := range probs {
		probs[i] /= total
	}
	return decidetest.ChoiceFor(q, probs...)
}

// states returns the state of every request the server received, sorted.
func (f *fakeDecide) states() []string {
	var out []string
	for _, r := range f.Requests() {
		if r.Request != nil {
			s, _ := r.Request.State.(string)
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// usd prices usage at one nano-dollar per input token.
func usd(_ string, u decide.Usage) *tool.Cost {
	return &tool.Cost{Amount: money.AmountFromNanos(int64(u.InputTokens)), Currency: "USD"}
}

// call runs a tool's executor directly, outside a runner.
func call(t *testing.T, ctx context.Context, tl tool.Tool, args string) (tool.Outcome, error) {
	t.Helper()
	return tl.Executor.Execute(ctx, tool.Call{ID: "call-1", Name: tl.Definition.Name, Args: json.RawMessage(args)})
}

// text is an outcome's output text.
func text(o tool.Outcome) string {
	if o.Output == nil || len(o.Output.Content) == 0 || o.Output.Content[0].Text == nil {
		return ""
	}
	return o.Output.Content[0].Text.Text
}

// data decodes an outcome's Data.
func data(t *testing.T, o tool.Outcome) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(o.Data, &m); err != nil {
		t.Fatalf("Data %q: %v", o.Data, err)
	}
	return m
}

// runTurn runs one turn with the tools on a scripted model.
func runTurn(
	t *testing.T,
	tools tool.List,
	limits dashi.Limits,
	request string,
	replies ...llm.Result,
) dashi.RunResult {
	t.Helper()
	runner := dashi.Runner{Client: dashitest.NewClient(replies...), Tools: tools}
	out, err := runner.Run(context.Background(), dashi.Turn{}, dashi.Start{
		Agent: dashi.Agent{Model: dashitest.Model, Limits: limits},
		Input: []llm.Message{llm.UserMessage(llm.Text(request))},
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// tickets returns n entries T-001… whose text sets billing and urgent.
func tickets(n int, text func(i int) string) []entry {
	out := make([]entry, n)
	for i := range out {
		out[i] = entry{id: fmt.Sprintf("T-%03d", i+1), text: text(i)}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
