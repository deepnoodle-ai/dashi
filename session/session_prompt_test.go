package session_test

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

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

// wire answers model calls at a public endpoint without a network, so the
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

// A session caches by default on Claude, and the second turn, run on an
// agent the host builds afresh, starts with the exact bytes of the first
// turn's last request: tools, system text, and the leading messages.
func TestSessionTurnsKeepThePromptPrefix(t *testing.T) {
	w := &wire{replies: []string{
		claudeToolUse("toolu_1", "look"),
		claudeText("A desk."),
		claudeText("Still a desk."),
	}}
	client, err := messages.New(messages.Config{
		APIKey: "test",
		Client: &http.Client{Transport: w},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Several tools, sent in the toolset's order on every call.
	s := &session.Session{Runner: dashi.Runner{
		Client: client,
		Tools:  tool.List{namedTool(t, "zoom"), lookTool(t), namedTool(t, "aim")},
	}}
	agent := func() dashi.Agent {
		return dashi.Agent{
			Model:        "claude-sonnet-5",
			Instructions: "You describe rooms.",
		}
	}
	s.Agent = agent()
	if _, err := s.Send(t.Context(), "What is here?"); err != nil {
		t.Fatal(err)
	}
	s.Agent = agent()
	out, err := s.Send(t.Context(), "And now?")
	if err != nil {
		t.Fatal(err)
	}
	if out.Answer != "Still a desk." {
		t.Fatalf("answer = %q", out.Answer)
	}
	reqs := w.requests(t)
	if len(reqs) != 3 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if string(reqs[0]["cache_control"]) != `{"type":"ephemeral"}` ||
		!strings.Contains(string(reqs[0]["system"]), `"cache_control"`) {
		t.Fatalf("first request has no cache points: %s", w.bodies[0])
	}
	if tools := string(reqs[0]["tools"]); strings.Index(tools, `"zoom"`) >
		strings.Index(tools, `"aim"`) {
		t.Fatalf("tools were reordered: %s", tools)
	}
	requirePrefix(t, reqs[0], reqs[1], "turn 1 call 2")
	requirePrefix(t, reqs[1], reqs[2], "turn 2 call 1")
}

func openAIText(text string) string {
	return fmt.Sprintf(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":%q,"annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`, text)
}

// A session sends its ID as the OpenAI prompt cache key on models where a
// key per conversation only improves routing, and not on GPT-5.6, where a
// key may separate caches. An agent's own key wins.
func TestSessionIDIsTheDefaultCacheKey(t *testing.T) {
	for _, tc := range []struct {
		name, model, agentKey string
		want                  func(sessionID string) string
	}{
		{"before GPT-5.6", "gpt-5.5", "", func(id string) string { return id }},
		{"GPT-5.6", "gpt-5.6", "", func(string) string { return "" }},
		{"agent key wins", "gpt-5.5", "tenant-4", func(string) string { return "tenant-4" }},
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
			s := &session.Session{Runner: dashi.Runner{Client: client}, Agent: agent}
			out, err := s.Send(t.Context(), "Hello.")
			if err != nil {
				t.Fatal(err)
			}
			var key string
			_ = json.Unmarshal(w.requests(t)[0]["prompt_cache_key"], &key)
			if want := tc.want(s.ID); key != want {
				t.Fatalf("prompt_cache_key = %q, want %q", key, want)
			}
			// The key is not part of the agent's behavior.
			if out.Turn.Steps[0].Started.Agent.Settings.PromptCache.Key != tc.agentKey {
				t.Fatal("the session key was recorded on the agent")
			}
		})
	}
}

func TestRecordedSummaryPreferenceSurvivesReopenAndModelSwitch(t *testing.T) {
	agentOf := func(turn dashi.Turn) dashi.Agent {
		view, err := dashi.Replay(turn)
		if err != nil {
			t.Fatal(err)
		}
		return view.Agent
	}
	dir := t.TempDir()
	recorder, err := filerecorder.New(filerecorder.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	public := llm.Content{Kind: llm.ContentReasoning, Reasoning: &llm.Reasoning{Text: "public"}, Replay: &llm.Opaque{Version: 1, Type: "reasoning", Unit: llm.OpaqueUnitContentBlock, Origin: content.Origin{Provider: dashitest.Provider, API: dashitest.API, Model: dashitest.Model, Scope: "dashitest"}, Raw: json.RawMessage(`{"private":"CIPHER"}`)}}
	reply := dashitest.Reply("answer")
	reply.Message.Content = append([]llm.Content{public}, reply.Message.Content...)
	client := dashitest.NewClient(reply, dashitest.Reply("next"), dashitest.Reply("third"))
	s := &session.Session{ID: "summary", New: true, Runner: dashi.Runner{Client: client, Recorder: recorder}, Agent: dashi.Agent{Model: dashitest.Model, Settings: llm.Settings{ReasoningSummary: new(true)}}}
	first, err := s.Send(t.Context(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if first.Answer != "answer" {
		t.Fatalf("public summary reached answer: %q stop=%+v turn=%+v", first.Answer, first.Stop, first.Turn)
	}
	reopenedRecorder, err := filerecorder.New(filerecorder.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	reopened := &session.Session{ID: "summary", Runner: dashi.Runner{Client: client, Recorder: reopenedRecorder}}
	second, err := reopened.Send(t.Context(), "again")
	if err != nil {
		t.Fatal(err)
	}
	if pref := agentOf(second.Turn).Settings.ReasoningSummary; pref == nil || !*pref {
		t.Fatal("reopened turn did not inherit true")
	}
	reopened.Agent = dashi.Agent{Model: "another-model", Settings: llm.Settings{ReasoningSummary: new(false)}}
	third, err := reopened.Send(t.Context(), "stop requesting")
	if err != nil {
		t.Fatal(err)
	}
	if pref := agentOf(third.Turn).Settings.ReasoningSummary; pref == nil || *pref || agentOf(third.Turn).Model != "another-model" {
		t.Fatal("false/model switch lost")
	}
	loaded, err := reopenedRecorder.Load(t.Context(), first.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pref := agentOf(loaded).Settings.ReasoningSummary; pref == nil || !*pref {
		t.Fatal("new off preference changed prior turn")
	}
	if !strings.Contains(string(public.Replay.Raw), "CIPHER") {
		t.Fatal("private artifact changed")
	}
	inputs := client.Inputs()
	if len(inputs) != 3 || inputs[2].Settings.ReasoningSummary == nil || *inputs[2].Settings.ReasoningSummary {
		t.Fatal("false not sent to next call")
	}
	found := false
	for _, message := range inputs[2].Messages {
		for _, part := range message.Content {
			if part.Reasoning != nil && part.Reasoning.Text == "public" && part.Replay != nil && strings.Contains(string(part.Replay.Raw), "CIPHER") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("summary-off stripped earlier display/replay")
	}
	for _, turn := range []dashi.Turn{first.Turn, second.Turn, third.Turn} {
		foundFact := false
		for _, step := range turn.Steps {
			if step.Kind == dashi.StepModelIntent && step.Model != nil {
				pref := step.Model.Effective.ReasoningSummary
				if pref == nil || *pref != *agentOf(turn).Settings.ReasoningSummary {
					t.Fatal("recorded model fact preference differs")
				}
				foundFact = true
			}
		}
		if !foundFact {
			t.Fatal("missing recorded model facts")
		}
	}
}
