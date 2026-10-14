package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

// A task sends its parent a message with the message_parent tool. The tool
// is bound to the task, so the sender is the task whose turn runs the call,
// whatever the text says. The message reaches the parent as an
// "agent-message" reminder from the task, such as
// from="task t_k3f2 (explore)".
//
// With Config.Session, Tasks puts it on the parent session: a steer reaches
// the running parent turn (SteerItem) and a queue message waits for the
// turn's end (EnqueueItem). When the parent is idle, the first message of a
// child turn starts a parent queue turn; later messages of that child turn
// wait in Tasks and join the turn's notice, so one child turn wakes an idle
// parent at most once. Without Config.Session, OnTask gets a TaskMessage
// event and the host routes it. With neither, children do not get the tool,
// and a call that arrives anyway is a tool error.
//
// A message is delivered once the parent's record holds its ID (see
// DeliveredMessages). Recover reads those IDs from the parent turns and
// sends again each message of a finished message_parent call that the
// parent lacks.

// parentSteer is a steer message that a task sent its parent. Waits read
// them.
type parentSteer struct {
	msg Message
	// followed means Tasks steered it into the running turn of
	// Config.Session, so that turn's next model call holds it.
	followed bool
}

// parentResult says how a message to the parent was routed.
type parentResult struct {
	// target is the parent turn the message steered into; empty when it
	// went to the parent's queue, to OnTask, or waits for the notice.
	target string
	// later means the message waits for the child turn's notice.
	later bool
	// again means the parent's record, or this process, already held it.
	again bool
}

type parentArgs struct {
	Message string   `json:"message"`
	Deliver Delivery `json:"deliver"`
}

// messageParentTool returns the message_parent tool bound to one task.
func (ts *Tasks) messageParentTool(ctx context.Context, taskID string) tool.Tool {
	def := tool.Definition{
		Name: MessageParentToolName,
		Description: "Send a message to the agent that gave you this task. " +
			"Use it only for a finding that changes its plan, a blocker, or " +
			"a question you can work around while you wait. Everything else " +
			"goes in your final answer. If you cannot go on without an " +
			"answer, end your turn with the question instead: the reply " +
			"starts your next turn.",
		Schema: mustSchema(map[string]any{
			"message": map[string]any{
				"type":        "string",
				"description": "What your parent should know.",
			},
			"deliver": map[string]any{
				"type": "string",
				"enum": []string{string(DeliverSteer), string(DeliverQueue)},
				"description": "steer (default): before its next model call. " +
					"queue: after its current turn ends.",
			},
		}, "message"),
		Kind: tool.KindLocal,
	}
	if ts.resumable(ctx) {
		def.Annotations.Idempotent = new(true)
	}
	// The result only says where the message went.
	def.Annotations.OpenWorld = new(false)
	return tool.Tool{
		Definition: def,
		Executor: executor(func(ctx context.Context, c tool.Call) (tool.Outcome, error) {
			return ts.execMessageParent(ctx, taskID, c)
		}),
	}
}

func (ts *Tasks) execMessageParent(
	ctx context.Context,
	taskID string,
	call tool.Call,
) (tool.Outcome, error) {
	var args parentArgs
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return tool.Outcome{}, fmt.Errorf("delegate: read arguments: %w", err)
	}
	d, err := checkMessage(args.Message, args.Deliver)
	if err != nil {
		return tool.Outcome{}, err
	}
	ref, ok := dashi.CurrentCall(ctx)
	if !ok {
		return tool.Outcome{}, tool.Fatal(errors.New(
			"delegate: the message_parent tool must run under a dashi.Runner",
		))
	}
	ts.mu.Lock()
	t := ts.tasks[taskID]
	ts.mu.Unlock()
	if t == nil || !isTurnOf(ref.TurnID, t.sessionID) {
		return tool.Outcome{}, tool.Fatal(fmt.Errorf(
			"delegate: message_parent runs only in the turns of task %s", taskID,
		))
	}
	m := Message{
		ID:       MessageID(ref.TurnID, ref.CallID),
		From:     taskSender(t.id, t.sub.Name),
		To:       parentSender,
		Text:     args.Message,
		Delivery: d,
	}
	r, err := ts.sendToParent(ctx, t, ref.TurnID, m, false)
	if err != nil {
		return tool.Outcome{}, err
	}
	text := "Sent to your parent."
	if r.later {
		text = "Sent to your parent. It reads it with your final answer."
	}
	out := tool.Succeeded(text)
	var parentID string
	if s := ts.cfg.Session; s != nil {
		parentID = s.ID
	}
	out.Data, err = json.Marshal(sendData{SessionID: parentID, TargetTurnID: r.target})
	if err != nil {
		return tool.Outcome{}, err
	}
	return out, nil
}

