package delegate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// A notice tells the agent that a task ended. Delivery has one rule: the
// result is delivered when the record of the conversation holds the notice
// for the child. Either a turn recorded a context item with the ID
// NoticeID(child), or a session's queue turn started with it: its command
// ID is "queue:" and the JSON list of the item IDs it carried (see
// dashi.Session.RunQueue). A task_status read does not count. Recover and
// the live path use this same check.
const (
	noticePrefix = "task-done:"
	queuePrefix  = "queue:"
)

// NoticeID returns the context item ID that records the notice for a child.
func NoticeID(childID string) string {
	return noticePrefix + childID
}

// Notice is the message that tells the agent a task ended.
type Notice struct {
	Task TaskView
}

// Notice returns the notice for an ended task.
func (v TaskView) Notice() Notice {
	return Notice{Task: v}
}

// Text returns the message for the model, such as
// "[task t_k3f2 completed] explore: find the parser".
// The answer or the reason follows on the next lines.
func (n Notice) Text() string {
	v := n.Task
	var sb strings.Builder
	fmt.Fprintf(&sb, "[task %s %s] %s: %s", v.ID, v.State, v.Subagent, v.Description)
	if v.Answer != "" {
		sb.WriteString("\n" + v.Answer)
	}
	if v.Reason != "" {
		sb.WriteString("\nReason: " + v.Reason)
	}
	return sb.String()
}

// Steer returns the notice as a steer: send it to a running turn with
// dashi.Session.SteerItem, which queues it for a queue turn when no turn
// runs, or queue its Item with dashi.Session.EnqueueItem. Its origin is
// dashi.ContextTool, so it never carries user or operator authority. Its ID
// is NoticeID of the child, so a turn records it once.
func (n Notice) Steer() dashi.Steer {
	return dashi.Steer{Item: dashi.ContextItem{
		ID:      NoticeID(n.Task.ChildID),
		Origin:  dashi.ContextTool,
		Message: llm.UserMessage(llm.Text(n.Text())),
	}}
}

// deliveredNotices returns the children whose notice the turns record.
func deliveredNotices(turns []dashi.Turn) map[string]bool {
	out := map[string]bool{}
	for _, turn := range turns {
		for _, step := range turn.Steps {
			for _, item := range step.Items() {
				if id, ok := strings.CutPrefix(item.ID, noticePrefix); ok {
					out[id] = true
				}
			}
			if list, ok := strings.CutPrefix(step.CommandID, queuePrefix); ok &&
				step.Kind == dashi.StepStarted {
				var ids []string
				if json.Unmarshal([]byte(list), &ids) == nil {
					for _, id := range ids {
						if child, ok := strings.CutPrefix(id, noticePrefix); ok {
							out[child] = true
						}
					}
				}
			}
		}
	}
	return out
}
