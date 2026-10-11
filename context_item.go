package dashi

import (
	"cmp"

	"github.com/deepnoodle-ai/dashi/llm"
)

// ContextOrigin records who supplied a ContextItem, independently of its
// message role. It decides the roles and reminder tiers the item may use:
// only operator context may speak with system or developer authority. The
// empty value means ContextUser, the least trusted origin.
type ContextOrigin string

// Context origins.
const (
	// ContextUser is content from the end user, such as a follow-up message.
	// It must use the user role.
	ContextUser ContextOrigin = "user"

	// ContextRuntime is context the application's runtime adds on its own
	// rather than on anyone's behalf, such as environment state or a
	// contextual reminder. It is trusted no more than user content and must
	// use the user role.
	ContextRuntime ContextOrigin = "runtime"

	// ContextTool is content a tool produced outside its call result, such as
	// a notification from a long-running job. It must use the user role.
	ContextTool ContextOrigin = "tool"

	// ContextAgent is content another agent in this session tree sent, such
	// as a parent's message to its subagent. It must be one contextual
	// reminder in the user role, with a sender in From.
	ContextAgent ContextOrigin = "agent"

	// ContextOperator is instruction from the application's operator, who
	// configures the agent. It may use the user, system, or developer role
	// and is the only origin allowed operator-tier reminders.
	ContextOperator ContextOrigin = "operator"
)

// contextKey identifies the snapshot series an item replaces, treating an
// empty origin as ContextUser.
func contextKey(item ContextItem) string {
	return string(cmp.Or(item.Origin, ContextUser)) + "\x00" + item.Name
}

// ContextItem carries content whose origin and replacement behavior are
// independent of its message role. All content needed for recovery is recorded.
type ContextItem struct {
	// ID identifies one addition of this item. A turn records an ID at most
	// once, and a repeat with the same content is skipped, so a source that
	// retries adding it cannot add it twice. Name, by contrast, names a
	// slot that a later snapshot replaces. ID is optional for Start.Context,
	// AddMessages, and
	// hook items and required for steers.
	ID string `json:"id,omitempty"`
	// Name identifies an item that a later snapshot may replace.
	Name string `json:"name,omitempty"`
	// Origin records who supplied the item; empty means ContextUser.
	Origin  ContextOrigin `json:"origin"`
	Message llm.Message   `json:"message"`
	// Snapshot supersedes an earlier item with the same name: Turn.Context
	// returns this one. Model requests keep the earlier item in place and
	// add this one after it, so the prompt prefix and the provider's cache
	// survive; the reminder instruction tells the model the latest is
	// current.
	Snapshot bool `json:"snapshot,omitempty"`
	// NextCallOnly keeps this item until a later model call shows evidence
	// of dispatch (a provider response or rejection), then omits it. A call
	// with no such evidence, such as one recovered after a crash, is resent
	// with the item, so the model may see it twice rather than never. A
	// call an urgent steer cut short does not expire it either: the
	// replacement call sees it again. For a value that must reflect each
	// call, such as the current time, use a named Snapshot item instead.
	NextCallOnly bool `json:"next_call_only,omitempty"`
}

func cloneContextItem(item ContextItem) ContextItem {
	item.Message = item.Message.Clone()
	return item
}
