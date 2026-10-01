package compaction

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// exchange is one Turn of history: a user prompt, an assistant message with
// parallel tool calls, the results, and a final reply. Each text is padded
// to size bytes.
func exchange(n, size int) []llm.Message {
	pad := strings.Repeat("x", size)
	id := func(k int) string { return fmt.Sprintf("call-%d-%d", n, k) }
	call := func(k int) llm.Content {
		c := tool.Call{
			ID:   id(k),
			Name: "read",
			Args: json.RawMessage(`{"path":"a.go"}`),
		}
		return llm.Content{Kind: llm.ContentToolCall, ToolCall: &c}
	}
	result := func(k int) llm.Content {
		return llm.Content{
			Kind: llm.ContentToolResult,
			ToolResult: &llm.ToolResult{
				CallID: id(k),
				Output: tool.TextOutput(pad),
			},
		}
	}
	return []llm.Message{
		llm.UserMessage(llm.Text(fmt.Sprintf("prompt %d %s", n, pad))),
		llm.AssistantMessage(call(1), call(2)),
		llm.UserMessage(result(1), result(2)),
		llm.AssistantMessage(llm.Text(fmt.Sprintf("answer %d", n))),
	}
}

// conversation builds a history of turns Turns and the index where each starts.
func conversation(turns, size int) ([]llm.Message, []int) {
	var history []llm.Message
	var starts []int
	for n := 0; n < turns; n++ {
		starts = append(starts, len(history))
		history = append(history, exchange(n, size)...)
	}
	return history, starts
}

// bigModel is a policy whose window is large enough for the given tokens.
func policyWith(window int) Policy {
	p := Policy{}
	p.Model.Capabilities.ContextWindow = window
	return p
}

// validPairs fails the test if a tool call lacks its result or a result lacks
// its call.
func validPairs(t *testing.T, messages []llm.Message) {
	t.Helper()
	calls := map[string]bool{}
	results := map[string]bool{}
	for _, m := range messages {
		for _, c := range m.Content {
			if c.ToolCall != nil {
				calls[c.ToolCall.ID] = true
			}
			if c.ToolResult != nil {
				if !calls[c.ToolResult.CallID] {
					t.Fatalf("orphaned result %q", c.ToolResult.CallID)
				}
				results[c.ToolResult.CallID] = true
			}
		}
	}
	for id := range calls {
		if !results[id] {
			t.Fatalf("orphaned call %q", id)
		}
	}
}

func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(x) == string(y)
}
