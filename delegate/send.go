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
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Delivery says when the recipient of an agent message reads it.
type Delivery string

const (
	// DeliverSteer delivers before the recipient's next model call.
	DeliverSteer Delivery = "steer"
	// DeliverQueue delivers when the recipient's turn ends, as its next
	// turn.
	DeliverQueue Delivery = "queue"
)

const (
	// messagePrefix starts every message ID.
	messagePrefix = "msg:"
	// messageName is the reminder name of an agent message.
	messageName = "agent-message"
	// parentSender is the From of what the parent sends.
	parentSender = "parent"
	// maxMessageText is the byte limit of one message's text.
	maxMessageText = 16 << 10
	// defaultMaxMessages bounds the messages of one sending turn to one
	// task when Config.MaxMessages is zero.
	defaultMaxMessages = 8
)

// Message is an agent message: text that a parent sends to one of its
// tasks.
type Message struct {
	// ID is MessageID of the sending call. The recipient's record holds it
	// once the message is delivered.
	ID string
	// From and To are "parent" or a task ID.
	From, To string
	Text     string
	Delivery Delivery
}

// MessageID returns the ID of the message a call sends:
// "msg:<turn>/<call>". A rerun of the call builds the same ID, so the
// recipient's record tells whether the message was delivered.
func MessageID(turnID, callID string) string {
	return messagePrefix + turnID + "/" + callID
}

// Item returns the message as a context item: a contextual "agent-message"
// reminder with the sender in From and origin dashi.ContextAgent, so it
// never carries user or operator authority. Its ID is the message ID.
func (m Message) Item() dashi.ContextItem {
	return dashi.ContextItem{
		ID:     m.ID,
		Origin: dashi.ContextAgent,
		Message: llm.ReminderMessage(llm.Reminder{
			Name:    messageName,
			Tier:    llm.ReminderContextual,
			From:    m.From,
			Content: m.Text,
		}),
	}
}

// Steer returns the message as a steer. It is never urgent: agent mail does
// not cut a model call short.
func (m Message) Steer() dashi.Steer {
	return dashi.Steer{Item: m.Item()}
}

// mail is a message Tasks accepted for a child whose record does not hold
// it yet, with the call that sent it.
type mail struct {
	msg  Message
	call dashi.CallRef
}

// sendResult says how deliver routed a message.
type sendResult struct {
	// state is the child's state when the message arrived.
	state TaskState
	// target is the running child turn the message steered into; empty
	// when it started a turn or waits for the next one.
	target string
	// after is the child's last turn when the message arrived while a
	// goroutine drove the child; empty when the message started a turn.
	after string
	// turn is the number of the turn the message started; 0 when it
	// started none.
	turn int
	// again means the child's record, or this process, already held the
	// message.
	again bool
}

// sendData is the tool.Outcome.Data of a task_send call that succeeded.
// The record keeps it for Recover and Report.
type sendData struct {
	// SessionID is the child session the message went to.
	SessionID string `json:"session_id"`
	// TargetTurnID is the running child turn the message steered into;
	// empty when it started a turn or waited for the next one.
	TargetTurnID string `json:"target_turn_id,omitempty"`
	// LastTurnID is the child's last turn when the message arrived while
	// the child ran, so the message waited on that turn; empty when the
	// message started a turn.
	LastTurnID string `json:"last_turn_id,omitempty"`
}

