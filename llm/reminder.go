package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"unicode/utf8"

	"github.com/deepnoodle-ai/kernel/content"
)

// ReminderTier describes the authority of runtime context, not its lifetime.
type ReminderTier string

const (
	ReminderContextual ReminderTier = "contextual"
	ReminderOperator   ReminderTier = "operator"
)

var reminderName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Reminder is typed context supplied by a host, library, or tool. Details are
// application metadata and never appear in the model-facing text.
type Reminder struct {
	Name    string          `json:"name"`
	Tier    ReminderTier    `json:"tier"`
	Content string          `json:"content"`
	Details json.RawMessage `json:"details,omitempty"`
}

func (r Reminder) Validate() error {
	if len(r.Name) > 64 || !reminderName.MatchString(r.Name) {
		return errors.New("llm: reminder name must match [a-z][a-z0-9-]* and be at most 64 bytes")
	}
	if r.Tier != ReminderContextual && r.Tier != ReminderOperator {
		return errors.New("llm: invalid reminder tier")
	}
	if !utf8.ValidString(r.Content) || len(r.Content) > 64<<10 {
		return errors.New("llm: reminder content must be UTF-8 and at most 64 KiB")
	}
	if len(r.Details) > 16<<10 || (len(r.Details) > 0 && (!json.Valid(r.Details) || bytes.TrimSpace(r.Details)[0] != '{')) {
		return errors.New("llm: reminder details must be a JSON object of at most 16 KiB")
	}
	return nil
}

func (r Reminder) Clone() Reminder {
	r.Details = slices.Clone(r.Details)
	return r
}

// ReminderMessage keeps runtime context typed in records and caller history.
// The provider adapter chooses the model-facing role during preparation.
func ReminderMessage(r Reminder) Message {
	v := r.Clone()
	return Message{Role: RoleUser, Content: []Content{{Kind: ContentReminder, Reminder: &v}}}
}

// FormatReminder is the provider-facing text representation. The tag alone
// does not grant system authority; the prepared message role does.
func FormatReminder(r Reminder) string {
	return fmt.Sprintf("<system-reminder name=%q>\n%s\n</system-reminder>", r.Name, r.Content)
}

// ReminderPreparedRole returns the authority this destination can encode for
// a typed reminder. Other routes receive tagged user content. Model policy
// must still be enforced outside the prompt.
func ReminderPreparedRole(r Reminder, origin content.Origin) Role {
	if r.Tier == ReminderOperator && origin.Provider == "openai" && origin.API == "openai.responses" {
		return RoleDeveloper
	}
	return RoleUser
}
