package llm

import (
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/deepnoodle-ai/kernel/content"
)

// ReminderTier describes the authority of runtime context, not its lifetime.
type ReminderTier string

const (
	// ReminderContextual is host context with no operator authority.
	ReminderContextual ReminderTier = "contextual"
	// ReminderOperator is context supplied with operator authority.
	ReminderOperator ReminderTier = "operator"
)

var reminderName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Reminder is named context supplied by a host, library, or tool.
type Reminder struct {
	Name    string       `json:"name"`
	Tier    ReminderTier `json:"tier"`
	Content string       `json:"content"`
}

// Validate checks the reminder name, tier, and content before use.
func (r Reminder) Validate() error {
	if len(r.Name) > 64 || !reminderName.MatchString(r.Name) {
		return errors.New("llm: reminder name must match [a-z][a-z0-9-]* and be at most 64 bytes")
	}
	if r.Tier != ReminderContextual && r.Tier != ReminderOperator {
		return errors.New("llm: invalid reminder tier")
	}
	if r.Content == "" ||
		!utf8.ValidString(r.Content) ||
		len(r.Content) > 64<<10 {
		return errors.New("llm: reminder content must be nonempty UTF-8 and at most 64 KiB")
	}
	return nil
}

// ReminderMessage keeps runtime context typed in records and caller history.
// The provider adapter chooses the model-facing role during preparation.
func ReminderMessage(r Reminder) Message {
	v := r
	return Message{
		Role: RoleUser,
		Content: []Content{
			{
				Kind:     ContentReminder,
				Reminder: &v,
			},
		},
	}
}

// FormatReminder is the provider-facing text representation. The tag alone
// does not grant system authority; the prepared message role does.
func FormatReminder(r Reminder) string {
	return fmt.Sprintf(
		"<system-reminder name=%q>\n%s\n</system-reminder>",
		r.Name,
		r.Content,
	)
}

// ReminderPreparedRole returns the authority this destination can encode for
// a typed reminder. Other routes receive tagged user content. Model policy
// must still be enforced outside the prompt.
func ReminderPreparedRole(r Reminder, origin content.Origin) Role {
	if r.Tier == ReminderOperator &&
		origin.Provider == "openai" &&
		origin.API == "openai.responses" {
		return RoleDeveloper
	}
	return RoleUser
}
