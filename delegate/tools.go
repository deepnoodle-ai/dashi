package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/tool"
)

// maxWaitSeconds is the longest a task_status call waits.
const maxWaitSeconds = 60

// Tools returns the toolset that holds task, task_status, and task_stop.
// Join it with the tools the agent already has. Children draw from the tools
// of the runner that runs the call, filtered by each Subagent; they never
// receive these three tools, so a child cannot start a child.
//
// The task tool is marked idempotent when the recorder can load turns. After
// a crash, the runner runs the call again, and the same child continues from
// its record. Without such a recorder, a resumed parent reports the call as
// result_unknown and starts no second child.
func (ts *Tasks) Tools() tool.Toolset {
	return taskTools{ts}
}

type taskTools struct{ ts *Tasks }

// resumable reports whether a retry can find the child: the override
// recorder, else the recorder of the runner that asks for tools. Outside a
// runner it assumes the host will set one that loads turns.
func (t taskTools) resumable(ctx context.Context) bool {
	if r := t.ts.cfg.Runner.Recorder; r != nil {
		return loads(r)
	}
	if parent, ok := dashi.CurrentRunner(ctx); ok {
		return loads(parent.Recorder)
	}
	return true
}

func (t taskTools) Tools(ctx context.Context) ([]tool.Tool, error) {
	names := make([]string, len(t.ts.catalog))
	var list strings.Builder
	for i, s := range t.ts.catalog {
		if s.Name == "" {
			return nil, errors.New("delegate: subagent requires a name")
		}
		if slices.Contains(names[:i], s.Name) {
			return nil, fmt.Errorf("delegate: subagent %q is listed twice", s.Name)
		}
		names[i] = s.Name
		fmt.Fprintf(&list, "\n- %s: %s", s.Name, s.Description)
	}
	start := tool.Definition{
		Name: ToolName,
		Description: "Start a subagent on a focused task. It runs in the " +
			"background and this call returns at once with a task ID. Keep " +
			"working. A message arrives when the task ends. The subagent " +
			"starts with no memory of this conversation and returns one " +
			"answer. Subagents:" + list.String(),
		Schema: mustSchema(map[string]any{
			"subagent": map[string]any{
				"type":        "string",
				"enum":        names,
				"description": "Which subagent runs the task.",
			},
			"prompt": map[string]any{
				"type": "string",
				"description": "The complete task. The subagent cannot see " +
					"this conversation, so include every fact it needs.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "A label of a few words for display.",
			},
		}, "subagent", "prompt", "description"),
		Kind: tool.KindLocal,
	}
	if t.resumable(ctx) {
		start.Annotations.Idempotent = new(true)
	}
	status := tool.Definition{
		Name: StatusToolName,
		Description: "Read the state of background tasks. With task_id: " +
			"progress of a running task, or the state and answer of an " +
			"ended one. Without task_id: one line per task. wait_seconds " +
			"holds the call until the task ends (without task_id, until " +
			"the next task ends) or the time is up. Do not poll: a message " +
			"arrives when a task ends.",
		Schema: mustSchema(map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"description": "The task ID that the task tool returned.",
			},
			"wait_seconds": map[string]any{
				"type":        "integer",
				"minimum":     0,
				"maximum":     maxWaitSeconds,
				"description": "How long to wait. Default 0.",
			},
		}),
		Kind: tool.KindLocal,
	}
	status.Annotations.ReadOnly = new(true)
	stop := tool.Definition{
		Name:        StopToolName,
		Description: "Stop a running background task.",
		Schema: mustSchema(map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"description": "The task ID that the task tool returned.",
			},
		}, "task_id"),
		Kind: tool.KindLocal,
	}
	stop.Annotations.Idempotent = new(true)
	return []tool.Tool{
		{Definition: start, Executor: executor(t.ts.execStart)},
		{Definition: status, Executor: executor(t.ts.execStatus)},
		{Definition: stop, Executor: executor(t.ts.execStop)},
	}, nil
}

func mustSchema(props map[string]any, required ...string) json.RawMessage {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	b, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	return b
}

type executor func(context.Context, tool.Call) (tool.Outcome, error)

func (f executor) Execute(
	ctx context.Context,
	c tool.Call,
) (tool.Outcome, error) {
	return f(ctx, c)
}

type taskArgs struct {
	Subagent    string `json:"subagent"`
	Prompt      string `json:"prompt"`
	Description string `json:"description"`
}

func parseTaskArgs(raw []byte) (taskArgs, error) {
	var args taskArgs
	err := json.Unmarshal(raw, &args)
	return args, err
}

