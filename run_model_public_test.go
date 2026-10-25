package dashi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/meta"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
)

// wire replies to model calls at a public endpoint without a network, so the
// catalog and default prompt caching apply as they do in production. It
// records each request body.
type wire struct {
	mu      sync.Mutex
	replies []string
	bodies  [][]byte
}

func (w *wire) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.bodies = append(w.bodies, body)
	reply := `{"error":{"type":"invalid_request_error","message":"no reply left"}}`
	status := http.StatusBadRequest
	if len(w.replies) > 0 {
		reply, w.replies, status = w.replies[0], w.replies[1:], http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(reply)),
		Request:    r,
	}, nil
}

func (w *wire) requests(t *testing.T) []map[string]json.RawMessage {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]map[string]json.RawMessage, len(w.bodies))
	for i, b := range w.bodies {
		if err := json.Unmarshal(b, &out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func claudeToolUse(id, name string) string {
	return fmt.Sprintf(`{"id":"msg_%s","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`, id, id, name)
}

func claudeText(text string) string {
	return fmt.Sprintf(`{"id":"msg_t","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`, text)
}

func lookTool(t *testing.T) tool.Tool {
	t.Helper()
	return namedTool(t, "look")
}

func namedTool(t *testing.T, name string) tool.Tool {
	t.Helper()
	look, err := tool.Func(name, "Look around.",
		func(context.Context, tool.Call, struct{}) (tool.Outcome, error) {
			return tool.Succeeded("a desk"), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return look
}

// messagesArray splits a request's messages into their exact bytes.
func messagesArray(t *testing.T, req map[string]json.RawMessage) []string {
	t.Helper()
	var raw []json.RawMessage
	if err := json.Unmarshal(req["messages"], &raw); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(raw))
	for i, m := range raw {
		out[i] = string(m)
	}
	return out
}

// requirePrefix checks that next starts with the cached part of prev: the
// same tools and system bytes, and prev's messages as its leading messages.
func requirePrefix(t *testing.T, prev, next map[string]json.RawMessage, label string) {
	t.Helper()
	for _, field := range []string{"tools", "system", "cache_control"} {
		if !bytes.Equal(prev[field], next[field]) {
			t.Fatalf("%s: %s changed:\n%s\n%s", label, field, prev[field], next[field])
		}
	}
	a, b := messagesArray(t, prev), messagesArray(t, next)
	if len(b) < len(a) {
		t.Fatalf("%s: history shrank from %d to %d messages", label, len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("%s: message %d changed:\n%s\n%s", label, i, a[i], b[i])
		}
	}
}

// Within a turn each request starts with the previous one, reminders
// included: a replaced snapshot stays where it was. The one miss is a
// NextCallOnly item, which leaves the history once a call has seen it.
func TestTurnRequestsKeepThePromptPrefixAcrossReminders(t *testing.T) {
	w := &wire{replies: []string{
		claudeToolUse("toolu_1", "look"),
		claudeToolUse("toolu_2", "look"),
		claudeText("Done."),
	}}
	client, err := messages.New(messages.Config{
		APIKey: "test",
		Client: &http.Client{Transport: w},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := 0
	hook := dashi.ModelHook{
		Name: "clock",
		Func: func(context.Context, dashi.ModelPlan) (dashi.ModelDecision, error) {
			call++
			items := []dashi.ContextItem{{
				ID:       fmt.Sprintf("clock-%d", call),
				Name:     "clock",
				Origin:   dashi.ContextRuntime,
				Snapshot: true,
				Message: llm.ReminderMessage(llm.Reminder{
					Name:    "clock",
					Tier:    llm.ReminderContextual,
					Content: fmt.Sprintf("It is 10:0%d.", call),
				}),
			}}
			if call == 1 {
				items = append(items, dashi.ContextItem{
					ID:           "hint",
					Origin:       dashi.ContextRuntime,
					NextCallOnly: true,
					Message: llm.ReminderMessage(llm.Reminder{
						Name:    "hint",
						Tier:    llm.ReminderContextual,
						Content: "Look first.",
					}),
				})
			}
			return dashi.ModelDecision{AddMessages: items}, nil
		},
	}
	r := dashi.Runner{
		Client: client,
		Tools:  tool.List{lookTool(t)},
		Hooks:  hook.Hooks(),
	}
	out, err := r.Run(t.Context(), dashi.NewTurn("t-1"), dashi.Start{
		CommandID: "start",
		Agent:     dashi.Agent{Model: "claude-sonnet-5", Instructions: "Look."},
		Input:     []llm.Message{llm.UserMessage(llm.Text("Go."))},
	})
	if err != nil || out.Reply != "Done." {
		t.Fatalf("outcome %q, %v", out.Reply, err)
	}
	reqs := w.requests(t)
	if len(reqs) != 3 {
		t.Fatalf("requests = %d", len(reqs))
	}
	first, second := messagesArray(t, reqs[0]), messagesArray(t, reqs[1])
	// Call 1: user, clock 1, hint. Call 2 drops the spent hint and keeps
	// everything before it.
	if len(first) != 3 || !strings.Contains(first[2], "Look first.") {
		t.Fatalf("first request messages: %v", first)
	}
	if first[0] != second[0] || first[1] != second[1] ||
		strings.Contains(strings.Join(second, ""), "Look first.") {
		t.Fatalf("second request did not keep the prefix up to the spent item:\n%v\n%v", first, second)
	}
	requirePrefix(t, reqs[1], reqs[2], "call 3")
	third := strings.Join(messagesArray(t, reqs[2]), "")
	if strings.Index(third, "10:01") < 0 ||
		strings.Index(third, "10:01") > strings.Index(third, "10:03") {
		t.Fatalf("replaced snapshots are not kept in order: %s", third)
	}
	var system []map[string]any
	_ = json.Unmarshal(reqs[0]["system"], &system)
	if len(system) != 1 ||
		!strings.Contains(system[0]["text"].(string), "the latest is current") {
		t.Fatalf("reminder instruction: %v", system)
	}
}

func openAIText(text string) string {
	return fmt.Sprintf(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":%q,"annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`, text)
}

// Runner.CacheKey is sent as the OpenAI prompt cache key on models where a
// key per conversation only improves routing, and not on GPT-5.6, where a
// key may separate caches. An agent's own key wins.
func TestRunnerCacheKeyIsTheDefaultPromptCacheKey(t *testing.T) {
	for _, tc := range []struct {
		name, model, agentKey, want string
	}{
		{"before GPT-5.6", "gpt-5.5", "", "conversation-1"},
		{"GPT-5.6", "gpt-5.6", "", ""},
		{"agent key wins", "gpt-5.5", "tenant-4", "tenant-4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &wire{replies: []string{openAIText("Hi.")}}
			client, err := responses.New(responses.Config{
				APIKey: "test",
				Client: &http.Client{Transport: w},
			})
			if err != nil {
				t.Fatal(err)
			}
			agent := dashi.Agent{Model: tc.model}
			agent.Settings.PromptCache.Key = tc.agentKey
			r := dashi.Runner{Client: client, CacheKey: "conversation-1"}
			start := dashi.Say("Hello.")
			start.Agent = agent
			out, err := r.Run(t.Context(), dashi.NewTurn("t-1"), start)
			if err != nil {
				t.Fatal(err)
			}
			var key string
			_ = json.Unmarshal(w.requests(t)[0]["prompt_cache_key"], &key)
			if key != tc.want {
				t.Fatalf("prompt_cache_key = %q, want %q", key, tc.want)
			}
			// The key is not part of the agent's behavior.
			if out.Turn.Steps[0].Started.Agent.Settings.PromptCache.Key != tc.agentKey {
				t.Fatal("the runner's key was recorded on the agent")
			}
		})
	}
}

func TestAgentVersionIgnoresTheCacheKey(t *testing.T) {
	revision := func(key string) string {
		w := &wire{replies: []string{claudeText("Hi.")}}
		client, err := messages.New(messages.Config{
			APIKey: "test",
			Client: &http.Client{Transport: w},
		})
		if err != nil {
			t.Fatal(err)
		}
		agent := dashi.Agent{Model: "claude-sonnet-5"}
		agent.Settings.PromptCache.Key = key
		out, err := dashi.Runner{Client: client}.Run(
			t.Context(),
			dashi.NewTurn("t-"+key),
			dashi.Start{CommandID: "s", Agent: agent, Input: []llm.Message{llm.UserMessage(llm.Text("Hi."))}},
		)
		if err != nil {
			t.Fatal(err)
		}
		return out.Turn.Steps[0].Started.Agent.Version
	}
	if a, b := revision("one"), revision("two"); a != b {
		t.Fatalf("revisions differ by key: %s, %s", a, b)
	}
}

// A capped turn with a one-hour lifetime reserves at the one-hour write
// rate, and cannot run on a sheet that lacks it.
func TestOneHourCacheBudgetUsesTheOneHourRate(t *testing.T) {
	run := func(oneHour *money.Amount) dashi.RunResult {
		t.Helper()
		w := &wire{replies: []string{claudeText("Hi.")}}
		pricing := &llm.Pricing{
			Currency:  "USD",
			UpdatedAt: "2026-09-29",
			Base: llm.Rates{
				Input:        money.MustParseAmount("3"),
				Output:       money.MustParseAmount("15"),
				CacheRead:    money.MustParseAmount("0.3"),
				CacheWrite:   money.MustParseAmount("3.75"),
				CacheWrite1h: oneHour,
			},
		}
		info := llm.ModelInfo{
			Pricing: pricing,
			Evidence: llm.ModelEvidence{
				Model:       "claude-sonnet-5",
				API:         messages.API,
				Endpoint:    "https://api.anthropic.com/v1/messages",
				Owner:       "Dashi tests",
				Source:      "fixture",
				Method:      llm.EvidenceFixture,
				QualifiedAt: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
			},
		}
		client, err := messages.New(messages.Config{
			APIKey: "test",
			Client: &http.Client{Transport: w},
			Models: map[string]llm.ModelInfo{"claude-sonnet-5": info},
		})
		if err != nil {
			t.Fatal(err)
		}
		agent := dashi.Agent{Model: "claude-sonnet-5"}
		agent.Settings.PromptCache.TTL = time.Hour
		agent.Settings.MaxTokens = 100
		out, err := dashi.Runner{Client: client}.Run(
			t.Context(),
			dashi.NewTurn("capped"),
			dashi.Start{
				CommandID: "s",
				Agent:     agent,
				Budget:    &dashi.Budget{MaxCost: &dashi.Money{Amount: money.MustParseAmount("10"), Currency: "USD"}},
				Input:     []llm.Message{llm.UserMessage(llm.Text("Hi."))},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	rate := money.MustParseAmount("6")
	out := run(&rate)
	var callStart *dashi.ModelFact
	for _, step := range out.Turn.Steps {
		if step.Kind == dashi.StepModelCallStarted {
			callStart = step.Model
		}
	}
	if out.Stop.Kind != dashi.StopCompleted || callStart == nil ||
		callStart.WorstCaseCost == nil || callStart.EstimatedInputTokens == 0 {
		t.Fatalf("capped one-hour turn: %+v", out.Stop)
	}
	// The input bound is priced at the $6 one-hour rate, the highest.
	want := money.AmountFromNanos(int64(callStart.EstimatedInputTokens) * 6000)
	if callStart.WorstCaseCost.Input != want {
		t.Fatalf("reserved input %s, want %s at the one-hour rate", callStart.WorstCaseCost.Input, want)
	}
	out = run(nil)
	if out.Stop.Kind != dashi.StopLimited || out.Stop.Limit == nil ||
		out.Stop.Limit.Kind != dashi.LimitCost {
		t.Fatalf("sheet without a one-hour rate: %+v", out.Stop)
	}
}

// A conversation can switch provider and model between turns. Reasoning
// and provider tool work from the first model cannot be replayed to the
// second, so the second call omits them and records each omission.
func TestNextTurnSwitchesProviderAfterReasoningModel(t *testing.T) {
	openAI := &wire{replies: []string{
		`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[` +
			`{"id":"rs_1","type":"reasoning","content":[],"encrypted_content":"gAAA","summary":[]},` +
			`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"look","arguments":"{}","status":"completed"}` +
			`],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`,
		`{"id":"resp_2","object":"response","status":"completed","model":"gpt-5.5","output":[` +
			`{"id":"rs_2","type":"reasoning","content":[],"encrypted_content":"gBBB","summary":[]},` +
			`{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"desk"}},` +
			`{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"A desk.","annotations":[]}]}` +
			`],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`,
	}}
	first, err := responses.New(responses.Config{APIKey: "test", Client: &http.Client{Transport: openAI}})
	if err != nil {
		t.Fatal(err)
	}
	start := dashi.Say("What is here?")
	start.Agent = dashi.Agent{Model: "gpt-5.5"}
	one, err := dashi.Runner{Client: first, Tools: tool.List{lookTool(t)}}.Run(t.Context(), dashi.NewTurn("t-1"), start)
	if err != nil || one.Reply != "A desk." {
		t.Fatalf("first turn: %q, %v, stop %+v", one.Reply, err, one.Stop)
	}
	history, warnings := dashi.BuildHistory([]dashi.Turn{one.Turn})
	if len(warnings) > 0 {
		t.Fatalf("history warnings: %v", warnings)
	}

	for _, tc := range []struct {
		name, model, reply string
		client             func(*http.Client) (llm.Client, error)
	}{
		{"anthropic", "claude-sonnet-5", claudeText("Still a desk."), func(h *http.Client) (llm.Client, error) {
			return messages.New(messages.Config{APIKey: "test", Client: h})
		}},
		// The same Responses API at another provider is still another
		// destination for replay.
		{"meta", "muse-spark-1.3", openAIText("Still a desk."), func(h *http.Client) (llm.Client, error) {
			return meta.New(meta.Config{APIKey: "test", Client: h})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &wire{replies: []string{tc.reply}}
			second, err := tc.client(&http.Client{Transport: w})
			if err != nil {
				t.Fatal(err)
			}
			next := dashi.Say("And now?")
			next.History = history
			next.Agent = dashi.Agent{Model: tc.model}
			two, err := dashi.Runner{Client: second, Tools: tool.List{lookTool(t)}}.Run(t.Context(), dashi.NewTurn("t-2"), next)
			if err != nil || two.Reply != "Still a desk." {
				t.Fatalf("second turn after switching model: %q, %v, stop %+v", two.Reply, err, two.Stop)
			}
			body := string(w.bodies[0])
			for _, private := range []string{"gAAA", "gBBB", "ws_1"} {
				if strings.Contains(body, private) {
					t.Fatalf("foreign replay %q reached the new provider: %s", private, body)
				}
			}
			omitted := map[llm.ContentChangeAction]int{}
			for _, step := range two.Turn.Steps {
				if step.Kind == dashi.StepModelCallStarted {
					for _, change := range step.Model.ContentChanges {
						omitted[change.Action]++
					}
				}
			}
			if omitted[llm.ContentChangeOmitReasoning] != 2 || omitted[llm.ContentChangeOmitProviderToolActivity] != 1 {
				t.Fatalf("omissions not recorded: %v", omitted)
			}
		})
	}
}
