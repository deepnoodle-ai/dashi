package compactiontext

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// An assistant reply that a later user message answered is not the last
// reply, so the fallback does not repeat it at the end.
func TestFallbackDropsAnsweredReply(t *testing.T) {
	call := func(id, name string) llm.Content {
		return llm.Content{Kind: llm.ContentToolCall, ToolCall: &tool.Call{ID: id, Name: name}}
	}
	result := func(id string) llm.Message {
		return llm.Message{Role: llm.RoleUser, Content: []llm.Content{{
			Kind:       llm.ContentToolResult,
			ToolResult: &llm.ToolResult{CallID: id},
		}}}
	}
	covered := []llm.Message{
		llm.AssistantMessage(llm.Text("early preamble"), call("a", "read")),
		result("a"),
		llm.UserMessage(llm.Text("new instruction")),
		llm.AssistantMessage(call("b", "write")),
		result("b"),
	}
	got := Fallback(covered, len(covered), 4096)
	if strings.Contains(got, "early preamble") || strings.Contains(got, "[your last reply]") {
		t.Fatalf("fallback repeats an answered reply:\n%s", got)
	}
	if !strings.Contains(got, "new instruction") || !strings.Contains(got, "write") {
		t.Fatalf("fallback lost later work:\n%s", got)
	}
	// A reply after the last user message is still kept.
	covered = append(covered, llm.AssistantMessage(llm.Text("all done")))
	if got := Fallback(covered, len(covered), 4096); !strings.Contains(got, "[your last reply]\nall done") {
		t.Fatalf("fallback lost the last reply:\n%s", got)
	}
}
