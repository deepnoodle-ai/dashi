package delegate

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// A notice tells the agent that a task ended. Delivery has one rule: the
// result is delivered when the record of the conversation holds the notice
// for the child. Either a turn recorded a context item with the ID
// NoticeID(child), or a follow-up turn (see FollowUp) started with it. A
// task_status read does not count. Recover and the live path use this same
// check.
const (
	noticePrefix   = "task-done:"
	followUpPrefix = "task-followup:"
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

// Steer returns the notice as a steer for a running turn: send it to a
// dashi.Inbox. Its origin is dashi.ContextTool, so it never carries user
// or operator authority. Its ID is NoticeID of the child, so a turn records
// it once.
func (n Notice) Steer() dashi.Steer {
	return dashi.Steer{Item: dashi.ContextItem{
		ID:      NoticeID(n.Task.ChildID),
		Origin:  dashi.ContextTool,
		Message: llm.UserMessage(llm.Text(n.Text())),
	}}
}

// IsNotice reports whether a steer is a task notice. A host uses it to split
// Inbox.Leftovers: user steers become the next user turn, and notices go to
// the next turn that runs or to a follow-up turn (see FollowUp).
func IsNotice(s dashi.Steer) bool {
	return s.Item.Origin == dashi.ContextTool &&
		strings.HasPrefix(s.Item.ID, noticePrefix)
}

// noticeChildIDs returns the sorted, distinct child IDs of notice steers.
func noticeChildIDs(notices []dashi.Steer) []string {
	ids := make([]string, len(notices))
	for i, n := range notices {
		ids[i] = strings.TrimPrefix(n.Item.ID, noticePrefix)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// FollowUpTurnID returns the turn ID for a follow-up turn that delivers the
// notice steers. It depends only on the set of children, so a retry finds
// the same turn.
func FollowUpTurnID(notices []dashi.Steer) string {
	list, _ := json.Marshal(noticeChildIDs(notices))
	sum := sha256.Sum256(list)
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString(sum[:])
	return "followup-" + strings.ToLower(enc[:16])
}

// FollowUp returns the Start for a turn that delivers notice steers when no
// turn runs. They come from Inbox.Leftovers, filtered with IsNotice, or from
// TaskView.Notice().Steer(). One turn carries all waiting notices. The start
// is scheduled work: it claims no user authority. agent becomes Start.Agent;
// pass the conversation's agent, since a zero agent runs with the defaults.
// The host sets History and passes FollowUpTurnID(notices) as the turn ID.
// The start records the children it delivers, so Recover does not report them
// again. The list must not be empty.
func FollowUp(agent dashi.Agent, notices []dashi.Steer) dashi.Start {
	texts := make([]string, len(notices))
	for i, n := range notices {
		texts[i] = n.Item.Message.Text()
	}
	list, _ := json.Marshal(noticeChildIDs(notices))
	return dashi.Start{
		CommandID: followUpPrefix + string(list),
		Origin:    dashi.StartScheduled,
		Agent:     agent,
		Input: []llm.Message{
			llm.UserMessage(llm.Text(strings.Join(texts, "\n\n"))),
		},
	}
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
			if list, ok := strings.CutPrefix(step.CommandID, followUpPrefix); ok &&
				step.Kind == dashi.StepStarted {
				var ids []string
				if json.Unmarshal([]byte(list), &ids) == nil {
					for _, id := range ids {
						out[id] = true
					}
				}
			}
		}
	}
	return out
}
