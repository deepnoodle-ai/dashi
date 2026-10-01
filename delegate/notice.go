package delegate

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// A notice tells the agent that a turn of a task ended. Delivery has one
// rule: the notice is delivered when the record of the conversation holds
// it for that child turn. Either a turn recorded a context item with the ID
// NoticeID(turn), or a session's queue turn started with it: its command ID
// is "queue:" and the JSON list of the item IDs it carried (see
// session.Session.RunQueue). A task_status read does not count. Recover and
// the live path use this same check.
const (
	noticePrefix = "task-done:"
	queuePrefix  = "queue:"
	// noticeName is the reminder name of a notice.
	noticeName = "task-ended"
	// maxNotice leaves room in a reminder's 64 KiB for the cut marker.
	maxNotice = 64<<10 - 64
	// maxFrom is the byte limit of llm.Reminder.From.
	maxFrom = 64
)

// NoticeID returns the context item ID that records the notice for a child
// turn.
func NoticeID(turnID string) string {
	return noticePrefix + turnID
}

// Notice is the message that tells the agent a turn of a task ended.
type Notice struct {
	Task TaskView
}

// Notice returns the notice for the turn the view names.
func (v TaskView) Notice() Notice {
	return Notice{Task: v}
}

// Text returns the message for the model, such as
// "[task t_k3f2 completed] explore: find the parser".
// The reply or the reason follows on the next lines.
func (n Notice) Text() string {
	v := n.Task
	var sb strings.Builder
	fmt.Fprintf(&sb, "[task %s %s] %s: %s", v.ID, v.State, v.Subagent, v.Description)
	if v.Reply != "" {
		sb.WriteString("\n" + v.Reply)
	}
	if v.Reason != "" {
		sb.WriteString("\nReason: " + v.Reason)
	}
	return sb.String()
}

// Item returns the notice as a context item: a contextual "task-ended"
// reminder from the task, such as from="task t_k3f2 (explore)", with origin
// dashi.ContextAgent, so it never carries user or operator authority. Its
// ID is NoticeID of the child turn, so a turn records it once. Text past the
// reminder limit of 64 KiB is cut.
func (n Notice) Item() dashi.ContextItem {
	text := n.Text()
	if len(text) > maxNotice {
		cut := cutUTF8(text, maxNotice)
		text = fmt.Sprintf("%s\n[notice cut at %d bytes]", cut, len(cut))
	}
	return dashi.ContextItem{
		ID:     NoticeID(n.Task.TurnID),
		Origin: dashi.ContextAgent,
		Message: llm.ReminderMessage(llm.Reminder{
			Name:    noticeName,
			Tier:    llm.ReminderContextual,
			From:    taskSender(n.Task.ID, n.Task.Subagent),
			Content: text,
		}),
	}
}

// Steer returns the notice as a steer: send it to a running turn with
// session.Session.SteerItem, which queues it for a queue turn when no turn
// runs, or queue its Item with session.Session.EnqueueItem. Config.Session
// does this for the host.
func (n Notice) Steer() dashi.Steer {
	return dashi.Steer{Item: n.Item()}
}

// taskSender returns the sender of what a task sends, such as
// "task t_k3f2 (explore)". It drops the characters a reminder's From cannot
// hold and cuts the rest to 64 bytes, since a subagent name has no limits.
func taskSender(id, subagent string) string {
	from := strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) || strings.ContainsRune(`"<>`, r) {
			return -1
		}
		return r
	}, "task "+id+" ("+subagent+")")
	return cutUTF8(from, maxFrom)
}

// deliveredNotices returns the child turns whose notice the turns record.
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
				step.Kind == dashi.StepTurnStarted {
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
