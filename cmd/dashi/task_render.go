package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/dashi/workspace"
	"github.com/deepnoodle-ai/wonton/tui"
)

// Task states the CLI adds to the states of delegate.
const (
	taskApprovalPending   delegate.TaskState = "approval pending"
	taskRecordUnavailable delegate.TaskState = "record unavailable"
)

// hintBlock returns a quiet line about the CLI's own state.
func hintBlock(id, text string) Block {
	return Block{ID: id, Kind: KindHint, Text: text, Done: true}
}

// taskNoticeName is the reminder name of delegate's task-ended notice.
const taskNoticeName = "task-ended"

// taskStatusBlock returns the status line of a task, such as
// "completed · 1 model call · $0.0069 · /task t_k3f2 to inspect". The line
// of a task's first turn goes under the tool line that started the task.
func taskStatusBlock(v delegate.TaskView) Block {
	id := v.TurnID
	if id == "" {
		id = v.SessionID
	}
	b := Block{
		ID:    "task:" + id,
		Kind:  KindTaskStatus,
		State: taskLineState(v.State),
		Name:  taskStateLabel(v),
		Arg:   taskSender(v.ID, v.Subagent),
		Text:  taskFacts(v),
		Done:  !v.Running() && v.State != taskApprovalPending,
	}
	// A later turn, which a message started, stands alone and says which
	// turn it is. The view may already count a turn after the one it names.
	n := v.Turns
	if v.TurnID != "" {
		n = turnNumber(v.TurnID)
	}
	if n <= 1 && v.Parent.CallID != "" {
		b.Parent = "t:" + v.Parent.CallID
	}
	if n > 1 {
		b.Arg += fmt.Sprintf(" turn %d", n)
	}
	return b
}

// taskSummary is one line of /tasks.
func taskSummary(v delegate.TaskView) string {
	parts := []string{v.ID, orDefault(v.Subagent, "task")}
	if v.Description != "" {
		parts = append(parts, truncate(firstLine(v.Description), 48))
	}
	parts = append(parts, taskStateLabel(v))
	if v.Turns > 1 {
		parts = append(parts, fmt.Sprintf("%d turns", v.Turns))
	}
	parts = append(parts, taskCounts(v)...)
	return strings.Join(parts, " · ")
}

// taskToolArg is what a task tool line shows after "task": the subagent,
// the description, and the task ID, such as "explore · find the parser ·
// t_k3f2".
func taskToolArg(raw json.RawMessage, turnID, callID string) string {
	var args struct {
		Subagent    string `json:"subagent"`
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
	}
	_ = json.Unmarshal(raw, &args)
	var parts []string
	if args.Subagent != "" {
		parts = append(parts, args.Subagent)
	}
	if d := orDefault(args.Description, args.Prompt); d != "" {
		parts = append(parts, truncate(firstLine(d), 48))
	}
	if turnID != "" && callID != "" {
		parts = append(parts, delegate.TaskID(delegate.ChildID(turnID, callID)))
	}
	return strings.Join(parts, " · ")
}

func taskSender(id, subagent string) string {
	if subagent == "" {
		return "task " + id
	}
	return "task " + id + " (" + subagent + ")"
}

// taskLineState maps a task state to the tool line state whose color it
// shares.
func taskLineState(state delegate.TaskState) string {
	switch state {
	case delegate.TaskRunning:
		return StateRunning
	case delegate.TaskState(dashi.StopCompleted):
		return StateOK
	case delegate.TaskState(dashi.StopFailed), delegate.TaskState(dashi.StopRefused),
		delegate.TaskState(dashi.StopResultUnknown), taskRecordUnavailable:
		return StateFailed
	}
	return StateWaiting
}

func taskStateLabel(v delegate.TaskView) string {
	label := orDefault(string(v.State), "unknown")
	if v.Reason != "" && v.State != delegate.TaskRunning && v.State != delegate.TaskState(dashi.StopCompleted) {
		label += ": " + truncate(firstLine(v.Reason), 60)
	}
	return label
}

