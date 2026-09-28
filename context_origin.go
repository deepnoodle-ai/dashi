package dashi

import "cmp"

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