// deliver puts a message on a child. While a goroutine drives the child
// session, a steer goes to the running turn (SteerItem) and a queue message
// to the session's queue, which runs after the turn as a queue turn. When
// the child is idle or ended, the message starts its next turn. A child
// that waits for an approval, or holds a call whose result is unknown,
// refuses it, as does a cap unless redelivery is set. A message the child's
// record or this process already holds is sent again as nothing.
//
// A message that starts a turn returns once the child's record holds that
// turn, as the task tool does, so a crash after the call cannot lose a turn
// the model was told about. A turn that fails before its record holds it
// is no turn: deliver returns the error and the message is not sent.
func (ts *Tasks) deliver(
	ctx context.Context,
	t *task,
	m Message,
	call dashi.CallRef,
	redelivery bool,
) (sendResult, error) {
	ts.mu.Lock()
	out, c, err := ts.deliverLocked(ctx, t, m, call, redelivery)
	ts.mu.Unlock()
	if err != nil || c == nil {
		return out, err
	}
	select {
	case <-c.recorded:
	case <-ctx.Done():
		return sendResult{}, ctx.Err()
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if c.startErr != nil {
		return sendResult{}, fmt.Errorf(
			"delegate: task %s could not start a turn: %w", t.id, c.startErr,
		)
	}
	return out, nil
}

// deliverLocked routes a message. It returns the turn the message started,
// if any, for deliver to wait on. The caller holds ts.mu.
func (ts *Tasks) deliverLocked(
	ctx context.Context,
	t *task,
	m Message,
	call dashi.CallRef,
	redelivery bool,
) (sendResult, *childTurn, error) {
	if t.knows(m.ID) {
		return sendResult{state: t.state(), again: true}, nil, nil
	}
	if ts.closed {
		return sendResult{}, nil, errors.New("delegate: tasks are closed")
	}
	if t.controlling {
		return sendResult{}, nil, fmt.Errorf(
			"delegate: the host is resolving task %s; try again later", t.id,
		)
	}
	if t.stopping {
		// The canceled turn has dropped its mail, and the goroutine that is
		// exiting would start no turn for this message.
		return sendResult{}, nil, fmt.Errorf(
			"delegate: task %s is stopping; try again", t.id,
		)
	}
	if err := ts.acceptsLocked(t, call.TurnID, redelivery); err != nil {
		return sendResult{}, nil, err
	}
	out := sendResult{state: t.state()}
	var started *childTurn
	switch {
	case t.cancel != nil:
		var q session.Queued
		var err error
		if m.Delivery == DeliverQueue {
			q, err = t.session.EnqueueItem(ctx, m.Item())
		} else {
			q, err = t.session.SteerItem(ctx, m.Steer())
			if err != nil && refusesSteers(t.session, err) {
				q, err = t.session.EnqueueItem(ctx, m.Item())
			}
		}
		if err != nil {
			return sendResult{}, nil, fmt.Errorf("delegate: send to task %s: %w", t.id, err)
		}
		// A steer that finds no turn running goes to the queue. The goroutine
		// runs it after the turn, or passOnLocked does when it exits.
		out.target = q.TurnID
		out.after = t.last().id
	case out.state == TaskRunning:
		// Recover has not resumed the turn yet.
		return sendResult{}, nil, fmt.Errorf(
			"delegate: task %s cannot take messages yet; try again", t.id,
		)
	default:
		if err := ts.startTurnLocked(ctx, t, startFor(m, call)); err != nil {
			return sendResult{}, nil, err
		}
		out.turn = len(t.turns)
		started = t.last()
	}
	t.mail = append(t.mail, &mail{msg: m, call: call})
	return out, started, nil
}

// acceptsLocked refuses a message the child cannot read, or, unless
// redelivery is set, one past a cap.
func (ts *Tasks) acceptsLocked(t *task, sender string, redelivery bool) error {
	switch t.state() {
	case TaskState(dashi.StopWaiting):
		return fmt.Errorf(
			"delegate: task %s waits for an approval and cannot read "+
				"messages; start a new task", t.id,
		)
	case TaskState(dashi.StopResultUnknown):
		return fmt.Errorf(
			"delegate: task %s stopped on a call whose result is "+
				"unknown and cannot read messages; start a new task", t.id,
		)
	}
	if redelivery {
		return nil
	}
	// Mail to a running turn may start one more turn after it, so the
	// bound applies whatever the state.
	if err := ts.checkTurnsLocked(t); err != nil {
		return err
	}
	limit := ts.cfg.MaxMessages
	if limit == 0 {
		limit = defaultMaxMessages
	}
	if n := t.sentBy(sender); limit > 0 && n >= limit {
		return fmt.Errorf(
			"delegate: message limit reached: %d messages to task %s in "+
				"this turn; wait for its reply", n, t.id,
		)
	}
	return nil
}

// startFor returns the Start of the child turn a message starts. Its command
// ID is the message ID, so the child's record holds the delivery, and its
// Parent is the sending call, so Report counts its spend there.
func startFor(m Message, call dashi.CallRef) dashi.Start {
	return dashi.Start{
		CommandID: m.ID,
		Origin:    dashi.StartScheduled,
		Parent:    &call,
		Input:     []llm.Message{m.Item().Message},
	}
}

// heldLocked records the message IDs a child step holds, so they are no
// longer pending. The caller holds ts.mu.
func (t *task) heldLocked(step dashi.Step) {
	ids := stepMessageIDs(step)
	if len(ids) == 0 {
		return
	}
	for _, id := range ids {
		t.held[id] = true
	}
	kept := t.mail[:0]
	for _, m := range t.mail {
		if !t.held[m.msg.ID] {
			kept = append(kept, m)
		}
	}
	clear(t.mail[len(kept):])
	t.mail = kept
}

// knows reports whether the child's record holds the message, Tasks
// accepted it in this process, or Tasks dropped it unread. The caller holds
// ts.mu.
func (t *task) knows(id string) bool {
	if t.held[id] {
		return true
	}
	for _, m := range t.mail {
		if m.msg.ID == id {
			return true
		}
	}
	t.droppedMu.Lock()
	defer t.droppedMu.Unlock()
	return t.dropped[id]
}

// sentBy counts the messages to the child that turn sender sent. The caller
// holds ts.mu.
func (t *task) sentBy(sender string) int {
	prefix := MessageID(sender, "")
	n := 0
	count := func(id string) {
		if strings.HasPrefix(id, prefix) {
			n++
		}
	}
	for id := range t.held {
		count(id)
	}
	for _, m := range t.mail {
		count(m.msg.ID)
	}
	t.droppedMu.Lock()
	defer t.droppedMu.Unlock()
	for id := range t.dropped {
		count(id)
	}
	return n
}

// deliveredMail is the child session's Delivered function: the message IDs
// its turns hold, and those Tasks dropped unread. Just before a queue turn,
// the session drops these from its queue, so a message never reaches the
// child twice, nor after Tasks told the parent it was not read. It runs
// under the session's lock, so it takes only droppedMu.
func (t *task) deliveredMail(turns []dashi.Turn) map[string]bool {
	out := map[string]bool{}
	for _, turn := range turns {
		for _, step := range turn.Steps {
			for _, id := range stepMessageIDs(step) {
				out[id] = true
			}
		}
	}
	t.droppedMu.Lock()
	defer t.droppedMu.Unlock()
	for id := range t.dropped {
		out[id] = true
	}
	return out
}

// dropMailLocked ends the pending mail of a child whose turn stopped where
// it cannot read it: waiting, result_unknown, or canceled by Stop. The steers
// leave the inbox, the session's queue will drop the rest, and turn c counts
// them, so its notice says they were not read. The caller holds ts.mu.
func (t *task) dropMailLocked(c *childTurn, turn dashi.Turn) {
	if len(t.mail) == 0 {
		return
	}
	cleared := false
	if len(turn.Steps) > 0 {
		_, err := t.inbox.Leftovers(turn)
		cleared = err == nil
	}
	if !cleared {
		for _, m := range t.mail {
			t.inbox.Cancel(m.msg.ID)
		}
	}
	t.droppedMu.Lock()
	for _, m := range t.mail {
		t.dropped[m.msg.ID] = true
	}
	t.droppedMu.Unlock()
	c.unread += len(t.mail)
	clear(t.mail)
	t.mail = nil
}

// dropOnStop reports whether a child turn that stopped in state leaves its
// pending mail unread.
func dropOnStop(state TaskState) bool {
	switch state {
	case TaskState(dashi.StopWaiting), TaskState(dashi.StopResultUnknown),
		TaskState(dashi.StopCanceled):
		return true
	}
	return false
}

// passOnLocked starts the next turn with mail that no turn read once the
// goroutine that drove the child has exited: steers that missed the last
// turn and queue messages behind it, when that turn did not end with
// dashi.NextNone (failed or limited), or when the mail arrived after the
// session last looked at its queue. The first message starts the turn; the
// rest wait in the session's queue and run after it. The caller holds ts.mu.
func (ts *Tasks) passOnLocked(ctx context.Context, t *task) {
	if ts.closed || t.cancel != nil || len(t.mail) == 0 {
		return
	}
	state := t.state()
	if state == TaskRunning || state == TaskState(dashi.StopInterrupted) ||
		dropOnStop(state) {
		return
	}
	first := t.mail[0]
	if err := ts.startTurnLocked(ctx, t, startFor(first.msg, first.call)); err != nil {
		slog.Warn("delegate: could not deliver messages to a task",
			"task", t.id, "count", len(t.mail), "error", err)
		t.dropMailLocked(t.last(), dashi.Turn{})
	}
}

// stepMessageIDs returns the message IDs a child step holds: a turn's
// command ID, the IDs in a queue turn's key, and its context items' IDs.
func stepMessageIDs(step dashi.Step) []string {
	var ids []string
	add := func(id string) {
		if strings.HasPrefix(id, messagePrefix) {
			ids = append(ids, id)
		}
	}
	if step.Kind == dashi.StepTurnStarted {
		add(step.CommandID)
		for _, id := range queueKeyIDs(step.CommandID) {
			add(id)
		}
	}
	for _, item := range step.Items() {
		add(item.ID)
	}
	return ids
}

// queueKeyIDs returns the item IDs in a queue turn's command ID, or nil for
// another command ID.
func queueKeyIDs(commandID string) []string {
	list, ok := strings.CutPrefix(commandID, queuePrefix)
	if !ok {
		return nil
	}
	var ids []string
	if json.Unmarshal([]byte(list), &ids) != nil {
		return nil
	}
	return ids
}

type sendArgs struct {
	TaskID  string   `json:"task_id"`
	Message string   `json:"message"`
	Deliver Delivery `json:"deliver"`
}

// checkSendArgs checks task_send arguments and fills the default delivery.
func checkSendArgs(raw []byte) (sendArgs, error) {
	var args sendArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return args, fmt.Errorf("delegate: read arguments: %w", err)
	}
	switch {
	case strings.TrimSpace(args.Message) == "":
		return args, errors.New("delegate: message is required")
	case len(args.Message) > maxMessageText:
		return args, fmt.Errorf(
			"delegate: message is %d bytes, over the limit of %d; shorten it",
			len(args.Message), maxMessageText,
		)
	case args.Deliver == "":
		args.Deliver = DeliverSteer
	case args.Deliver != DeliverSteer && args.Deliver != DeliverQueue:
		return args, fmt.Errorf(
			"delegate: deliver must be %q or %q", DeliverSteer, DeliverQueue,
		)
	}
	return args, nil
}

