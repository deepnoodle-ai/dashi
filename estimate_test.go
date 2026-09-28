package dashi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
)

// prefixThrough returns the turn up to and including the n-th step of kind.
func prefixThrough(t *testing.T, turn Turn, kind StepKind, n int) Turn {
	t.Helper()
	for i, step := range turn.Steps {
		if step.Kind == kind {
			n--
			if n == 0 {
				out := cloneTurn(turn)
				out.Steps = out.Steps[:i+1]
				return out
			}
		}
	}
	t.Fatalf("no step %d of kind %s", n, kind)
	return Turn{}
}

func TestNextCallFitsWithACallInFlight(t *testing.T) {
	client := newPricedClient(final("done"))
	out, err := runBudgetTurnErr(
		client,
		&Budget{MaxCost: &Money{Amount: amount("1"), Currency: "USD"}},
		llm.Settings{MaxTokens: 100},
		nil,
		Hooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	// Cut the record while the first call runs.
	inFlight := prefixThrough(t, out.Turn, StepModelIntent, 1)
	s, err := replay(inFlight)
	if err != nil {
		t.Fatal(err)
	}
	if s.modelIntent == "" || s.latestAttempt != "" {
		t.Fatalf("not in flight: intent=%q latest=%q", s.modelIntent, s.latestAttempt)
	}
	risk := s.modelIntents[s.modelIntent].AtRisk.Total
	r := &runtime{ctx: t.Context(), agent: s.Agent}
	if !r.nextCallFits(s, 0) {
		t.Fatal("a generous cap refused the next call during the first call")
	}
	// The next call is larger than the running one, so a cap that fits the
	// running call's worst case twice over, but not a larger second call
	// beside it, must refuse.
	tight := risk + risk
	s.Budget.MaxCost.Amount = tight
	if r.nextCallFits(s, 0) {
		t.Fatalf("cap %s allowed a next call beside a running call at risk %s", tight, risk)
	}
}

// promptOnly prepares requests with a real provider adapter and answers
// from a script, so the estimate can be compared with real request bodies.
type promptOnly struct {
	llm.Client
	results []llm.Result
	sends   int
	sizes   []int
}

func (c *promptOnly) Prepare(ctx context.Context, in llm.Input) (llm.Request, error) {
	req, err := c.Client.Prepare(ctx, in)
	if err == nil {
		c.sizes = append(c.sizes, llm.EstimatedInputTokens(req))
	}
	return req, err
}

func (c *promptOnly) Send(context.Context, llm.Call) (llm.Result, error) {
	result := c.results[c.sends]
	c.sends++
	return result, nil
}

func TestNextRequestEstimateIsAnUpperBound(t *testing.T) {
	openai, err := responses.New(responses.Config{APIKey: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	anthropic, err := messages.New(messages.Config{APIKey: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		client llm.Client
		model  string
	}{
		"openai responses":   {openai, "gpt-5-mini"},
		"anthropic messages": {anthropic, "claude-sonnet-4-5"},
	} {
		t.Run(name, func(t *testing.T) {
			ref := llm.ModelRef{Provider: tc.client.Provider(), Model: tc.model}
			// Quoted, escaped tool arguments are what some providers
			// encode twice.
			args, _ := json.Marshal(map[string]string{
				"q": strings.Repeat("\"é\n<>&", 100),
			})
			call := tool.Call{ID: "call_1", Name: "lookup", Args: args}
			client := &promptOnly{Client: tc.client, results: []llm.Result{
				{
					Message: llm.AssistantMessage(
						llm.Text("thinking: "+strings.Repeat("\" <tag> é\n", 200)),
						llm.Content{Kind: llm.ContentToolCall, ToolCall: &call},
					),
					Stop:      llm.Stop{Kind: llm.StopToolUse},
					Requested: ref,
				},
				{
					Message:   llm.AssistantMessage(llm.Text("done")),
					Stop:      llm.Stop{Kind: llm.StopFinished},
					Requested: ref,
				},
			}}
			lookup := tool.Tool{
				Definition: tool.Definition{
					Name:   "lookup",
					Schema: []byte(`{"type":"object"}`),
					Kind:   tool.KindLocal,
				},
				Executor: execFunc(func(context.Context, tool.Call) (tool.Outcome, error) {
					return tool.Succeeded(strings.Repeat("\"r\"\n", 150)), nil
				}),
			}
			out, err := (Runner{
				Agent: Agent{
					Model:    tc.model,
					Settings: llm.Settings{MaxTokens: 100},
				},
				Client: client,
				Tools:  tool.List{lookup},
			}).Run(t.Context(), Turn{ID: "t1"}, Start{
				Input: []llm.Message{
					llm.UserMessage(llm.Text("question " + strings.Repeat("q", 200))),
				},
			})
			if err != nil || out.Stop.Kind != StopCompleted || len(client.sizes) != 2 {
				t.Fatalf("stop=%+v sizes=%v err=%v", out.Stop, client.sizes, err)
			}
			// Estimate the second request from the record before it.
			before := prefixThrough(t, out.Turn, StepToolSettled, 1)
			s, err := replay(before)
			if err != nil {
				t.Fatal(err)
			}
			req, ok := estimatedNextRequest(s, 0)
			if !ok {
				t.Fatal("no estimate")
			}
			estimate := llm.EstimatedInputTokens(req)
			if estimate < client.sizes[1] {
				t.Fatalf("estimate %d is below the real request %d", estimate, client.sizes[1])
			}
		})
	}
}
