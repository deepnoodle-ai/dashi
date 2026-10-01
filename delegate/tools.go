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
	claim := readClaimFrom(ctx)
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
		c, r := t.last(), readNone
		if !v.Running() && ctx.Err() == nil {
			r = ts.readLocked(ctx, c, claim)
		}
		t.waiting--
		if r == readClaimed || r == readWithdraw {
			ts.wg.Add(1)
		}
		ts.mu.Unlock()
		switch r {
		case readClaimed:
			go ts.confirmRead(t, c, *claim, false)
		case readWithdraw:
			if ts.cfg.Session.Withdraw(NoticeID(c.id)) {
				ts.mu.Lock()
				c.read, c.readCall = true, claim.call
				ts.mu.Unlock()
				go ts.confirmRead(t, c, *claim, true)
			} else {
				r = readNone
				ts.wg.Done()
			}
		}
		ts.notifyEnded(t, nil)
		return v, r != readNone, nil
	}
}

// maxReadConfirm is how long confirmRead waits for the parent's record to
// settle a task_status call. A runner saves a call's result as soon as it
// returns, so only a failed save waits this long.
const maxReadConfirm = 30 * time.Second

// readResult is what a task_status read of a turn's final result does.
type readResult int

const (
	// readNone delivers nothing: the notice carries the result.
	readNone readResult = iota
	// readClaimed takes the place of a notice that has not gone out.
	readClaimed
	// readHeld delivers a result whose notice the host holds; the host
	// finds the read in the record with DeliveredNotices.
	readHeld
	// readWithdraw delivers if the caller can withdraw the notice that
	// went to Config.Session.
	readWithdraw
)

// readLocked decides whether a task_status call that gives the agent the
// final result of turn c, the task's last, delivers it. When the turn's
// notice has not gone out, the read claims its place: the turn is marked
// read under the lock that notifyEnded takes too, so the notice and the read
// never both carry the result. When the host holds the notice (no
// Config.Session), the read delivers, and the host checks the record before
// it sends the notice. When the notice has gone to Config.Session, the
// caller withdraws it, since no turn takes a steer while a tool runs. A
// claim or a withdrawal needs a claim, so that confirmRead can tell whether
// the parent recorded the read. A turn whose notice is deferred, or that
// another call claimed, is left alone; the same call run again after its
// turn was continued delivers again. The caller holds ts.mu; readLocked may
// release it while a notice goes out.
func (ts *Tasks) readLocked(
	ctx context.Context,
	c *childTurn,
	claim *readClaim,
) readResult {
	switch {
	case c == nil || c.deferred || ts.closed:
		return readNone
	case c.read:
		if claim != nil && claim.call == c.readCall {
			return readHeld
		}
		return readNone
	case !c.notified:
		if claim == nil {
			return readNone
		}
		c.read, c.readCall = true, claim.call
		return readClaimed
	case ts.cfg.Session == nil:
		return readHeld
	case claim == nil:
		return readNone
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
		return readNone
	}
	return readWithdraw
}

// readClaim names the task_status call that claimed a result, and the
// parent recorder that will hold it.
type readClaim struct {
	call   dashi.CallRef
	loader dashi.Loader
}

// readClaimFrom returns the claim of the task_status call running under
// ctx. Without a runner whose recorder can load turns, a read cannot be
// confirmed, so it returns nil and the read claims nothing.
func readClaimFrom(ctx context.Context) *readClaim {
	call, ok := dashi.CurrentCall(ctx)
	if !ok {
		return nil
	}
	parent, ok := dashi.CurrentRunner(ctx)
	if !ok {
		return nil
	}
	loader, ok := parent.Recorder.(dashi.Loader)
	if !ok {
		return nil
	}
	return &readClaim{call: call, loader: loader}
}

// confirmRead waits until the parent's record settles the task_status call
// that claimed turn c: the call finished, or its turn stopped without it.
// A recorded call that names c delivered the result: a claim made before
// the notice went out now reports the turn with AlreadyDelivered. Anything
// else, such as a turn canceled before the result was saved, gives the
// result back to the notice, so it reaches the agent in this session. So
// does a record that does not settle within maxReadConfirm, as when the
// save failed: a repeated result beats a lost one. Close ends the wait, and
// then the record decides on the next Recover. The caller has added to
// ts.wg.
func (ts *Tasks) confirmRead(t *task, c *childTurn, claim readClaim, notified bool) {
	defer ts.wg.Done()
	delivered, ok := ts.awaitRecord(claim, c.id)
	if !ok {
		return
	}
	ts.mu.Lock()
	if delivered {
		if notified {
			ts.mu.Unlock()
			return
		}
		c.notified = true
		v := ts.viewLocked(t, c)
		ts.mu.Unlock()
		if ts.cfg.OnTask != nil {
			ts.cfg.OnTask(TaskEvent{Kind: TaskEnded, Task: v, AlreadyDelivered: true})
		}
		ts.mu.Lock()
		ts.reportedLocked(c, true)
		ts.mu.Unlock()
		return
	}
	c.read = false
	v := ts.viewLocked(t, c)
	ts.mu.Unlock()
	if notified {
		// The notice went out once and was withdrawn: send it again.
		ts.toParent(v.Notice().Steer())
		return
	}
	ts.notifyEnded(t, nil)
}

// awaitRecord polls the parent turn of a claim until it settles, and
// reports whether the recorded call delivered child turn id. ok is false
// when Close ends the wait first.
func (ts *Tasks) awaitRecord(claim readClaim, id string) (delivered, ok bool) {
	pause := time.Millisecond
	deadline := time.After(maxReadConfirm)
	for {
		delivered, settled := readSettled(claim, id)
		if settled {
			return delivered, true
		}
		select {
		case <-ts.quit:
			return false, false
		case <-deadline:
			return false, true
		case <-time.After(pause):
		}
		pause = min(2*pause, 100*time.Millisecond)
	}
}

// readSettled loads the parent turn of a claim and reports whether the
// record settles the call: it finished, delivering id or not, or the turn
// recorded a stop after the call started and before it finished.
func readSettled(claim readClaim, id string) (delivered, settled bool) {
	turn, err := claim.loader.Load(context.Background(), claim.call.TurnID)
	if err != nil {
		return false, false
	}
	started := false
	for _, step := range turn.Steps {
		switch {
		case step.Tool != nil && step.Tool.Call.ID == claim.call.CallID &&
			step.Kind == dashi.StepToolStarted:
			started = true
		case step.Tool != nil && step.Tool.Call.ID == claim.call.CallID &&
			step.Kind == dashi.StepToolFinished:
			return statusDelivery(step) == id, true
		case started && step.Kind == dashi.StepTurnStopped:
			return false, true
		}
	}
	return false, false
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