func (ts *Tasks) execSend(
	ctx context.Context,
	call tool.Call,
) (tool.Outcome, error) {
	args, err := checkSendArgs(call.Args)
	if err != nil {
		return tool.Outcome{}, err
	}
	ref, ok := dashi.CurrentCall(ctx)
	if !ok {
		return tool.Outcome{}, tool.Fatal(errors.New(
			"delegate: the task_send tool must run under a dashi.Runner",
		))
	}
	ts.mu.Lock()
	t, found := ts.tasks[args.TaskID]
	fromChild := false
	for _, other := range ts.tasks {
		fromChild = fromChild || strings.HasPrefix(ref.TurnID, other.sessionID+"-")
	}
	ts.mu.Unlock()
	if fromChild {
		return tool.Outcome{}, tool.Fatal(errors.New(
			"delegate: task_send runs only in the parent's turns",
		))
	}
	if !found {
		return tool.Outcome{}, ts.unknown(args.TaskID)
	}
	m := Message{
		ID:       MessageID(ref.TurnID, ref.CallID),
		From:     parentSender,
		To:       t.id,
		Text:     args.Message,
		Delivery: args.Deliver,
	}
	r, err := ts.deliver(ctx, t, m, ref, false)
	if err != nil {
		return tool.Outcome{}, err
	}
	out := tool.Succeeded(sendText(t.id, m.Delivery, r))
	out.Data, err = json.Marshal(sendData{
		SessionID:    t.sessionID,
		TargetTurnID: r.target,
		LastTurnID:   r.after,
	})
	if err != nil {
		return tool.Outcome{}, err
	}
	return out, nil
}

