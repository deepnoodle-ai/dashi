package llm

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi/content"
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
	Name string       `json:"name"`
	Tier ReminderTier `json:"tier"`
	// From names the sender of an agent message, such as "parent" or
	// "task t_k3f2 (explore)". Dashi sets it from the sending tool's binding,
	// never from model text. It is optional, at most 64 bytes of printable
	// UTF-8, and holds no quote or angle bracket.
	From    string `json:"from,omitempty"`
	Content string `json:"content"`
}

// Validate checks the reminder name, tier, and content before use.
func (r Reminder) Validate() error {
	if len(r.Name) > 64 || !reminderName.MatchString(r.Name) {
		return errors.New("llm: reminder name must match [a-z][a-z0-9-]* and be at most 64 bytes")
	}
	if r.Tier != ReminderContextual && r.Tier != ReminderOperator {
		return errors.New("llm: invalid reminder tier")
	}
	if !validReminderFrom(r.From) {
		return errors.New(
			"llm: reminder from must be at most 64 bytes of printable UTF-8 with no quote or angle bracket",
		)
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

func validReminderFrom(from string) bool {
	if len(from) > 64 || !utf8.ValidString(from) {
		return false
	}
	for _, r := range from {
		if !unicode.IsPrint(r) || strings.ContainsRune(`"<>`, r) {
			return false
		}
	}
	return true
}

// FormatReminder is the provider-facing text representation. The tag alone
// does not grant system authority; the prepared message role does. A
// nonempty From is written as a from attribute after the name. Content that
// holds a reminder tag has that tag's "<" written as "&lt;", so no content
// can close its reminder or open another; see [ReminderEscapes].
func FormatReminder(r Reminder) string {
	from := ""
	if r.From != "" {
		from = ` from="` + r.From + `"`
	}
	return fmt.Sprintf(
		"<system-reminder name=%q%s>\n%s\n</system-reminder>",
		r.Name,
		from,
		escapeReminderTags(r.Content),
	)
}

// ReminderEscapes reports whether FormatReminder escapes a reminder tag in
// the content of r.
func ReminderEscapes(r Reminder) bool {
	return escapeReminderTags(r.Content) != r.Content
}

// escapeReminderTags replaces "<" with "&lt;" where it starts
// "<system-reminder" or "</system-reminder", ASCII case folded.
func escapeReminderTags(s string) string {
	var sb strings.Builder
	rest := s
	for {
		i := strings.IndexByte(rest, '<')
		if i < 0 {
			break
		}
		tag := strings.TrimPrefix(rest[i+1:], "/")
		if len(tag) >= len("system-reminder") &&
			strings.EqualFold(tag[:len("system-reminder")], "system-reminder") {
			sb.WriteString(rest[:i])
			sb.WriteString("&lt;")
		} else {
			sb.WriteString(rest[:i+1])
		}
		rest = rest[i+1:]
	}
	if sb.Len() == 0 {
		return s
	}
	sb.WriteString(rest)
	return sb.String()
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
