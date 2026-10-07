package providers_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/decide/selective"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/providers/anthropic/messages"
	"github.com/deepnoodle-ai/dashi/providers/openai/completions"
	"github.com/deepnoodle-ai/dashi/providers/openai/responses"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

func TestPrepareSelectiveRetainedDirectCallerReplay(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"type":"message","id":"source","model":"fixture","stop_reason":"tool_use","content":[{"type":"text","text":"Synthetic message"},{"type":"tool_use","id":"call-2","name":"act","input":{"counter":9007199254740993},"caller":{"type":"direct"}}]}`)
	}))
	defer source.Close()
	client, err := messages.New(messages.Config{Provider: "anthropic", Endpoint: source.URL})
	if err != nil {
		t.Fatal(err)
	}
	request, err := client.Prepare(t.Context(), llm.Input{Model: "fixture", Messages: []llm.Message{llm.UserMessage(llm.Text("Start."))}})
	if err != nil {
		t.Fatal(err)
	}
	generated, err := client.Send(t.Context(), llm.Call{Request: request})
	if err != nil {
		t.Fatal(err)
	}
	reply := func(id, text string) llm.Message {
		return llm.UserMessage(llm.Content{Kind: llm.ContentToolResult, ToolResult: &llm.ToolResult{CallID: id, Output: tool.TextOutput(text)}})
	}
	history := []llm.Message{
		{Role: llm.RoleSystem, Content: []llm.Content{llm.Text("Test instructions.")}},
		llm.UserMessage(llm.Text("Fix synthetic parser.")),
		llm.AssistantMessage(llm.Content{Kind: llm.ContentToolCall, ToolCall: &tool.Call{ID: "call-1", Name: "act", Args: []byte(`{}`)}}),
		reply("call-1", strings.Repeat("synthetic obsolete log text\n", 100)),
		generated.Message,
		reply("call-2", "Keep this synthetic tool result."),
		llm.AssistantMessage(llm.Text("Obsolete idea.")),
		llm.UserMessage(llm.Text("Continue.")),
		llm.AssistantMessage(llm.Text("Continuing.")),
	}
	saved, _ := json.Marshal(history)
	server := decidetest.NewServer(t)
	for key, p := range map[string]float64{"s0.needed": 0.1, "s1.needed": 0.9, "s2.needed": 0.1} {
		server.Answer(key, decidetest.NoulAnswer(p))
	}
	var selection selective.Selection
	var base compaction.Compactor
	base.Policy.Model.Capabilities.ContextWindow = 4000
	retained, err := (&selective.Compactor{Base: base, Client: server.Client(t, decide.WithMaxRetries(0)), MinVerbatim: -1, OnSelection: func(s selective.Selection) { selection = s }}).Start(t.Context(), history)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Fallback != "" {
		t.Fatalf("selective fixture fell back: %s", selection.Fallback)
	}
	a, _ := messages.New(messages.Config{Provider: "anthropic", Endpoint: "https://unused.invalid"})
	r, _ := responses.New(responses.Config{Provider: "openai", Endpoint: "https://unused.invalid"})
	c, _ := completions.New(completions.Config{Provider: "openai", Endpoint: "https://unused.invalid"})
	for _, destination := range []llm.Client{a, r, c} {
		input := llm.Input{Model: "destination", Messages: retained.Compaction.Keep}
		before, _ := json.Marshal(input)
		prepared, err := destination.Prepare(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		body := string(prepared.Body())
		if !strings.Contains(body, "9007199254740993") || !strings.Contains(body, "call-2") || !strings.Contains(body, "Keep this synthetic tool result.") {
			t.Fatalf("retained exchange changed: %s", body)
		}
		if len(prepared.ContentChanges()) != 2 {
			t.Fatalf("changes = %+v", prepared.ContentChanges())
		}
		after, _ := json.Marshal(input)
		if string(before) != string(after) {
			t.Fatal("preparation changed retained input")
		}
	}
	after, _ := json.Marshal(history)
	if string(saved) != string(after) {
		t.Fatal("compaction or preparation changed saved history")
	}
	t.Logf("Selective retention and offline preparation pass on all three destinations. Decide used %d local requests.", selection.Requests)
}
