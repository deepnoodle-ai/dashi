// Package delegate lets an agent hand focused work to subagents that run in
// the background. The model calls the "task" tool with a subagent name and a
// prompt. The call returns at once with a task ID, and the agent keeps
// working. The model can later call "task_status" to read the state or the
// reply, "task_send" to send the task a message, and "task_stop" to end a
// task. A message steers into the task's running turn, or starts its next
// turn, so a finished task takes a follow-up with its memory. A task sends
// its parent a message with "message_parent", the one tool a child gets
// from delegate. When a turn of a task ends, the agent gets a notice. Tasks
// puts notices and messages on Config.Session, or the host routes them from
// Config.OnTask. A notice whose result a recorded task_status call already
// returned is not delivered again (see DeliveredNotices).
//
// A Tasks value owns the running children. The host creates one for each
// conversation, so a child does not depend on one tool call or one Turn.
//
// Each task runs in a child session (session.Session) whose ID comes from the
// parent tool call (ChildID), so its turns are "<child>-1", "<child>-2", and
// so on, each with its own record. A child turn's record names the parent
// call that started it (dashi.Start.Parent). If the recorder can load turns
// (a dashi.Loader), Tasks.Recover finds the unfinished child turns of a
// reopened conversation and resumes them. Report adds the children's spend
// to the parent's.
//
// A child inherits from the runner that runs the task call, at call time: the
// client, recorder, and hooks, plus the parent turn's current model
// (dashi.CurrentTurn) unless the Subagent sets its own. Hosts wire it in a few
// lines:
//
//	tasks := delegate.NewTasks(delegate.Config{Session: s})
//	s.Runner.Tools = tool.Join(s.Runner.Tools, tasks.Tools())
//	err := tasks.Recover(ctx, s.Runner, turns) // after a reopen
//	defer tasks.Close(ctx)
package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/session"
)

// Names of the tools that Tasks.Tools provides.
const (
	// ToolName starts a task.
	ToolName = "task"
	// StatusToolName reads the state or the reply of a task.
	StatusToolName = "task_status"
	// StopToolName stops a task.
	StopToolName = "task_stop"
	// SendToolName sends a message to a task.
	SendToolName = "task_send"
	// MessageParentToolName sends the parent a message. Only a task's child
	// session has it, bound to that task.
	MessageParentToolName = "message_parent"
)

// Subagent is a kind of child that the model can choose.
type Subagent struct {
	// Name is what the model passes as "subagent", such as "explore". It
	// also becomes the child's Agent.ID when that is empty, so a hook that
	// reads TurnView.Agent.ID can tell which subagent asks for a tool.
	Name string
	// Description tells the model when to choose this subagent.
	Description string
	// Agent holds the instructions, model, tools, and limits of the child
	// turn, and becomes its Start.Agent. Limits bound the child's work;
	// delegate adds no timeout of its own. Agent.Tools names the tools of
	// the parent's Runner.Tools that the child may use, whether or not the
	// parent agent selected them; empty means every tool the parent agent
	// may use. The child never receives the task tool, so a child cannot
	// start a child. At child start, delegate records the resolved names in
	// the child's Agent.Tools, so a resumed child keeps them.
	Agent dashi.Agent
	// ReadOnly limits the child to tools annotated read-only. It applies
	// with Agent.Tools. A tool without the annotation is excluded, because
	// an unknown effect is not a safe one.
	ReadOnly bool
}

// Explore returns a read-only subagent for searching and reading. It uses
// only tools annotated read-only, so MCP tools that declare the hint are
// included and the rest are not.
func Explore() Subagent {
	return Subagent{
		Name: "explore",
		Description: "Searches and reads to answer a question about the " +
			"codebase. It cannot change anything. Use it when the answer " +
			"needs many reads that would fill your context.",
		Agent: dashi.Agent{
			Instructions: "You are a read-only research subagent. Search " +
				"and read to answer the request. Do not change anything. " +
				"Your final message is the only thing the parent agent " +
				"sees. Make it short. Name the files and lines that matter.",
			Limits: dashi.Limits{ModelCalls: 24, ToolConcurrency: 4},
		},
		ReadOnly: true,
	}
}

