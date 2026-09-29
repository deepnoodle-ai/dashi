// Package delegate lets an agent hand focused work to subagents that run in
// the background. The model calls the "task" tool with a subagent name and a
// prompt. The call returns at once with a task ID, and the agent keeps
// working. The model can later call "task_status" to read the state or the
// answer, and "task_stop" to end a task. When a task ends, the host gets an
// event and can send the result to the agent as a message.
//
// A Tasks value owns the running children. The host creates one for each
// conversation, so a child does not depend on one tool call or one Turn.
//
// Each child is a full Dashi turn with its own record. The record names the
// parent tool call (dashi.Start.Parent), and the child's ID comes from that
// call (ChildID). If the recorder can load turns (a dashi.Loader), Tasks.Recover
// finds the unfinished children of a reopened conversation and resumes them.
// Report adds the children's spend to the parent's.
//
// A child inherits from the runner that runs the task call, at call time: the
// client, recorder, hooks, and the parent's current model unless the Subagent
// sets its own. Hosts wire it in a few lines:
//
//	tasks := delegate.NewTasks(delegate.Config{OnTask: onTask})
//	runner.Tools = tool.Join(runner.Tools, tasks.Tools())
//	err := tasks.Recover(ctx, runner, turns) // after a reopen
//	defer tasks.Close(ctx)
package delegate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Names of the tools that Tasks.Tools provides.
const (
	// ToolName starts a task.
	ToolName = "task"
	// StatusToolName reads the state or the answer of a task.
	StatusToolName = "task_status"
	// StopToolName stops a task.
	StopToolName = "task_stop"
)

// Subagent is a kind of child that the model can choose.
type Subagent struct {
	// Name is what the model passes as "subagent", such as "explore". It
	// also becomes the child's Agent.ID when that is empty, so a hook that
	// reads TurnView.Agent.ID can tell which subagent asks for a tool.
	Name string
	// Description tells the model when to choose this subagent.
	Description string
	// Agent holds the instructions, model, and limits of the child turn.
	// Limits bound the child's work; delegate adds no timeout of its own.
	Agent dashi.Agent
	// Tools names the parent tools the child may use. Empty means every
	// parent tool. The child never receives the task tool, so a child
	// cannot start a child.
	Tools []string
	// ReadOnly limits the child to tools annotated read-only. It applies
	// with Tools. A tool without the annotation is excluded, because an
	// unknown effect is not a safe one.
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
			Limits: dashi.Limits{ModelAttempts: 24, ToolConcurrency: 4},
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
			Limits: dashi.Limits{ModelAttempts: 32},
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
	// Agent and Tools are ignored. A Recorder should implement dashi.Loader.
	// Without one, children run but do not survive a restart.
	Runner dashi.Runner
	// MaxAnswer caps the bytes of a child's answer that reach the model,
	// in a notice or a status result. Zero means 16 KiB; a negative value
	// means no cap.
	MaxAnswer int
	// OnChild receives every child event for live display: committed steps,
	// model deltas, and tool progress. The Observer in Runner, if set, also
	// receives each event, after OnChild. It runs on the child's observer
	// goroutine and must return promptly.
	OnChild func(ChildEvent)
	// OnTask receives a TaskStarted event when a task starts or resumes and
	// a TaskEnded event when it ends. The host decides how a result reaches
	// the agent (see Notice). It runs on the task's goroutine and must
	// return promptly. TaskEnded fires once for each task in one process.
	// It does not fire when Close interrupts a task.
	OnTask func(TaskEvent)
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

// ChildID returns the ID of the child turn for one parent tool call. The
// result depends only on the two arguments, and different pairs give
// different IDs, so the host stores nothing. With a Loader recorder, a retry
// finds the same child.
// A host that wants its own ID can call Runner.Run with Start itself.
func ChildID(parentTurnID, callID string) string {
	// The length prefix keeps ("a", "b:c") apart from ("a:b", "c").
	return fmt.Sprintf(
		"child-%d:%s:%s",
		len(parentTurnID),
		parentTurnID,
		callID,
	)
}

// Report sums the spend of parent and of the children its task calls started.
// It finds children only through those calls, by ChildID: a child that a host
// started with its own ID is not counted. Each child counts once, however
// many times it resumed. Parent comes from parent.Spend, which never holds
// child spend.
func Report(
	ctx context.Context,
	r dashi.Loader,
	parent dashi.TurnView,
) (SpendReport, error) {
	report := SpendReport{Parent: parent.Spend.Clone()}
	for _, callID := range slices.Sorted(maps.Keys(parent.Tools)) {
		if parent.Tools[callID].Call.Name != ToolName {
			continue
		}
		turn, err := r.Load(ctx, ChildID(parent.ID, callID))
		if errors.Is(err, dashi.ErrTurnNotFound) {
			continue
		}
		if err != nil {
			return SpendReport{}, fmt.Errorf("delegate: load child: %w", err)
		}
		view, err := dashi.Replay(turn)
		if err != nil {
			return SpendReport{}, fmt.Errorf("delegate: replay child: %w", err)
		}
		report.Children = report.Children.Add(view.Spend)
	}
	report.Inclusive = report.Parent.Add(report.Children)
	return report, nil
}

// filtered is the child's view of the parent toolset. It resolves the parent
// on every model call, so a dynamic toolset such as an MCP server keeps its
// live behavior inside the child.
type filtered struct {
	parent tool.Toolset
	sub    Subagent
}

func (f filtered) Tools(ctx context.Context) ([]tool.Tool, error) {
	if f.parent == nil {
		return nil, nil
	}
	all, err := f.parent.Tools(ctx)
	if err != nil {
		return nil, err
	}
	var out []tool.Tool
	for _, t := range all {
		name := t.Definition.Name
		readOnly := t.Definition.Annotations.ReadOnly
		switch {
		case name == ToolName || name == StatusToolName || name == StopToolName:
		case len(f.sub.Tools) > 0 && !slices.Contains(f.sub.Tools, name):
		case f.sub.ReadOnly && (readOnly == nil || !*readOnly):
		default:
			out = append(out, t)
		}
	}
	return out, nil
}
