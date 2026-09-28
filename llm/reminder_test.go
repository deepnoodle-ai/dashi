package llm

import (
	"strings"
	"testing"
)

func TestReminderValidationAndOwnership(t *testing.T) {
	r := Reminder{
		Name:    "workspace-map",
		Tier:    ReminderContextual,
		Content: "Use the current map.",
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	m := ReminderMessage(r)
	r.Content = "changed"
	if m.Content[0].Reminder.Content != "Use the current map." {
		t.Fatal("reminder message did not own the value")
	}
	if err := (Input{
		Model: "m",
		Messages: []Message{
			m,
		},
	}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Reminder{
		{Name: "Bad Name", Tier: ReminderContextual},
		{Name: "x", Tier: "system"},
		{Name: "x", Tier: ReminderContextual},
		{
			Name:    "x",
			Tier:    ReminderOperator,
			Content: string([]byte{0xff}),
		},
		{
			Name: "x",
			Tier: ReminderOperator,
			Content: strings.Repeat(
				"x",
				64<<10+1,
			),
		},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("accepted invalid reminder: %+v", bad)
		}
	}
}

func TestReminderRequiresStandaloneUserMessage(t *testing.T) {
	r := Reminder{
		Name:    "x",
		Tier:    ReminderOperator,
		Content: "instructions",
	}
	for _, m := range []Message{
		{Role: RoleDeveloper, Content: ReminderMessage(r).Content},
		{
			Role: RoleUser,
			Content: append(
				ReminderMessage(r).Content,
				Text("other"),
			),
		},
	} {
		if err := (Input{
			Model: "m",
			Messages: []Message{
				m,
			},
		}).Validate(); err == nil {
			t.Fatalf("accepted mixed or elevated reminder: %+v", m)
		}
	}
	if got := FormatReminder(r); !strings.Contains(
		got,
		`<system-reminder name="x">`,
	) || !strings.Contains(
		got,
		"instructions",
	) {
		t.Fatal(got)
	}
}