// General returns a subagent that can use every parent tool. Use it for
// multi-step work that the parent does not need to watch.
func General() Subagent {
	return Subagent{
		Name: "general",
		Description: "Completes a multi-step task with every tool you have. " +
			"Use it for work that needs many steps and only one result.",
		Agent: dashi.Agent{
			Instructions: "You are a subagent. Complete the task in the " +
				"request. Your final message is the only thing the parent " +
				"agent sees. State the result and each change you made.",
			Limits: dashi.Limits{ModelCalls: 32},
		},
	}
}

// Config configures the tasks. The zero value works.
type Config struct {
	// Subagents is the catalog the model chooses from. Empty means Explore
	// and General.
	Subagents []Subagent
	// Runner holds optional overrides for child turns. Each child inherits
	// from the runner that runs the task call. Set Client, Recorder, or
	// Observer here only to make children differ from the parent; a nonzero
	// field replaces the inherited value. Hooks are always inherited, so the
	// host's approval prompt covers child tool calls. OnContextDone is
	// ignored: Tasks interrupts a child on Close and cancels it on Stop.
	// Tools is ignored. A Recorder should implement dashi.Loader.
	// Without one, children run but do not survive a restart.
	Runner dashi.Runner
	// MaxReply caps the bytes of a child's reply that reach the model,
	// in a notice or a status result. Zero means 16 KiB; a negative value
	// means no cap.
	MaxReply int
	// OnChild receives every child event for live display: committed steps,
	// model deltas, and tool progress. The Observer in Runner, if set, also
	// receives each event, after OnChild. It runs on the child's observer
	// goroutine and must return promptly.
	OnChild func(ChildEvent)
	// OnTask receives a TaskStarted event when a child turn starts or
	// resumes, a TaskEnded event when it ends, and a TaskMessage event when
	// a task sends its parent a message. Without Session, the host decides
	// how a result or a message reaches the agent (see Notice and Message).
	// It runs on the task's goroutine and must return promptly. TaskEnded
	// fires once for each child turn in one process. It does not fire when
	// Close interrupts a turn.
	OnTask func(TaskEvent)
	// Session, when set, is the parent's session, and Tasks puts each
	// notice and message on it before OnTask runs: SteerItem reaches the
	// running turn, or queues the item for a queue turn when no turn runs
	// (see RunUntilQuiet). A session with a host Steering source, or one
	// that is compacting, gets the item on its queue. One child turn wakes
	// an idle session at most once by message: its later messages join its
	// notice. NewTasks sets the session's Delivered function, so a queue
	// turn drops a notice or message that the record shows delivered. Nil
	// means the host routes notices and messages from OnTask.
	Session *session.Session
	// MaxTaskTurns bounds the turns of one child session. Zero means 10; a
	// negative value means no bound. Past it, task_send refuses.
	MaxTaskTurns int
	// MaxMessages bounds the messages that one sending turn sends to one
	// recipient: a parent turn to one task, or a child turn to its parent.
	// Zero means 8; a negative value means no bound.
	MaxMessages int
	// PrepareStart may append checked evidence and pin generation settings,
	// active compaction, and the agent version before a new child is recorded.
	// It receives an owned Start outside the task lock. It cannot change the
	// task prompt, parent, destination, instructions, tools, limits, or budget.
	// Existing recorded children never call it.
	PrepareStart func(context.Context, dashi.CallRef, dashi.Start) (dashi.Start, error)
	// BindServices selects immutable services for a new or recovered child.
	// Nil fields inherit. Recorder, hooks, tools, and observation stay owned
	// by Tasks. It runs outside the task lock, with an owned agent snapshot.
	BindServices func(context.Context, dashi.CallRef, dashi.Agent) (ChildServices, error)
	// NoticeReady defers final notices for a stopped child when false. Live
	// recorded steps still reach OnChild. Nil preserves existing behavior.
	// Wait returns AttentionError when only deferred stopped work remains.
	NoticeReady func(dashi.TurnView) bool
}