// taskCounts lists the work and cost of a task. A record that cannot be
// read has no trustworthy counts.
func taskCounts(v delegate.TaskView) []string {
	if v.State == taskRecordUnavailable {
		return nil
	}
	var parts []string
	if v.ModelCalls > 0 || !v.Running() {
		parts = append(parts, fmt.Sprintf("%d model %s", v.ModelCalls, plural(v.ModelCalls, "call")))
	}
	if v.ToolCalls > 0 {
		parts = append(parts, fmt.Sprintf("%d tool %s", v.ToolCalls, plural(v.ToolCalls, "call")))
	}
	if cost := costText(v.Spend); cost != "" {
		parts = append(parts, cost)
	}
	return parts
}

func taskFacts(v delegate.TaskView) string {
	return strings.Join(append(taskCounts(v), "/task "+v.ID+" to inspect"), " · ")
}

// deliveredItemBlocks returns a block for each message from an agent in
// msgs: a message line for an agent message, and a KindTaskResult marker
// for a task's result. These messages are reminders, so they have no user
// text.
func deliveredItemBlocks(prefix string, msgs []llm.Message) []Block {
	var out []Block
	for _, m := range msgs {
		for _, c := range m.Content {
			r := c.Reminder
			if c.Kind != llm.ContentReminder || r == nil || r.Name != taskNoticeName && r.From == "" {
				continue
			}
			id := fmt.Sprintf("%s:%d", prefix, len(out))
			if r.Name == taskMessageName {
				direction, task := MessageToParent, taskIDOf(r.From)
				if r.From == "parent" {
					direction, task = MessageToTask, ""
				}
				out = append(out, taskMessageBlock(id, direction, task, r.Content, false))
				continue
			}
			b := Block{
				ID:   id,
				Kind: KindTaskResult,
				Arg:  orDefault(r.From, "agent"),
				Text: "message delivered",
				Done: true,
			}
			if r.Name == taskNoticeName {
				b.Text = "result delivered"
				if state := noticeState(r.Content); state != "" {
					b.Name, b.State = state, taskLineState(delegate.TaskState(state))
				}
			}
			out = append(out, b)
		}
	}
	return out
}

// taskMessageName is the reminder name of delegate's agent messages.
const taskMessageName = "agent-message"

// Message line directions, kept in Block.State.
const (
	// MessageToTask is a message the parent sent a task with task_send.
	MessageToTask = "to-task"
	// MessageToParent is a message a task sent its parent with
	// message_parent.
	MessageToParent = "to-parent"
)

// taskMessageArrow is the arrow of a message line: "→" to a task, "←" to
// the parent.
func taskMessageArrow(direction string) string {
	if direction == MessageToTask {
		return "→"
	}
	return "←"
}

// taskMessageBlock returns a message line between the parent and task
// taskID, such as "→ t_k3f2  Skip vendor/". It shows the first line of
// text. Queued marks a message that waits for the recipient's turn to end.
func taskMessageBlock(id, direction, taskID, text string, queued bool) Block {
	b := Block{
		ID:    id,
		Kind:  KindTaskMessage,
		State: direction,
		Arg:   taskID,
		Text:  firstLine(text),
		Done:  true,
	}
	if queued {
		b.Name = "queued"
	}
	return b
}

// taskSendArgs are the arguments of a task_send call.
type taskSendArgs struct {
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
	Deliver string `json:"deliver"`
}

// taskSendArg is what a task_send tool line shows after its name until the
// message is sent, such as "t_k3f2 · Skip vendor/".
func taskSendArg(raw json.RawMessage) string {
	var args taskSendArgs
	_ = json.Unmarshal(raw, &args)
	return joinNonEmpty(" · ", args.TaskID, firstLine(args.Message))
}

// taskSendBlock returns the message line of a task_send call that sent its
// message. It takes the place of the call's tool line.
func taskSendBlock(callID string, raw json.RawMessage) Block {
	var args taskSendArgs
	_ = json.Unmarshal(raw, &args)
	return taskMessageBlock("t:"+callID, MessageToTask, args.TaskID, args.Message, args.Deliver == string(delegate.DeliverQueue))
}