func (ts *Tasks) execStart(
	ctx context.Context,
	call tool.Call,
) (tool.Outcome, error) {
	args, err := parseTaskArgs(call.Args)
	if err != nil {
		return tool.Outcome{}, fmt.Errorf("delegate: read arguments: %w", err)
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return tool.Outcome{}, errors.New("delegate: prompt is required")
	}
	sub, ok := ts.find(args.Subagent)
	if !ok {
		return tool.Outcome{}, fmt.Errorf(
			"delegate: unknown subagent %q; choose one of: %s",
			args.Subagent,
			strings.Join(ts.names(), ", "),
		)
	}
	ref, ok := dashi.CurrentCall(ctx)
	if !ok {
		return tool.Outcome{}, tool.Fatal(errors.New(
			"delegate: the task tool must run under a dashi.Runner",
		))
	}
	parent, _ := dashi.CurrentRunner(ctx)
	// Without the parent turn the child would lose the parent's tool
	// selection and model, so fail rather than widen what the child may do.
	turn, ok := dashi.CurrentTurn(ctx)
	if !ok {
		return tool.Outcome{}, tool.Fatal(errors.New(
			"delegate: the task tool cannot read its parent turn",
		))
	}
	v, err := ts.startTask(ctx, sub, args, ref, parent, turn.Agent)
	if err != nil {
		if ctx.Err() != nil {
			return tool.Outcome{}, ctx.Err()
		}
		return tool.Outcome{}, err
	}
	if !v.Running() {
		return tool.Succeeded(fmt.Sprintf(
			"Task %s (%s): %s. It has already ended (%s). A message that "+
				"starts with \"[task %s\" carries the result. Do not poll.",
			v.ID, v.Subagent, v.Description, v.State, v.ID,
		)), nil
	}
	return tool.Succeeded(fmt.Sprintf(
		"Started task %s (%s): %s. It runs in the background. A message "+
			"that starts with \"[task %s\" arrives when it ends. Keep "+
			"working. Do not poll.",
		v.ID, v.Subagent, v.Description, v.ID,
	)), nil
}

type statusArgs struct {
	TaskID      string `json:"task_id"`
	WaitSeconds int    `json:"wait_seconds"`
}

func (ts *Tasks) execStatus(
	ctx context.Context,
	call tool.Call,
) (tool.Outcome, error) {
	var args statusArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return tool.Outcome{}, fmt.Errorf("delegate: read arguments: %w", err)
	}
	if args.WaitSeconds < 0 || args.WaitSeconds > maxWaitSeconds {
		return tool.Outcome{}, fmt.Errorf(
			"delegate: wait_seconds must be 0 to %d", maxWaitSeconds,
		)
	}
	wait := time.Duration(args.WaitSeconds) * time.Second
	if args.TaskID != "" {
		v, delivered, err := ts.awaitOne(ctx, args.TaskID, wait)
		if err != nil {
			return tool.Outcome{}, err
		}
		out := tool.Succeeded(describe(v, true))
		if delivered {
			// The record shows which result this call delivered, by turn ID
			// and not by text (see DeliveredNotices).
			out.Data, err = json.Marshal(statusData{DeliveredTurnID: v.TurnID})
			if err != nil {
				return tool.Outcome{}, err
			}
		}
		return out, nil
	}
	views := ts.awaitAny(ctx, wait)
	if len(views) == 0 {
		return tool.Succeeded("No tasks."), nil
	}
	lines := make([]string, len(views))
	for i, v := range views {
		lines[i] = describe(v, false)
	}
	return tool.Succeeded(strings.Join(lines, "\n")), nil
}

func (ts *Tasks) execStop(
	ctx context.Context,
	call tool.Call,
) (tool.Outcome, error) {
	var args statusArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return tool.Outcome{}, fmt.Errorf("delegate: read arguments: %w", err)
	}
	err := ts.stop(ctx, args.TaskID)
	switch {
	case errors.Is(err, ErrUnknownTask):
		return tool.Outcome{}, ts.unknown(args.TaskID)
	case errors.Is(err, ErrTaskEnded):
		v, _ := ts.get(args.TaskID)
		return tool.Succeeded(fmt.Sprintf(
			"Task %s already %s.", v.ID, v.State,
		)), nil
	case err != nil:
		return tool.Outcome{}, err
	}
	return tool.Succeeded(fmt.Sprintf("Stopped task %s.", args.TaskID)), nil
}

func (ts *Tasks) get(id string) (TaskView, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t, ok := ts.tasks[id]
	if !ok {
		return TaskView{}, false
	}
	return ts.viewLocked(t, nil), true
}

func (ts *Tasks) unknown(id string) error {
	ts.mu.Lock()
	known := slices.Clone(ts.order)
	ts.mu.Unlock()
	if len(known) == 0 {
		return fmt.Errorf("delegate: unknown task %q; there are no tasks", id)
	}
	return fmt.Errorf(
		"delegate: unknown task %q; known tasks: %s",
		id, strings.Join(known, ", "),
	)
}