// sendToParent sends a message from child turn turnID of t to the parent,
// unless the parent's record or this process already holds it. Past
// Config.MaxMessages for that child turn it refuses, unless redelivery is
// set. Without Config.Session and Config.OnTask nothing can deliver it, so
// it refuses. A routed message counts as a report for Wait once OnTask has
// returned for it.
func (ts *Tasks) sendToParent(
	ctx context.Context,
	t *task,
	turnID string,
	m Message,
	redelivery bool,
) (parentResult, error) {
	if !ts.reachesParent() {
		return parentResult{}, errors.New(
			"delegate: nothing can deliver a message to your parent here; " +
				"put it in your final answer",
		)
	}
	ts.mu.Lock()
	if ts.parentKnown[m.ID] {
		ts.mu.Unlock()
		return parentResult{again: true}, nil
	}
	if ts.closed {
		ts.mu.Unlock()
		return parentResult{}, errors.New("delegate: tasks are closed")
	}
	if limit := ts.maxMessages(); !redelivery && limit > 0 {
		prefix := MessageID(turnID, "")
		n := 0
		for id := range ts.parentKnown {
			if strings.HasPrefix(id, prefix) {
				n++
			}
		}
		if n >= limit {
			ts.mu.Unlock()
			return parentResult{}, fmt.Errorf(
				"delegate: message limit reached: %d messages to your parent "+
					"in this turn; put the rest in your answer", n,
			)
		}
	}
	ts.parentKnown[m.ID] = true
	c := t.find(turnID)
	ts.mu.Unlock()
	r, err := ts.routeToParent(ctx, c, m, redelivery)
	if err != nil {
		ts.mu.Lock()
		delete(ts.parentKnown, m.ID)
		ts.mu.Unlock()
		return parentResult{}, fmt.Errorf("delegate: send to your parent: %w", err)
	}
	ts.mu.Lock()
	v := ts.viewLocked(t, c)
	ts.mu.Unlock()
	if ts.cfg.OnTask != nil {
		msg := m
		ts.cfg.OnTask(TaskEvent{Kind: TaskMessage, Task: v, Message: &msg})
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if m.Delivery == DeliverSteer && !r.later {
		t.steers = append(t.steers, parentSteer{msg: m, followed: r.target != ""})
	}
	if !r.later {
		ts.reports++
	}
	ts.changedLocked()
	return r, nil
}

// routeToParent puts a message on Config.Session. While the parent runs a
// turn, a steer goes to that turn and a queue message to the queue. When it
// is idle, the first message of child turn c goes to the queue, which starts
// a parent queue turn; later ones wait in c for its notice, while c runs. A
// redelivery never waits. Without Config.Session it does nothing: OnTask
// routes the message.
func (ts *Tasks) routeToParent(
	ctx context.Context,
	c *childTurn,
	m Message,
	redelivery bool,
) (parentResult, error) {
	s := ts.cfg.Session
	if s == nil {
		return parentResult{}, nil
	}
	if !redelivery && c != nil && !parentRunning(ctx, s) {
		ts.mu.Lock()
		later := c.woke && c.state == TaskRunning
		if later {
			c.later = append(c.later, m)
		}
		c.woke = true
		ts.mu.Unlock()
		if later {
			return parentResult{later: true}, nil
		}
	}
	q, err := putOnParent(ctx, s, m.Delivery, m.Item())
	return parentResult{target: q.TurnID}, err
}

// parentRunning reports whether the parent session runs a turn. When its
// state cannot be read, it says yes, so the message goes out at once.
func parentRunning(ctx context.Context, s *session.Session) bool {
	st, err := s.Open(ctx)
	return err != nil || st.Next == dashi.NextRunning
}

// putOnParent puts an item on the parent session. A steer reaches the
// running turn, or goes to the queue when none runs; a session that refuses
// steers (while compacting, or with a host steering source) gets the item
// on its queue instead. A queue item goes to the queue.
func putOnParent(
	ctx context.Context,
	s *session.Session,
	d Delivery,
	item dashi.ContextItem,
) (session.Queued, error) {
	if d == DeliverQueue {
		return s.EnqueueItem(ctx, item)
	}
	q, err := s.SteerItem(ctx, dashi.Steer{Item: item})
	if err != nil && refusesSteers(s, err) {
		q, err = s.EnqueueItem(ctx, item)
	}
	return q, err
}

// flushLater sends the parent the messages that waited for the end of child
// turn c, just before its notice, so they reach the parent with it.
func (ts *Tasks) flushLater(c *childTurn) {
	ts.mu.Lock()
	later := c.later
	c.later = nil
	ts.mu.Unlock()
	for _, m := range later {
		ts.toParent(dashi.Steer{Item: m.Item()})
	}
}

// reachesParent reports whether a message to the parent has a path:
// Config.Session, where Tasks puts it, or Config.OnTask, where the host
// routes it. Without one, children do not get message_parent.
func (ts *Tasks) reachesParent() bool {
	return ts.cfg.Session != nil || ts.cfg.OnTask != nil
}

// maxMessages returns the bound on the messages of one sending turn to one
// recipient; 0 means none.
func (ts *Tasks) maxMessages() int {
	switch n := ts.cfg.MaxMessages; {
	case n < 0:
		return 0
	case n == 0:
		return defaultMaxMessages
	default:
		return n
	}
}

// unsentToParent is a finished message_parent call whose message the
// parent's record lacks.
type unsentToParent struct {
	t      *task
	turnID string
	msg    Message
}

// unsentToParentLocked returns the finished message_parent calls of the
// tasks' turns whose messages the parent's record lacks, in order. The
// caller holds ts.mu.
func (ts *Tasks) unsentToParentLocked(tasks []*task) []unsentToParent {
	var out []unsentToParent
	for _, t := range tasks {
		for _, c := range t.turns {
			v := c.view()
			for _, callID := range slices.Sorted(maps.Keys(v.Tools)) {
				tv := v.Tools[callID]
				if tv.Call.Name != MessageParentToolName || tv.Status != dashi.ToolFinished ||
					tv.Outcome == nil || tv.Outcome.State != tool.StateSucceeded {
					continue
				}
				var args parentArgs
				if json.Unmarshal(tv.Call.Args, &args) != nil {
					continue
				}
				d, err := checkMessage(args.Message, args.Deliver)
				id := MessageID(c.id, callID)
				if err != nil || ts.parentKnown[id] {
					continue
				}
				out = append(out, unsentToParent{t: t, turnID: c.id, msg: Message{
					ID:       id,
					From:     taskSender(t.id, t.sub.Name),
					To:       parentSender,
					Text:     args.Message,
					Delivery: d,
				}})
			}
		}
	}
	return out
}

// redeliverToParent sends again each message Recover found missing from the
// parent's record. An error is logged and not retried.
func (ts *Tasks) redeliverToParent(ctx context.Context, unsent []unsentToParent) {
	for _, u := range unsent {
		if _, err := ts.sendToParent(ctx, u.t, u.turnID, u.msg, true); err != nil {
			slog.Warn("delegate: could not send a message to the parent again after a restart",
				"task", u.t.id, "message", u.msg.ID, "error", err)
		}
	}
}

// DeliveredMessages returns the IDs of the agent messages that turns record
// as delivered: in a turn's context, as a turn's command ID, in a queue
// turn's key, or named by a task or task_status call that returned the
// text (its outcome's Data). Give it a recipient's turns. A host that routes
// messages from Config.OnTask checks it just before it sends one.
func DeliveredMessages(turns []dashi.Turn) map[string]bool {
	out := map[string]bool{}
	for _, turn := range turns {
		for _, step := range turn.Steps {
			for _, id := range stepMessageIDs(step) {
				out[id] = true
			}
			for _, id := range statusRead(step).DeliveredMessageIDs {
				out[id] = true
			}
		}
	}
	return out
}

// withParentDelivered returns the parent session's Delivered function that
// NewTasks sets: the notices and messages its record holds, so a queue turn
// carries neither twice, and the IDs that host, the session's earlier
// function, names.
func withParentDelivered(
	host func([]dashi.Turn) map[string]bool,
) func([]dashi.Turn) map[string]bool {
	return func(turns []dashi.Turn) map[string]bool {
		out := DeliveredNotices(turns)
		maps.Copy(out, DeliveredMessages(turns))
		if host != nil {
			maps.Copy(out, host(turns))
		}
		return out
	}
}

// steersSince returns the steer messages of t past the first since.
// The caller holds ts.mu.
func (t *task) steersSince(since int) []parentSteer {
	if since >= len(t.steers) {
		return nil
	}
	return slices.Clone(t.steers[since:])
}

// steerLines writes the steer messages a wait ended on, for the model. A
// message that Tasks steered into the running parent turn follows in the
// next model call, so only its sender is named; any other is written out,
// and its ID returned, so the record shows it delivered.
func steerLines(steers []parentSteer) (string, []string) {
	var lines, ids []string
	for _, s := range steers {
		if s.followed {
			lines = append(lines, fmt.Sprintf("A message from %s follows.", s.msg.From))
			continue
		}
		lines = append(lines, fmt.Sprintf("Message from %s:\n%s", s.msg.From, s.msg.Text))
		ids = append(ids, s.msg.ID)
	}
	return strings.Join(lines, "\n"), ids
}

// childTools is the toolset of a child session: the parent's tools less the
// task tools, plus message_parent bound to the task when a message can reach
// the parent (see reachesParent). It resolves the parent
// on every model call, so a dynamic toolset such as an MCP server keeps its
// live behavior inside the child; the child's recorded Agent.Tools decides
// which tools it offers.
type childTools struct {
	parent tool.Toolset
	ts     *Tasks
	taskID string
}

func (c childTools) Tools(ctx context.Context) ([]tool.Tool, error) {
	var out []tool.Tool
	if c.parent != nil {
		all, err := c.parent.Tools(ctx)
		if err != nil {
			return nil, err
		}
		for _, t := range all {
			if !isTaskTool(t.Definition.Name) {
				out = append(out, t)
			}
		}
	}
	if c.ts.reachesParent() {
		out = append(out, c.ts.messageParentTool(ctx, c.taskID))
	}
	return out, nil
}
