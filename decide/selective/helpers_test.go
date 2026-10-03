package selective

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/compaction"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/decide"
	"github.com/deepnoodle-ai/decide/decidetest"
)

func user(text string) llm.Message      { return llm.UserMessage(llm.Text(text)) }
func assistant(text string) llm.Message { return llm.AssistantMessage(llm.Text(text)) }

func call(id, name string) llm.Message {
	return llm.AssistantMessage(llm.Content{
		Kind:     llm.ContentToolCall,
		ToolCall: &tool.Call{ID: id, Name: name, Args: []byte(`{}`)},
	})
}

func result(id, text string) llm.Message {
	return llm.UserMessage(llm.Content{
		Kind:       llm.ContentToolResult,
		ToolResult: &llm.ToolResult{CallID: id, Output: tool.TextOutput(text)},
	})
}

func reminder(name, from, text string) llm.Content {
	return llm.Content{
		Kind:     llm.ContentReminder,
		Reminder: &llm.Reminder{Name: name, Tier: llm.ReminderContextual, From: from, Content: text},
	}
}

func reasoning(text string) llm.Content {
	return llm.Content{Kind: llm.ContentReasoning, Reasoning: &llm.Reasoning{Text: text}}
}

// bigLog is a tool output of n lines, about 50 bytes each.
func bigLog(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString("2026-10-02T10:00:00Z INFO worker heartbeat ok seq=")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString("\n")
	}
	return b.String()
}

// The standard history. Indexes:
//
//	0 system
//	1 user: the task (pinned)
//	2 assistant: call c1      ┐ s0, a big log
//	3 result c1 (big log)     ┘
//	4 assistant: call c2      ┐ s1, the needed error
//	5 result c2 (error)       ┘
//	6 assistant: an idea        s2
//	7 user: follow-up           tail from here
//	8 assistant: reply
const (
	needle = "FAIL: unexpected token ) at offset 16"
	idea   = "Maybe the lexer is wrong; I will check it later."
)

func history() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleSystem, Content: []llm.Content{llm.Text("You are a coding agent.")}},
		user("Fix the parser tests. Do not change exported signatures."),
		call("c1", "read_log"),
		result("c1", bigLog(60)),
		call("c2", "run_tests"),
		result("c2", needle),
		assistant(idea),
		user("Now rerun the tests."),
		assistant("Rerunning."),
	}
}

// smallWindow gives a total of 1,000 tokens: older segments 600, tail 400.
func smallWindow() compaction.Compactor {
	var base compaction.Compactor
	base.Policy.Model.Capabilities.ContextWindow = 4_000
	return base
}

// judge returns a fake Decide server with canned P(yes) per question key
// and a client for it with no retries.
func judge(t *testing.T, answers map[string]float64) (*decidetest.Server, *decide.Client) {
	t.Helper()
	srv := decidetest.NewServer(t)
	for key, p := range answers {
		srv.Answer(key, decidetest.NoulAnswer(p))
	}
	return srv, srv.Client(t, decide.WithMaxRetries(0))
}

// questionKeys returns every question key the server was asked.
func questionKeys(srv *decidetest.Server) map[string]decide.Question {
	out := map[string]decide.Question{}
	for _, r := range srv.Requests() {
		if r.Request != nil {
			for k, q := range r.Request.Questions {
				out[k] = q
			}
		}
	}
	return out
}

// requestText returns the summary request's text.
func requestText(t *testing.T, input []llm.Message) string {
	t.Helper()
	if len(input) != 1 {
		t.Fatalf("summary input has %d messages", len(input))
	}
	return input[0].Text()
}
