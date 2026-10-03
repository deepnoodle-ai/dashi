package state

import (
	"testing"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestRequest(t *testing.T) {
	reminder := llm.UserMessage(llm.Content{
		Kind:     llm.ContentReminder,
		Reminder: &llm.Reminder{Name: "time", Content: "It is noon."},
	})
	named := llm.UserMessage(llm.Text("plan: step 1"))
	named.Context = &llm.MessageContext{Origin: "user", Name: "plan"}
	view := dashi.TurnView{Input: []llm.Message{
		llm.UserMessage(llm.Text("first request")),
		llm.UserMessage(llm.Text("Triage the open tickets")),
		{Role: llm.RoleAssistant, Content: []llm.Content{llm.Text("on it")}},
		reminder,
		named,
	}}
	if got := Request(view, 0); got != "Triage the open tickets" {
		t.Fatalf("request = %q", got)
	}
	if got := Request(view, 6); got != "Triage" {
		t.Fatalf("cut request = %q", got)
	}
	if got := Request(dashi.TurnView{}, 100); got != "" {
		t.Fatalf("empty turn request = %q", got)
	}
}

// A compaction turn's Input is the session's summary instruction, not a
// request from the user.
func TestRequestCompactionTurn(t *testing.T) {
	view := dashi.TurnView{
		Origin: dashi.StartCompaction,
		Input:  []llm.Message{llm.UserMessage(llm.Text("Summarize the conversation."))},
	}
	if got := Request(view, 0); got != "" {
		t.Fatalf("compaction turn request = %q; want none", got)
	}
}

func TestCut(t *testing.T) {
	for _, tc := range []struct {
		text string
		max  int
		want string
	}{
		{"hello", 0, "hello"},
		{"hello", -1, "hello"},
		{"hello", 5, "hello"},
		{"hello", 3, "hel"},
		{"héllo", 2, "h"}, // é is two bytes
		{"héllo", 3, "hé"},
		{"日本", 4, "日"},
		{"日本", 2, ""},
	} {
		got := Cut(tc.text, tc.max)
		if got != tc.want || !utf8.ValidString(got) {
			t.Errorf("Cut(%q, %d) = %q; want %q", tc.text, tc.max, got, tc.want)
		}
	}
}