// taskIDOf reads the task ID from a sender such as
// "task t_k3f2 (explore)". Another sender comes back whole.
func taskIDOf(from string) string {
	rest, ok := strings.CutPrefix(from, "task ")
	if !ok {
		return from
	}
	id, _, _ := strings.Cut(rest, " ")
	return id
}

// taskMessagePlainWidth cuts the text of a message line in plain output,
// which has no terminal width.
const taskMessagePlainWidth = 100

// taskMessageText is the plain text of a message line, such as
// "  ← t_k3f2  Parse is in config/load.go".
func taskMessageText(b Block) string {
	head := "  " + taskMessageArrow(b.State) + " " + b.Arg
	if b.Name != "" {
		head += " (" + b.Name + ")"
	}
	return head + "  " + truncate(b.Text, taskMessagePlainWidth)
}

// taskMessageView draws a message line. The text does not wrap: the
// terminal edge cuts it, and /tasks ID shows it whole.
func taskMessageView(b Block) tui.View {
	views := []tui.View{
		tui.Text("  %s ", taskMessageArrow(b.State)).Fg(tui.ColorCyan),
		tui.Text("%s", b.Arg),
	}
	if b.Name != "" {
		views = append(views, tui.Text(" (%s)", b.Name).Style(mutedStyle))
	}
	views = append(views, tui.Text("  "), tui.Text("%s", b.Text).SourceOffset(0).Flex(1))
	return tui.Group(views...)
}

// noticeState reads the state from a notice such as
// "[task t_k3f2 completed] explore: find the parser".
func noticeState(text string) string {
	rest, ok := strings.CutPrefix(firstLine(text), "[task ")
	if !ok {
		return ""
	}
	inside, _, ok := strings.Cut(rest, "]")
	if !ok {
		return ""
	}
	_, state, _ := strings.Cut(inside, " ")
	return state
}

// taskLineText is the plain text of a task status line or result marker.
// A status line that is not under its tool line names its task.
func taskLineText(b Block) string {
	head := joinNonEmpty(" ", b.Arg, b.Name)
	if b.Kind == KindTaskResult {
		return "↳ " + head + " · " + b.Text
	}
	return "  ↳ " + head + " · " + b.Text
}

// taskUnderMarker starts a status line under its tool line. Two spaces
// follow the marker: some terminal fonts draw ⎿ wider than its cell, and one
// space then reads as none.
const taskUnderMarker = "    ⎿  "