// sendText is the task_send result for the model.
func sendText(id string, d Delivery, r sendResult) string {
	switch {
	case r.again:
		return fmt.Sprintf("Sent to task %s.", id)
	case r.turn > 0:
		return fmt.Sprintf("Sent to task %s (%s). It starts turn %d with it "+
			"and keeps its memory. A message that starts with \"[task %s\" "+
			"arrives when it ends.", id, r.state, r.turn, id)
	case d == DeliverQueue:
		return fmt.Sprintf("Sent to task %s (%s). It starts a new turn "+
			"with it after its current turn ends.", id, r.state)
	case r.target != "":
		return fmt.Sprintf("Sent to task %s (%s). It reads it before its "+
			"next model call.", id, r.state)
	default:
		// The steer found the turn ending; it runs as the next turn.
		return fmt.Sprintf("Sent to task %s (%s). It starts a new turn "+
			"with it after its current turn ends.", id, r.state)
	}
}

// sentMessage is a task_send call that a parent turn finished with success.
type sentMessage struct {
	msg  Message
	call dashi.CallRef
	data sendData
}

// sentMessages returns the task_send calls a parent turn finished with
// success. Data is empty when a hook or the result limit changed the result.
func sentMessages(turnID string, view dashi.TurnView) []sentMessage {
	var out []sentMessage
	for _, callID := range slices.Sorted(maps.Keys(view.Tools)) {
		tv := view.Tools[callID]
		if tv.Call.Name != SendToolName || tv.Status != dashi.ToolFinished ||
			tv.Outcome == nil || tv.Outcome.State != tool.StateSucceeded {
			continue
		}
		args, err := checkSendArgs(tv.Call.Args)
		if err != nil {
			continue
		}
		var data sendData
		if len(tv.Outcome.Data) > 0 {
			_ = json.Unmarshal(tv.Outcome.Data, &data)
		}
		out = append(out, sentMessage{
			msg: Message{
				ID:       MessageID(turnID, callID),
				From:     parentSender,
				To:       args.TaskID,
				Text:     args.Message,
				Delivery: args.Deliver,
			},
			call: dashi.CallRef{TurnID: turnID, CallID: callID},
			data: data,
		})
	}
	return out
}