// awaitOne returns the view of one task, after it ends or the wait is over,
// and whether the call delivers the final result the view holds (see
// readLocked). While the call waits, the end of the last turn is left to it
// (see notifyEnded); it reports that end on return, so an end that it does
// not deliver still gets its notice.
func (ts *Tasks) awaitOne(
	ctx context.Context,
	id string,
	wait time.Duration,
) (TaskView, bool, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ts.mu.Lock()
	t, ok := ts.tasks[id]
	if !ok {
		ts.mu.Unlock()
		return TaskView{}, false, ts.unknown(id)
	}
	t.waiting++
	over := wait == 0
	for {
		v, ch := ts.viewLocked(t, nil), ts.changed
		if v.Running() && !over {
			ts.mu.Unlock()
			select {
			case <-ch:
			case <-timer.C:
				over = true
			case <-ctx.Done():
				over = true
			}
			ts.mu.Lock()
			continue
		}
		c, delivered, withdraw := t.last(), false, ""
		if !v.Running() && ctx.Err() == nil {
			delivered, withdraw = ts.readLocked(ctx, c)
		}
		t.waiting--
		ts.mu.Unlock()
		if withdraw != "" && ts.cfg.Session.Withdraw(withdraw) {
			ts.mu.Lock()
			c.read = true
			ts.mu.Unlock()
			delivered = true
		}
		ts.notifyEnded(t, nil)
		return v, delivered, nil
	}
}

// readLocked decides whether a task_status call that gives the agent the
// final result of turn c, the task's last, delivers it. When the turn's
// notice has not gone out, the read takes its place: the turn is marked
// read under the lock that notifyEnded takes too, so the notice and the read
// never both carry the result. When the host holds the notice (no
// Config.Session), the read delivers, and the host finds that in the record
// with DeliveredNotices before it sends the notice. When the notice has gone
// to Config.Session, readLocked returns its ID for the caller to withdraw,
// since no turn takes a steer while a tool runs; the read delivers only if
// that works. A turn whose notice is deferred is left alone. The caller
// holds ts.mu; readLocked may release it while a notice goes out.
func (ts *Tasks) readLocked(
	ctx context.Context,
	c *childTurn,
) (delivered bool, withdraw string) {
	switch {
	case c == nil || c.deferred:
		return false, ""
	case c.read:
		return true, ""
	case !c.notified:
		c.read = true
		return true, ""
	case ts.cfg.Session == nil:
		return true, ""
	}
	// Let a notice that is going out arrive first, so it can be withdrawn.
	for !c.reported && ctx.Err() == nil {
		ch := ts.changed
		ts.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		ts.mu.Lock()
	}
	if ctx.Err() != nil || c.suppressed {
		return false, ""
	}
	return false, NoticeID(c.id)
}

// awaitAny returns every task after the running turn of a task ends, or
// after the wait is over. With no running task, or no wait, it returns at once.
func (ts *Tasks) awaitAny(ctx context.Context, wait time.Duration) []TaskView {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var running map[string]bool
	for {
		ts.mu.Lock()
		if running == nil {
			running = map[string]bool{}
			for id, t := range ts.tasks {
				if t.state() == TaskRunning {
					running[id] = true
				}
			}
		}
		ended := false
		for id := range running {
			ended = ended || ts.tasks[id].state() != TaskRunning
		}
		ch := ts.changed
		views := make([]TaskView, 0, len(ts.order))
		for _, id := range ts.order {
			views = append(views, ts.viewLocked(ts.tasks[id], nil))
		}
		ts.mu.Unlock()
		if wait == 0 || len(running) == 0 || ended {
			return views
		}
		select {
		case <-ch:
		case <-timer.C:
			return views
		case <-ctx.Done():
			return views
		}
	}
}

// describe writes a view for the model. With detail it adds the reply.
func describe(v TaskView, detail bool) string {
	var sb strings.Builder
	if detail {
		fmt.Fprintf(&sb, "Task %s (%s): %s\n", v.ID, v.Subagent, v.Description)
	} else {
		fmt.Fprintf(&sb, "%s %s %s: %s, ", v.ID, v.Subagent, v.Description, v.State)
	}
	if detail {
		fmt.Fprintf(&sb, "%s, ", v.State)
	}
	fmt.Fprintf(&sb, "%d tool calls, %s", v.ToolCalls, formatSpend(v.Spend))
	if detail {
		if v.Reply != "" {
			sb.WriteString("\n" + v.Reply)
		}
		if v.Reason != "" {
			sb.WriteString("\nReason: " + v.Reason)
		}
	}
	return sb.String()
}

// formatSpend writes the cost of a spend, such as "0.02 USD", one amount per
// currency. A spend with no priced call says so.
func formatSpend(s dashi.Spend) string {
	var parts []string
	for _, cur := range slices.Sorted(maps.Keys(s.Cost)) {
		parts = append(parts, fmt.Sprintf("%s %s", s.Cost[cur], cur))
	}
	out := strings.Join(parts, ", ")
	if out == "" {
		out = "no cost"
	}
	if s.CostUnknown > 0 && len(parts) > 0 {
		out += " (some calls are unpriced)"
	}
	if s.UsageIncomplete {
		out += "; usage incomplete"
	}
	return out
}