// taskLineUnderText is the plain text of a status line under its tool line.
func taskLineUnderText(b Block) string {
	return taskUnderMarker + joinNonEmpty(" · ", b.Name, b.Text)
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func taskLineColor(state string) tui.Color {
	switch state {
	case StateOK:
		return tui.ColorGreen
	case StateFailed:
		return tui.ColorRed
	case StateRunning, "":
		return tui.ColorCyan
	}
	return tui.ColorYellow
}

// taskLineView draws a result marker, or a status line that is not under
// its tool line. The arrow and the state take the state color.
func taskLineView(b Block) tui.View {
	color := taskLineColor(b.State)
	indent := "  ↳ "
	if b.Kind == KindTaskResult {
		indent = "↳ "
	}
	views := []tui.View{tui.Text("%s", indent).Fg(color), tui.Text("%s", b.Arg)}
	if b.Name != "" {
		views = append(views, tui.Text(" %s", b.Name).Fg(color))
	}
	// The text is the line's source for selection; the rest is a label.
	views = append(views, tui.Text(" · ").Style(mutedStyle),
		tui.Text("%s", b.Text).SourceOffset(0).Wrap().Style(mutedStyle).Flex(1))
	return tui.Group(views...)
}

// withTaskLine adds the task status line of a tool block under its view.
func withTaskLine(v tui.View, b Block) tui.View {
	if b.Task == nil || v == nil {
		return v
	}
	t := *b.Task
	// The separator is its own view: a wrapping view drops the spaces at its
	// edges, which would join the state and the separator where it wraps.
	return tui.Stack(v, tui.Group(
		tui.Text(taskUnderMarker).Style(mutedStyle),
		tui.Text("%s", t.Name).Fg(taskLineColor(t.State)),
		tui.Text(" · ").Style(mutedStyle),
		tui.Text("%s", t.Text).Wrap().Style(mutedStyle).Flex(1),
	)).Gap(0)
}
func (c *Controller) endDialog() {
	c.mu.Lock()
	c.mode = c.workerMode
	c.answerPending.Store(false)
	c.mu.Unlock()
	c.mu.Lock()
	quit, action := c.quit, c.quitAction
	idle := c.workerMode == ModeIdle
	if quit && idle {
		c.quit = false
		c.quitAction = ""
	}
	c.mu.Unlock()
	if quit && idle && (action == "" || !c.keepQueued(action)) {
		c.ui.Quit()
		return
	}
	c.scheduleTaskNotices()
}
func (c *Controller) presentTaskPrompt(p brokerPrompt) {
	c.goWork(func() {
		// Cancelled line answers must not enter the next original call's prompt.
		for {
			select {
			case <-c.answers:
			default:
				goto drained
			}
		}
	drained:
		// A prompt names its task, not the internal turn and call IDs.
		task := ""
		if p.Owner.TaskID != "" {
			task = delegate.TaskID(p.Owner.TaskID)
			if c.eng != nil {
				if host := c.eng.taskHost(); host != nil {
					for _, task := range host.list() {
						if task.SessionID == p.Owner.TaskID {
							task.State = "approval pending"
							c.emit(taskStatusBlock(task))
							break
						}
					}
				}
			}
		}
		var response brokerResponse
		var err error
		if p.Approval != nil {
			response.approval, err = c.approveUI(p.ctx, *p.Approval, task)
		} else {
			response.answer, err = c.askUI(p.ctx, *p.Question, task)
		}
		if err == nil {
			c.broker.respond(p.ID, p.Owner, response)
		} else {
			c.broker.cancelRequest(p.ID, p.Owner)
		}
	})
}
func (c *Controller) Approve(ctx context.Context, r ApprovalRequest) (Approval, error) {
	return c.broker.approve(ctx, r)
}
func (c *Controller) Ask(ctx context.Context, q Question) (workspace.Answer, error) {
	if _, err := promptOwner(ctx); err != nil {
		ctx = context.WithValue(ctx, brokerOwnerKey{}, brokerOwner{})
	}
	return c.broker.ask(ctx, q)
}
func (c *Controller) scheduleTaskNotices() {
	c.mu.Lock()
	automatic := c.autoTaskDelivery
	c.mu.Unlock()
	if !automatic || c.eng == nil {
		return
	}
	host := c.eng.taskHost()
	if host == nil {
		return
	}
	host.mu.Lock()
	ready := (len(host.notices) > 0 || host.rescan) && host.deliveryErr == nil
	host.mu.Unlock()
	if !ready {
		return
	}
	// While a turn, dialog, or command runs, say nothing: this runs again
	// when it ends, and a running turn is not stopped.
	c.mu.Lock()
	busy := c.mode != ModeIdle || c.taskPump || c.quit
	c.mu.Unlock()
	if busy {
		return
	}
	if c.eng.HasPending() {
		c.emit(hintBlock("task-delivery", "Task results will be sent after the pending prompt. Press Enter to send it."))
		return
	}
	if last := c.eng.Last(c.base); last != nil && last.Open() {
		text := "Task results will be sent after your next prompt."
		if last.CanGoOn() {
			text = "Task results will be sent when this turn goes on. Press Enter to continue it, or type a new prompt."
		}
		c.emit(hintBlock("task-delivery", text))
		return
	}
	c.mu.Lock()
	if c.mode != ModeIdle || c.taskPump || c.quit {
		c.mu.Unlock()
		return
	}
	c.taskPump = true
	c.mu.Unlock()
	// startRun retains the sole parent command owner. A pending parent prompt
	// or a stopped parent yields without continuation or an inferred permission.
	if !c.startRun(func(ctx context.Context) (Result, error) {
		defer func() { c.mu.Lock(); c.taskPump = false; c.mu.Unlock() }()
		return c.eng.DriveTaskNotices(ctx)
	}) {
		c.mu.Lock()
		c.taskPump = false
		c.mu.Unlock()
	}
}

type taskSnapshot struct {
	Task   delegate.TaskView
	Turn   dashi.Turn
	View   dashi.TurnView
	Target delegate.TaskTarget
	Bound  bool
	Prompt *dashi.CallRef
}

func (e *Engine) inspectTask(ctx context.Context, id string) (taskSnapshot, error) {
	host := e.taskHost()
	if host == nil {
		return taskSnapshot{}, fmt.Errorf("no task host is open")
	}
	host.mu.Lock()
	problem := host.problems[id]
	host.mu.Unlock()
	if problem != "" {
		return taskSnapshot{}, fmt.Errorf("recorded child unavailable: %s", problem)
	}
	for _, task := range host.tasks.List() {
		if task.ID != id {
			continue
		}
		if host.inputs.loader == nil {
			return taskSnapshot{}, fmt.Errorf("task recorder cannot load records")
		}
		turn, err := host.inputs.loader.Load(ctx, task.TurnID)
		if err != nil {
			return taskSnapshot{}, err
		}
		view, err := turn.View()
		if err != nil {
			return taskSnapshot{}, err
		}
		// Only turn 1 has the task call as its parent (see readRecorded).
		if turn.ID == session.TurnID(task.SessionID, 1) && (view.Parent == nil || *view.Parent != task.Parent) {
			return taskSnapshot{}, fmt.Errorf("task parent identity differs")
		}
		return taskSnapshot{Task: task, Turn: turn, View: view, Target: delegate.TaskTarget{ID: id, TurnID: turn.ID, LastSeq: view.LastSeq}, Bound: true}, nil
	}
	host.mu.Lock()
	saved, ok := host.recorded[id]
	host.mu.Unlock()
	if ok {
		return saved, nil
	}
	return taskSnapshot{}, delegate.ErrUnknownTask
}
func taskDetails(snapshot taskSnapshot) string {
	v := snapshot.View
	var b strings.Builder
	if !snapshot.Bound {
		b.WriteString("Recorded child; live binding is unavailable. Restore its destination/tools and reopen this session before task controls.\n")
	}
	fmt.Fprintf(&b, "Task %s · %s\nChild Turn: %s · step %d\nParent: %s · call %s\nState: %s · stop %s · ended %t\nDestination: %s / %s / %s\n%s\n", snapshot.Task.ID, snapshot.Task.Subagent, v.ID, v.LastSeq, snapshot.Task.Parent.TurnID, snapshot.Task.Parent.CallID, snapshot.Task.State, v.Stop.Kind, v.Stop.Ended, v.Agent.Provider, v.Agent.API, v.Agent.Model, snapshot.Task.Description)
	if snapshot.Prompt != nil {
		fmt.Fprintf(&b, "Live approval/answer pending · Turn %s · original call %s. Inspection does not answer it.\n", snapshot.Prompt.TurnID, snapshot.Prompt.CallID)
	}
	spend := snapshot.Task.Spend.String()
	if cost := costText(snapshot.Task.Spend); cost != "" {
		spend = cost + " (" + spend + ")"
	}
	calls, tools := snapshot.Task.ModelCalls, snapshot.Task.ToolCalls
	fmt.Fprintf(&b, "Child spend: %s\nWork: %d model %s · %d tool %s\nEstimated usage calls: %d · usage incomplete %t\nModel-call cap: %d; no child monetary cap. Parent caps exclude children.\n", spend, calls, plural(calls, "call"), tools, plural(tools, "call"), snapshot.Task.Spend.Estimated, snapshot.Task.Spend.UsageIncomplete, v.Agent.Limits.ModelCalls)
	calls, unresolved := taskDispatchCounts(snapshot.Turn)
	fmt.Fprintf(&b, "This Turn: %d model calls confirmed sent · %d started model calls with no recorded result. Started calls and estimates do not prove a request was sent.\nTokens: input %d · output %d · cache writes %d · cache reads %d; cache-write reporting unavailable %t · total mismatch %t.\n", calls, unresolved, v.Spend.Usage.InputTokens, v.Spend.Usage.OutputTokens, v.Spend.Usage.CacheCreationInputTokens, v.Spend.Usage.CacheReadInputTokens, v.Spend.Usage.CacheCreationInputTokensUnavailable, v.Spend.Usage.TotalTokenCountMismatch)
	if len(snapshot.Turn.Steps) > 0 && snapshot.Turn.Steps[0].Started != nil {
		for _, m := range snapshot.Turn.Steps[0].Started.Input {
			if m.Context != nil {
				fmt.Fprintf(&b, "Accepted resource: %s / %s (recorded snapshot; no file reads)\n", m.Context.Origin, m.Context.Name)
			}
			for _, part := range m.Content {
				if part.Block == nil {
					continue
				}
				var source *content.Source
				if part.Block.Image != nil {
					source = &part.Block.Image.Source
				}
				if part.Block.Document != nil {
					source = &part.Block.Document.Source
				}
				if source != nil && source.Ref != nil {
					fmt.Fprintf(&b, "Accepted attachment: %s · %d bytes · SHA256 %s · ref %s (unresolved)\n", source.MIMEType, source.Ref.Size, source.Ref.SHA256, source.Ref.ID)
				}
			}
		}
	}
	if v.Stop.Reason != "" {
		fmt.Fprintf(&b, "Recorded reason: %s\n", v.Stop.Reason)
	}
	for _, p := range v.Pending {
		fmt.Fprintf(&b, "Pending %s: %s · %s\nArguments: %s\n", p.Kind, p.ID, p.Name, string(p.Args))
	}
	for _, id := range slices.Sorted(maps.Keys(v.Tools)) {
		call := v.Tools[id]
		if call.Status != dashi.ToolRequested && call.Status != dashi.ToolApprovalPending {
			continue
		}
		fmt.Fprintf(&b, "Recorded requested call %s: %s · %s\nArguments: %s\n", id, call.Call.Name, call.Status, string(call.Call.Args))
	}
	if snapshot.Task.Running() {
		fmt.Fprintf(&b, "Live lower bounds. Stop: /task-stop %s\n", snapshot.Task.ID)
	} else if !v.Stop.Ended {
		fmt.Fprintf(&b, "Target controls: /task %s continue · /task %s done CALL_ID · /task %s discard\nContinue requires the recorded remedy; done requires verified completion of the exact started call.\n", snapshot.Task.ID, snapshot.Task.ID, snapshot.Task.ID)
	}
	for _, step := range snapshot.Turn.Steps {
		if step.Kind == dashi.StepToolFinished && step.Tool != nil && step.Tool.Outcome != nil {
			fmt.Fprintf(&b, "Recorded tool result: %s · call %s · %s\n%s\n", step.Tool.Call.Name, step.Tool.Call.ID, step.Tool.Outcome.State, taskDetailText(outputText(step.Tool.Outcome)))
		}
		if step.Model != nil && step.Model.Result != nil && step.Model.Purpose != dashi.ModelPurposeCompaction {
			message := step.Model.Result.Message
			if message.Role == llm.RoleAssistant && message.Text() != "" {
				fmt.Fprintf(&b, "Recorded child message:\n%s\n", taskDetailText(message.Text()))
			}
		}
	}
	return workspace.PrintableText(b.String())
}

// A model_call_started step counts a started call; only a recorded result
// counts a call as confirmed sent. Unfinished calls and calls whose result is
// unknown are counted separately.
func taskDispatchCounts(turn dashi.Turn) (int, int) {
	callStarts, finished, dispatched := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, step := range turn.Steps {
		f := step.Model
		if f == nil {
			continue
		}
		if step.Kind == dashi.StepModelCallStarted {
			callStarts[f.ModelCallID] = true
			continue
		}
		if step.Kind != dashi.StepModelCallFinished && step.Kind != dashi.StepModelCallFinishedByHost {
			continue
		}
		finished[f.ModelCallID] = !f.ResultUnknown && f.SendStatus != llm.SendStatusUnknown
		if f.NotSent || f.SendStatus == llm.SendStatusNotSent {
			continue
		}
		r := f.Result
		if f.SendStatus == llm.SendStatusProviderRejected || r != nil && (r.ID != "" || len(r.Message.Content) > 0 || r.Usage != nil || r.Stop.Kind != "") {
			dispatched[f.ModelCallID] = true
		}
	}
	unresolved := 0
	for id := range callStarts {
		if !finished[id] {
			unresolved++
		}
	}
	return len(dispatched), unresolved
}
func taskDetailText(s string) string {
	const limit = 64 << 10
	if len(s) <= limit {
		return s
	}
	n := limit
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n] + "\n[Display cut at 64 KiB; full text remains in the recorded Turn.]"
}