// sortSentLocked sorts the finished sends of the parent turns that the
// children's records lack. A message is undeliverable when its target turn,
// or a turn from the one it waited on, stopped where it could not read it
// (waiting, result_unknown, or canceled), or when the child now waits or
// holds a call whose result is unknown: it counts on that turn, so the
// notice says it was not read, and is never sent. So a message never starts
// a turn of a child that Stop canceled before it read it. The rest are
// returned, to deliver again once the children resume. The caller holds
// ts.mu.
func (ts *Tasks) sortSentLocked(sends []sentMessage) []sentMessage {
	var again []sentMessage
	for _, s := range sends {
		t := ts.tasks[s.msg.To]
		if t == nil || s.data.SessionID != "" && t.sessionID != s.data.SessionID ||
			t.knows(s.msg.ID) {
			continue
		}
		var unread *childTurn
		if c := t.find(s.data.TargetTurnID); c != nil && dropOnStop(c.state) {
			unread = c
		}
		if unread == nil && s.data.LastTurnID != "" {
			unread = t.droppedFrom(s.data.LastTurnID)
		}
		last := t.last()
		switch {
		case unread != nil:
		case last.state == TaskState(dashi.StopWaiting),
			last.state == TaskState(dashi.StopResultUnknown):
			unread = last
		case last.state == TaskState(dashi.StopCanceled) && s.data.SessionID == "":
			// Without the call's data, the message may have waited on the
			// canceled turn; it must not restart the child.
			unread = last
		}
		if unread == nil {
			again = append(again, s)
			continue
		}
		unread.unread++
		t.droppedMu.Lock()
		t.dropped[s.msg.ID] = true
		t.droppedMu.Unlock()
	}
	return again
}

// droppedFrom returns the first turn, from turn id on, that stopped where
// it drops its mail, or nil. When no turn has that ID, it checks only the
// last turn. The caller holds ts.mu.
func (t *task) droppedFrom(id string) *childTurn {
	i := slices.IndexFunc(t.turns, func(c *childTurn) bool { return c.id == id })
	if i < 0 {
		i = len(t.turns) - 1
	}
	for _, c := range t.turns[i:] {
		if dropOnStop(c.state) {
			return c
		}
	}
	return nil
}

// redeliver sends again each message Recover found missing, through
// deliver, past the caps the first send passed. A refusal is logged and not
// retried.
func (ts *Tasks) redeliver(ctx context.Context, sends []sentMessage) {
	for _, s := range sends {
		ts.mu.Lock()
		t := ts.tasks[s.msg.To]
		ts.mu.Unlock()
		if _, err := ts.deliver(ctx, t, s.msg, s.call, true); err != nil {
			slog.Warn("delegate: could not deliver a message again after a restart",
				"task", t.id, "message", s.msg.ID, "error", err)
		}
	}
}
