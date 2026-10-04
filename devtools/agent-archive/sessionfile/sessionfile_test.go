package sessionfile

import (
	"strings"
	"testing"

	"github.com/deepnoodle-ai/dashi/llm"
)

func TestReminderName(t *testing.T) {
	for in, want := range map[string]string{
		"hook_additional_context": "hook-additional-context",
		"Date":                    "date",
		"9 odd!":                  "odd-",
		"___":                     "attachment",
		"":                        "attachment",
	} {
		if got := ReminderName(in); got != want {
			t.Errorf("ReminderName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReminder(t *testing.T) {
	changed := Counts{}
	m, ok := Reminder("agent-message", `bad "sender"`, "hello", changed)
	if !ok || m.Content[0].Reminder.From != "" || m.Content[0].Validate() != nil {
		t.Fatalf("reminder with a bad sender: %+v", m)
	}
	m, ok = Reminder("x", "", strings.Repeat("a", MaxReminderBytes+1), changed)
	if !ok || m.Content[0].Kind != llm.ContentBlock {
		t.Fatalf("large reminder: %+v", m.Content[0].Kind)
	}
	if _, ok := Reminder("x", "", "", changed); ok {
		t.Fatal("empty reminder was kept")
	}
	if changed[ChangeReminderAsText] != 1 || changed[ChangeDropped+"reminder"] != 1 {
		t.Fatalf("changed: %v", changed)
	}
}