// ChildEvent is one event of a child turn, with the link to its parent.
type ChildEvent struct {
	// Parent is the parent turn and the task call that runs the child.
	Parent dashi.CallRef
	// Subagent is the catalog name the model chose.
	Subagent string
	// Event is the child's own event; Event.TurnID is the child turn.
	Event dashi.Event
}

// SpendReport splits spend between a parent turn and its children.
type SpendReport struct {
	// Parent is the parent turn's own spend.
	Parent dashi.Spend
	// Children is the sum over the children the parent's task calls started.
	Children dashi.Spend
	// Inclusive is Parent plus Children, each counted once.
	Inclusive dashi.Spend
}

// ChildID returns the ID of the child session for one parent tool call. Its
// turns are session.TurnID(ChildID(…), n). The result depends only on the two
// arguments, and different pairs give different IDs, so the host stores
// nothing. With a Loader recorder, a retry finds the same child.
// A host that wants its own ID can drive a session.Session itself.
func ChildID(parentTurnID, callID string) string {
	// The length prefix keeps ("a", "b:c") apart from ("a:b", "c").
	return fmt.Sprintf(
		"child-%d:%s:%s",
		len(parentTurnID),
		parentTurnID,
		callID,
	)
}

// Report sums the spend of parent and of the child turns its calls started.
// It finds the child sessions that the parent's task calls start (ChildID)
// and that its finished task_send calls name, and counts each child turn
// whose starting call is a call of parent: its recorded Parent, or, for a
// queue turn that messages started, the call that sent the first message
// in its key. So a child turn that another parent turn started counts
// there, not here. A child session that a host started with its own ID is
// not counted, nor one that only a task_send call whose result a hook
// replaced names. Each child turn counts once, however many times it
// resumed. Parent comes from parent.Spend, which never holds child spend.
func Report(
	ctx context.Context,
	r dashi.Loader,
	parent dashi.TurnView,
) (SpendReport, error) {
	report := SpendReport{Parent: parent.Spend.Clone()}
	var sessions []string
	for _, callID := range slices.Sorted(maps.Keys(parent.Tools)) {
		id := ""
		switch tv := parent.Tools[callID]; tv.Call.Name {
		case ToolName:
			id = ChildID(parent.ID, callID)
		case SendToolName:
			var d sendData
			if tv.Outcome != nil && len(tv.Outcome.Data) > 0 &&
				json.Unmarshal(tv.Outcome.Data, &d) == nil {
				id = d.SessionID
			}
		}
		if id != "" && !slices.Contains(sessions, id) {
			sessions = append(sessions, id)
		}
	}
	for _, id := range sessions {
		rec, err := loadRecord(ctx, r, id)
		if err != nil {
			return SpendReport{}, fmt.Errorf("delegate: load child: %w", err)
		}
		for i, view := range rec.views {
			if startedIn(rec.turns[i], view, parent.ID) {
				report.Children = report.Children.Add(view.Spend)
			}
		}
	}
	report.Inclusive = report.Parent.Add(report.Children)
	return report, nil
}

// startedIn reports whether a call of parent turn parentID started a child
// turn: its recorded Parent, or, for a queue turn, the call that sent the
// first message in its key.
func startedIn(turn dashi.Turn, view dashi.TurnView, parentID string) bool {
	if view.Parent != nil {
		return view.Parent.TurnID == parentID
	}
	if len(turn.Steps) == 0 {
		return false
	}
	for _, id := range queueKeyIDs(turn.Steps[0].CommandID) {
		if strings.HasPrefix(id, messagePrefix) {
			return strings.HasPrefix(id, MessageID(parentID, ""))
		}
	}
	return false
}
