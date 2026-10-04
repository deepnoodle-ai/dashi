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

// Tools returns the toolset that holds task, task_status, task_send, and
// task_stop. Join it with the tools the agent already has. Children draw
// from the tools of the runner that runs the call, filtered by each
// Subagent; they never receive these tools, so a child cannot start a child.
// Each child gets message_parent instead, bound to its task, when
// Config.Session or Config.OnTask can deliver its messages.
//
// The task and task_send tools are marked idempotent when the recorder can
// load turns. After a crash, the runner runs the call again: the same child
// continues from its record, and a message the child's record holds is not
// sent twice. Without such a recorder, a resumed parent reports the call as
// result_unknown and sends nothing twice.
func (ts *Tasks) Tools() tool.Toolset {
	return taskTools{ts}
}

type taskTools struct{ ts *Tasks }

// resumable reports whether a retry can find the child: the override
// recorder, else the recorder of the runner that asks for tools. Outside a
// runner it assumes the host will set one that loads turns.
func (t taskTools) resumable(ctx context.Context) bool {
	return t.ts.resumable(ctx)
}

func (ts *Tasks) resumable(ctx context.Context) bool {
	if r := ts.cfg.Runner.Recorder; r != nil {
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
			"background and this call returns at once with a task ID, or " +
			"after up to wait_seconds. Keep working, or end your turn to " +
			"wait: a message arrives when the task ends. The subagent " +
			"starts with no memory of this conversation and returns one " +
			"answer; task_send gives it a correction or a follow-up. " +
			"Subagents:" + list.String(),
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
			"wait_seconds": map[string]any{
				"type":    "integer",
				"minimum": 0,
				"maximum": maxWaitSeconds,
				"description": "How long to wait for the task's answer or a " +
					"message from it before this call returns. Default 0.",
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
			"holds the call until the task ends or sends you a message " +
			"(without task_id, any task), or the time is up. Do not poll: " +
			"a message arrives when a task ends.",
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
	send := tool.Definition{
		Name: SendToolName,
		Description: "Send a message to a task you started. A running task " +
			"reads it before its next model call (deliver \"steer\", the " +
			"default), or starts a new turn with it after its current turn " +
			"(\"queue\"). A task that ended starts a new turn with it and " +
			"keeps its memory, so use this for a follow-up instead of a new " +
			"task. Its reply arrives as a message that starts with \"[task\". " +
			"Each message may start a paid turn: send only what changes its work.",
		Schema: mustSchema(map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"description": "The task ID that the task tool returned.",
			},
			"message": map[string]any{
				"type":        "string",
				"description": "What the task should know or do next.",
			},
			"deliver": map[string]any{
				"type": "string",
				"enum": []string{string(DeliverSteer), string(DeliverQueue)},
				"description": "steer (default): before its next model call. " +
					"queue: as a new turn after its current turn ends.",
			},
		}, "task_id", "message"),
		Kind: tool.KindLocal,
	}
	if t.resumable(ctx) {
		send.Annotations.Idempotent = new(true)
	}
	return []tool.Tool{
		{Definition: start, Executor: executor(t.ts.execStart)},
		{Definition: status, Executor: executor(t.ts.execStatus)},
		{Definition: send, Executor: executor(t.ts.execSend)},
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
	WaitSeconds int    `json:"wait_seconds"`
}

// checkWait checks wait_seconds and returns the wait.
func checkWait(seconds int) (time.Duration, error) {
	if seconds < 0 || seconds > maxWaitSeconds {
		return 0, fmt.Errorf(
			"delegate: wait_seconds must be 0 to %d", maxWaitSeconds,
		)
	}
	return time.Duration(seconds) * time.Second, nil
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
	wait, err := checkWait(args.WaitSeconds)
	if err != nil {
		return tool.Outcome{}, err
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
	started := fmt.Sprintf(
		"Started task %s (%s): %s. It runs in the background. A message "+
			"that starts with \"[task %s\" arrives when it ends. Keep "+
			"working. Do not poll.",
		v.ID, v.Subagent, v.Description, v.ID,
	)
	if wait == 0 {
		return tool.Succeeded(started), nil
	}
	// The wait counts every message of the task, so one sent before the
	// wait began still ends it.
	v, final, steers, err := ts.awaitOne(ctx, v.ID, wait, 0)
	if err != nil {
		return tool.Outcome{}, err
	}
	var d statusData
	text := started
	if !v.Running() {
		text = describe(v, true)
		if final {
			d.DeliveredTurnID = v.TurnID
		}
	}
	if len(steers) > 0 {
		var lines string
		lines, d.DeliveredMessageIDs = steerLines(steers)
		text += "\n" + lines
	}
	return withStatusData(tool.Succeeded(text), d)
}

// withStatusData records what a result delivered in its outcome's Data,
// when it delivered anything.
func withStatusData(out tool.Outcome, d statusData) (tool.Outcome, error) {
	if d.DeliveredTurnID == "" && len(d.DeliveredMessageIDs) == 0 {
		return out, nil
	}
	var err error
	out.Data, err = json.Marshal(d)
	if err != nil {
		return tool.Outcome{}, err
	}
	return out, nil
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
	wait, err := checkWait(args.WaitSeconds)
	if err != nil {
		return tool.Outcome{}, err
	}
	var d statusData
	var text string
	var steers []parentSteer
	if args.TaskID != "" {
		var v TaskView
		var final bool
		v, final, steers, err = ts.awaitOne(ctx, args.TaskID, wait, -1)
		if err != nil {
			return tool.Outcome{}, err
		}
		text = describe(v, true)
		if final {
			// The record names the child turn whose result the model saw,
			// so a host can skip its notice (see DeliveredNotices).
			d.DeliveredTurnID = v.TurnID
		}
	} else {
		var views []TaskView
		views, steers = ts.awaitAny(ctx, wait)
		if len(views) == 0 {
			return tool.Succeeded("No tasks."), nil
		}
		lines := make([]string, len(views))
		for i, v := range views {
			lines[i] = describe(v, false)
		}
		text = strings.Join(lines, "\n")
	}
	if len(steers) > 0 {
		var lines string
		lines, d.DeliveredMessageIDs = steerLines(steers)
		text += "\n" + lines
	}
	return withStatusData(tool.Succeeded(text), d)
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

// awaitOne returns the view of one task, after its turn ends, it sends a
// steer message to the parent, or the wait is over. It also reports
// whether the view holds the final result that the turn's notice carries
// (see finalLocked), and returns the steer messages past the first since;
// a negative since counts from now.
func (ts *Tasks) awaitOne(
	ctx context.Context,
	id string,
	wait time.Duration,
	since int,
) (TaskView, bool, []parentSteer, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		ts.mu.Lock()
		t, ok := ts.tasks[id]
		if !ok {
			ts.mu.Unlock()
			return TaskView{}, false, nil, ts.unknown(id)
		}
		if since < 0 {
			since = len(t.steers)
		}
		v, ch := ts.viewLocked(t, nil), ts.changed
		final := finalLocked(t)
		steers := t.steersSince(since)
		ts.mu.Unlock()
		if !v.Running() || wait == 0 || len(steers) > 0 {
			return v, final, steers, nil
		}
		select {
		case <-ch:
		case <-timer.C:
			return v, final, nil, nil
		case <-ctx.Done():
			return v, final, nil, nil
		}
	}
}

// finalLocked reports whether the last turn of t ended with the result its
// notice carries. A deferred notice waits for the host, and a turn that
// Close interrupted resumes later, so neither is final yet. The caller holds
// ts.mu.
func finalLocked(t *task) bool {
	c := t.last()
	return c != nil && c.state != TaskRunning && !c.deferred && !c.suppressed
}

// awaitAny returns every task after the running turn of a task ends or a
// task sends a steer message to the parent, or after the wait is over, with
// the steer messages sent while it waited. With no running task, or no
// wait, it returns at once.
func (ts *Tasks) awaitAny(ctx context.Context, wait time.Duration) ([]TaskView, []parentSteer) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var running map[string]bool
	var since map[string]int
	for {
		ts.mu.Lock()
		if running == nil {
			running, since = map[string]bool{}, map[string]int{}
			for id, t := range ts.tasks {
				since[id] = len(t.steers)
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
		var steers []parentSteer
		for _, id := range ts.order {
			t := ts.tasks[id]
			views = append(views, ts.viewLocked(t, nil))
			if n, ok := since[id]; ok {
				steers = append(steers, t.steersSince(n)...)
			}
		}
		ts.mu.Unlock()
		if wait == 0 || len(running) == 0 || ended || len(steers) > 0 {
			return views, steers
		}
		select {
		case <-ch:
		case <-timer.C:
			return views, nil
		case <-ctx.Done():
			return views, nil
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
