package dashi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
	"github.com/deepnoodle-ai/dashi/tool"
)

// A denied call is a tool result flagged as an error. Chat Completions has no
// error flag, so the next model call must still be sent, with the failure
// stated in the tool message text.
func TestDeniedToolCallContinuesOnChatCompletions(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, body)
			n := len(bodies)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				io.WriteString(w, `{"id":"chat-1","model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"remove","arguments":"{\"path\":\"a\"}"}}]},"finish_reason":"tool_calls"}]}`)
				return
			}
			io.WriteString(w, `{"id":"chat-2","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"understood"},"finish_reason":"stop"}]}`)
		},
	))
	t.Cleanup(server.Close)
	client, err := completions.New(completions.Config{
		Provider: "openai",
		APIKey:   "test",
		Endpoint: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	remove, err := tool.Func(
		"remove",
		"Remove a file.",
		func(context.Context, tool.Call, struct {
			Path string `json:"path"`
		}) (tool.Outcome, error) {
			ran = true
			return tool.Succeeded("removed"), nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	deny := dashi.ToolHook{
		Name: "deny",
		Func: func(context.Context, dashi.ToolPlan) (dashi.ToolDecision, error) {
			return dashi.Deny("Removing files is not allowed."), nil
		},
	}
	out, err := (dashi.Runner{
		Agent: dashi.Agent{
			ID:       "agent",
			Revision: "r1",
			Provider: "openai",
			API:      completions.API,
			Model:    "m",
		},
		Client: client,
		Tools:  tool.List{remove},
		Hooks:  deny.Hooks(),
	}).Run(
		t.Context(),
		dashi.Turn{ID: "denied"},
		dashi.Start{
			CommandID: "start",
			Input:     []llm.Message{llm.UserMessage(llm.Text("remove a"))},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if out.Stop.Kind != dashi.StopCompleted || ran || len(bodies) != 2 {
		t.Fatalf("stop=%+v ran=%v sends=%d", out.Stop, ran, len(bodies))
	}
	var second struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(bodies[1], &second); err != nil {
		t.Fatal(err)
	}
	last := second.Messages[len(second.Messages)-1]
	if last["role"] != "tool" || last["tool_call_id"] != "call-1" ||
		last["content"] != "Tool failed:\nRemoving files is not allowed." {
		t.Fatalf("denied result sent as %v", last)
	}
}