// taskConversation returns what /tasks ID prints: each turn of a task with
// the messages it got and sent, in order, and its reply.
func taskConversation(v delegate.TaskView, turns []dashi.Turn) string {
	var b strings.Builder
	b.WriteString(joinNonEmpty(" · ", "Task "+v.ID, v.Subagent, firstLine(v.Description)))
	b.WriteString("\n")
	for n, turn := range turns {
		view, err := turn.View()
		if err != nil {
			fmt.Fprintf(&b, "Turn %d · unreadable: %v\n", n+1, err)
			continue
		}
		state := orDefault(string(view.Stop.Kind), string(delegate.TaskRunning))
		if view.Stop.Reason != "" && view.Stop.Kind != dashi.StopCompleted {
			state += ": " + firstLine(view.Stop.Reason)
		}
		fmt.Fprintf(&b, "Turn %d · %s\n", n+1, state)
		for _, step := range turn.Steps {
			var msgs []llm.Message
			switch {
			case step.Kind == dashi.StepTurnStarted && step.Started != nil:
				msgs = step.Started.Input
			case step.Kind == dashi.StepMessagesAdded && step.Context != nil && step.Context.Steering:
				for _, item := range step.Context.Items {
					msgs = append(msgs, item.Message)
				}
			case step.Kind == dashi.StepToolFinished && step.Tool != nil &&
				step.Tool.Call.Name == delegate.MessageParentToolName &&
				step.Tool.Outcome != nil && step.Tool.Outcome.State == tool.StateSucceeded:
				var args struct {
					Message string `json:"message"`
					Deliver string `json:"deliver"`
				}
				_ = json.Unmarshal(step.Tool.Call.Args, &args)
				head := "←"
				if args.Deliver == string(delegate.DeliverQueue) {
					head += " (queued)"
				}
				writeConversationText(&b, "  "+head+" ", args.Message)
			}
			for _, m := range msgs {
				for _, c := range m.Content {
					if r := c.Reminder; c.Kind == llm.ContentReminder && r != nil && r.Name == taskMessageName && r.From == "parent" {
						writeConversationText(&b, "  → ", r.Content)
					}
				}
			}
		}
		if view.Stop.Kind == dashi.StopCompleted && view.Reply != "" {
			b.WriteString("  Reply:\n")
			writeConversationText(&b, "    ", taskDetailText(view.Reply))
		}
	}
	return workspace.PrintableText(b.String())
}

// writeConversationText writes text after head, and its later lines
// indented to line up with its first.
func writeConversationText(b *strings.Builder, head, text string) {
	indent := strings.Repeat(" ", utf8.RuneCountInString(head))
	b.WriteString(head)
	b.WriteString(strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n"+indent))
	b.WriteString("\n")
}
