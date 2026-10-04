package delegate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/money"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

// sendTurn is the parent turn that sends messages in these tests.
const sendTurn = "parent-turn-7"

// senderModel is the model of a parent turn that sends messages: its input
// holds one task_send argument object per line, and it calls task_send
// with each, as send-1, send-2, and so on, then ends.
func senderModel(_ context.Context, in llm.Input) (llm.Result, error) {
	if len(in.Messages) > 1 {
		return text("sent"), nil
	}
	var r llm.Result
	lines := strings.Split(strings.TrimSpace(in.Messages[0].Text()), "\n")
	for i, line := range lines {
		c := toolCall(fmt.Sprintf("send-%d", i+1), delegate.SendToolName, line)
		if i == 0 {
			r = c
			continue
		}
		r.Message.Content = append(r.Message.Content, c.Message.Content...)
	}
	return r, nil
}

// sendArgs returns task_send arguments for the harness's task.
func sendArgs(message string, deliver delegate.Delivery) string {
	args := map[string]string{"task_id": taskID(), "message": message}
	if deliver != "" {
		args["deliver"] = string(deliver)
	}
	b, _ := json.Marshal(args)
	return string(b)
}

// msgID is the ID of the message that call send-n of sendTurn sends.
func msgID(n int) string {
	return delegate.MessageID(sendTurn, fmt.Sprintf("send-%d", n))
}

// send runs sendTurn, whose model calls task_send once per args, one at a
// time.
func (h *harness) send(t *testing.T, args ...string) dashi.RunResult {
	t.Helper()
	out, err := h.sendCtx(t.Context(), args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (h *harness) sendCtx(ctx context.Context, args ...string) (dashi.RunResult, error) {
	return h.sendAs(ctx, sendTurn, args...)
}

func (h *harness) sendAs(ctx context.Context, turnID string, args ...string) (dashi.RunResult, error) {
	a := agent("sender")
	a.Limits.ToolConcurrency = 1
	return h.runner.Run(ctx, dashi.Turn{ID: turnID}, dashi.Start{
		CommandID: "start",
		Agent:     a,
		Input: []llm.Message{
			llm.UserMessage(llm.Text(strings.Join(args, "\n"))),
		},
	})
}

// resumeSend continues sendTurn after a crash.
func (h *harness) resumeSend(t *testing.T) dashi.RunResult {
	t.Helper()
	out, err := h.runner.Run(t.Context(), h.mustLoad(t, sendTurn), dashi.Continue{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (h *harness) mustLoad(t *testing.T, id string) dashi.Turn {
	t.Helper()
	turn, err := h.rec.Load(context.Background(), id)
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	return turn
}

// sendResult returns the text of call send-n and whether it failed.
func sendResult(t *testing.T, v dashi.TurnView, n int) (string, bool) {
	t.Helper()
	tv, ok := v.Tools[fmt.Sprintf("send-%d", n)]
	if !ok || tv.Status != dashi.ToolFinished {
		t.Fatalf("send-%d not finished: %+v", n, tv.Status)
	}
	var sb strings.Builder
	for _, b := range tv.Result().Output.Content {
		if b.Text != nil {
			sb.WriteString(b.Text.Text)
		}
	}
	return sb.String(), tv.Outcome.State == tool.StateFailed
}

// sent fails unless call send-n succeeded, and returns its text.
func sent(t *testing.T, v dashi.TurnView, n int) string {
	t.Helper()
	got, failed := sendResult(t, v, n)
	if failed {
		t.Fatalf("send-%d failed: %s", n, got)
	}
	return got
}

// refused fails unless call send-n failed with text holding want.
func refused(t *testing.T, v dashi.TurnView, n int, want string) {
	t.Helper()
	got, failed := sendResult(t, v, n)
	if !failed || !strings.Contains(got, want) {
		t.Fatalf("send-%d failed=%v text=%q, want an error with %q", n, failed, got, want)
	}
}

// childTurn is turn n of the harness's child session.
func childTurn(n int) string {
	return session.TurnID(childID(), n)
}

// childTurns counts the turns the record holds for the child session.
func (h *harness) childTurns(t *testing.T) int {
	t.Helper()
	n := 0
	for {
		turn, err := h.rec.Load(t.Context(), childTurn(n+1))
		if err != nil || len(turn.Steps) == 0 {
			return n
		}
		n++
	}
}

// delivered counts where the child's record holds message id: in context,
// as a command ID, or in a queue turn's key.
func (h *harness) delivered(t *testing.T, id string) int {
	t.Helper()
	n := 0
	for i := 1; i <= h.childTurns(t); i++ {
		turn := h.mustLoad(t, childTurn(i))
		n += countItems(turn, id)
		if key := turn.Steps[0].CommandID; key == id ||
			strings.HasPrefix(key, "queue:") && strings.Contains(key, `"`+id+`"`) {
			n++
		}
	}
	return n
}

// quiet waits until no task is pending.
func (h *harness) quiet(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		idle, err := h.tasks.Wait(ctx)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if idle {
			return
		}
	}
}

// lastMessage is the text of the last message of a request, a reminder's
// content included.
func lastMessage(in llm.Input) string {
	return inputText(llm.Input{Messages: in.Messages[len(in.Messages)-1:]})
}

// follows answers a follow-up turn of the child with the text it read.
func follows(_ context.Context, in llm.Input) (llm.Result, error) {
	return costed(text("read: "+strings.TrimSpace(lastMessage(in))), "0.25"), nil
}

// The parent redirects a running child. The child reads the message as an
// agent-message reminder from the parent before its next model call, after
// its tool result, and no model call is cut short.
func TestSendSteersIntoARunningChild(t *testing.T) {
	g := newGate()
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if len(in.Messages) == 1 {
			if err := g.wait(ctx); err != nil {
				return llm.Result{}, err
			}
			return toolCall("c1", "read", `{}`), nil
		}
		return follows(ctx, in)
	}
	h := newHarness(t, scripted(child), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	out := h.send(t, sendArgs("Skip vendor/; the parser is in config/", ""))
	want := "Sent to task " + taskID() + " (running). It reads it before its next model call."
	if got := sent(t, out.TurnView, 1); got != want {
		t.Fatalf("result=%q", got)
	}
	g.open()
	ended := h.next(t, delegate.TaskEnded)
	if ended.Task.TurnID != childTurnID() || ended.Task.Turns != 1 ||
		ended.Task.Reply != "read: Skip vendor/; the parser is in config/" {
		t.Fatalf("ended=%+v", ended.Task)
	}
	in := h.client.last("child")
	last := in.Messages[len(in.Messages)-1]
	if len(last.Content) != 1 || last.Content[0].Reminder == nil {
		t.Fatalf("last message=%+v", last)
	}
	r := last.Content[0].Reminder
	if r.Name != "agent-message" || r.From != "parent" || r.Tier != llm.ReminderContextual ||
		r.Content != "Skip vendor/; the parser is in config/" {
		t.Fatalf("reminder=%+v", r)
	}
	prev := in.Messages[len(in.Messages)-2]
	if len(prev.Content) == 0 || prev.Content[0].ToolResult == nil {
		t.Fatalf("the message before the reminder is not the tool result: %+v", prev)
	}
	if v := h.view(t, childTurnID()); v.ModelCalls != 2 {
		t.Fatalf("model calls=%d", v.ModelCalls)
	}
	if n := h.delivered(t, msgID(1)); n != 1 || h.childTurns(t) != 1 {
		t.Fatalf("delivered %d times in %d turns", n, h.childTurns(t))
	}
	for _, s := range h.mustLoad(t, childTurnID()).Steps {
		for _, item := range s.Items() {
			if item.ID == msgID(1) && item.Origin != dashi.ContextAgent {
				t.Fatalf("origin=%q", item.Origin)
			}
		}
	}
	// The record says which turn the message steered into.
	tv := out.Tools["send-1"]
	if !strings.Contains(string(tv.Outcome.Data), `"target_turn_id":"`+childTurnID()+`"`) {
		t.Fatalf("data=%s", tv.Outcome.Data)
	}
}

// holdRecorder holds the first commit that hold matches until release
// closes.
type holdRecorder struct {
	dashi.Recorder
	hold    func(dashi.CommitRequest) bool
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHoldRecorder(hold func(dashi.CommitRequest) bool) *holdRecorder {
	return &holdRecorder{
		hold:    hold,
		reached: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (h *holdRecorder) Load(ctx context.Context, id string) (dashi.Turn, error) {
	return h.Recorder.(dashi.Loader).Load(ctx, id)
}

func (h *holdRecorder) Commit(ctx context.Context, req dashi.CommitRequest) error {
	held := false
	if h.hold(req) {
		h.once.Do(func() {
			held = true
			close(h.reached)
		})
	}
	if held {
		<-h.release
	}
	return h.Recorder.Commit(ctx, req)
}

// stopsTurnOne matches the commit that stops the child's first turn.
func stopsTurnOne(r dashi.CommitRequest) bool {
	return r.Step.TurnID == childTurnID() && r.Step.Kind == dashi.StepTurnStopped
}

// A steer that misses the child's turn, because the turn was ending, runs
// as the child's next turn, a queue turn, once. Report counts that queue
// turn under the parent turn that sent the message.
func TestSendThatMissesTheTurnRunsAsTheNextTurn(t *testing.T) {
	hold := newHoldRecorder(stopsTurnOne)
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if len(in.Messages) == 1 {
			return costed(text("first"), "0.25"), nil
		}
		return follows(ctx, in)
	}
	h := newHarness(t, scripted(child), delegate.Config{},
		func(m *dashi.MemoryRecorder) dashi.Recorder {
			hold.Recorder = m
			return hold
		})
	parent, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	<-hold.reached
	out := h.send(t, sendArgs("also check env.go", ""))
	if got := sent(t, out.TurnView, 1); !strings.Contains(got, "(running)") {
		t.Fatalf("result=%q", got)
	}
	close(hold.release)
	h.next(t, delegate.TaskEnded)
	second := h.next(t, delegate.TaskEnded)
	if second.Task.TurnID != childTurn(2) || second.Task.Reply != "read: also check env.go" {
		t.Fatalf("second=%+v", second.Task)
	}
	if key := h.mustLoad(t, childTurn(2)).Steps[0].CommandID; key != `queue:["`+msgID(1)+`"]` {
		t.Fatalf("turn 2 key=%q", key)
	}
	h.quiet(t)
	if n := h.delivered(t, msgID(1)); n != 1 || h.childTurns(t) != 2 {
		t.Fatalf("delivered %d times in %d turns", n, h.childTurns(t))
	}
	usd := money.MustParseAmount
	for _, tc := range []struct {
		view dashi.TurnView
		want string
	}{{parent.TurnView, "0.25"}, {out.TurnView, "0.25"}} {
		report, err := delegate.Report(t.Context(), h.rec, tc.view)
		if err != nil {
			t.Fatal(err)
		}
		if report.Children.Cost["USD"] != usd(tc.want) {
			t.Fatalf("%s children=%v", tc.view.ID, report.Children.Cost)
		}
	}
}

// A queue message to a running child waits for its reply, then starts its
// next turn.
func TestQueueMessageRunsAfterTheReply(t *testing.T) {
	g := newGate()
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if len(in.Messages) == 1 {
			if err := g.wait(ctx); err != nil {
				return llm.Result{}, err
			}
			return text("first"), nil
		}
		return follows(ctx, in)
	}
	h := newHarness(t, scripted(child), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	out := h.send(t, sendArgs("then list the tests", delegate.DeliverQueue))
	want := "Sent to task " + taskID() + " (running). It starts a new turn " +
		"with it after its current turn ends."
	if got := sent(t, out.TurnView, 1); got != want {
		t.Fatalf("result=%q", got)
	}
	g.open()
	if first := h.next(t, delegate.TaskEnded); first.Task.Reply != "first" {
		t.Fatalf("first=%+v", first.Task)
	}
	second := h.next(t, delegate.TaskEnded)
	if second.Task.TurnID != childTurn(2) || second.Task.Reply != "read: then list the tests" {
		t.Fatalf("second=%+v", second.Task)
	}
	v := h.view(t, childTurn(2))
	if v.Origin != dashi.StartScheduled {
		t.Fatalf("turn 2 origin=%q", v.Origin)
	}
	if h.delivered(t, msgID(1)) != 1 || h.view(t, childTurnID()).ModelCalls != 1 {
		t.Fatal("turn 1 read the queue message")
	}
}

// A finished child takes a follow-up as turn 2, with turn 1 in its
// history, and its reply arrives as a second notice. Report counts turn 2
// under the parent turn that sent the message.
func TestSendGivesAFinishedChildAFollowUp(t *testing.T) {
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if len(in.Messages) == 1 {
			return costed(text("Parse is in config/load.go"), "0.25"), nil
		}
		return follows(ctx, in)
	}
	h := newHarness(t, scripted(child), delegate.Config{}, nil)
	parent, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	out := h.send(t, sendArgs("Now list the tests that cover Parse", ""))
	want := "Sent to task " + taskID() + " (completed). It starts turn 2 with " +
		"it and keeps its memory. A message that starts with \"[task " +
		taskID() + "\" arrives when it ends."
	if got := sent(t, out.TurnView, 1); got != want {
		t.Fatalf("result=%q", got)
	}
	started := h.next(t, delegate.TaskStarted)
	if started.Task.TurnID != childTurn(2) || started.Task.Turns != 2 {
		t.Fatalf("started=%+v", started.Task)
	}
	ended := h.next(t, delegate.TaskEnded)
	if ended.Task.TurnID != childTurn(2) || ended.Task.Turns != 2 ||
		ended.Task.Reply != "read: Now list the tests that cover Parse" {
		t.Fatalf("ended=%+v", ended.Task)
	}
	in := inputText(h.client.last("child"))
	if !strings.Contains(in, "find the parser") ||
		!strings.Contains(in, "Parse is in config/load.go") {
		t.Fatalf("turn 2 lacks turn 1's history: %q", in)
	}
	v := h.view(t, childTurn(2))
	sender := dashi.CallRef{TurnID: sendTurn, CallID: "send-1"}
	if v.Parent == nil || *v.Parent != sender || v.Origin != dashi.StartScheduled ||
		v.Agent.Instructions != h.view(t, childTurnID()).Agent.Instructions {
		t.Fatalf("turn 2 parent=%+v origin=%q", v.Parent, v.Origin)
	}
	if id := h.mustLoad(t, childTurn(2)).Steps[0].CommandID; id != msgID(1) {
		t.Fatalf("turn 2 command ID=%q", id)
	}
	usd := money.MustParseAmount
	for _, tc := range []struct {
		view dashi.TurnView
		want string
	}{{parent.TurnView, "0.25"}, {out.TurnView, "0.25"}} {
		report, err := delegate.Report(t.Context(), h.rec, tc.view)
		if err != nil {
			t.Fatal(err)
		}
		if report.Children.Cost["USD"] != usd(tc.want) {
			t.Fatalf("%s children=%v", tc.view.ID, report.Children.Cost)
		}
	}
	if s := h.tasks.List()[0].Spend.Cost["USD"]; s != usd("0.5") {
		t.Fatalf("task spend=%v", s)
	}
}

// A failed, limited, or stopped child takes a message as a new turn.
func TestStoppedChildrenTakeAFollowUp(t *testing.T) {
	for _, tc := range []struct {
		state string
		first modelFunc
		limit int
		stop  bool
	}{
		{state: "failed", first: func(context.Context, llm.Input) (llm.Result, error) {
			return text(""), nil
		}},
		{state: "limited", limit: 1, first: func(context.Context, llm.Input) (llm.Result, error) {
			return toolCall("c1", "read", `{}`), nil
		}},
		{state: "canceled", stop: true},
	} {
		t.Run(tc.state, func(t *testing.T) {
			g := newGate()
			child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
				if strings.Contains(inputText(in), "try again") {
					return text("done now"), nil
				}
				if tc.first == nil {
					if err := g.wait(ctx); err != nil {
						return llm.Result{}, err
					}
				}
				return tc.first(ctx, in)
			}
			sub := scout()
			sub.Agent.Limits.ModelCalls = tc.limit
			cfg := delegate.Config{Subagents: []delegate.Subagent{sub}}
			h := newHarness(t, scripted(child), cfg, nil)
			if _, err := h.start(t.Context()); err != nil {
				t.Fatal(err)
			}
			if tc.stop {
				g.waitStarted(t)
				if err := h.tasks.Stop(t.Context(), taskID()); err != nil {
					t.Fatal(err)
				}
			}
			if e := h.next(t, delegate.TaskEnded); string(e.Task.State) != tc.state {
				t.Fatalf("state=%q", e.Task.State)
			}
			out := h.send(t, sendArgs("try again", ""))
			if got := sent(t, out.TurnView, 1); !strings.Contains(got, "("+tc.state+")") {
				t.Fatalf("result=%q", got)
			}
			ended := h.next(t, delegate.TaskEnded)
			if ended.Task.TurnID != childTurn(2) || ended.Task.Reply != "done now" {
				t.Fatalf("ended=%+v", ended.Task)
			}
			h.none(t, delegate.TaskEnded)
			h.quiet(t)
			if h.childTurns(t) != 2 {
				t.Fatalf("child turns=%d", h.childTurns(t))
			}
		})
	}
}

// deferWrites makes every write call wait for an approval nobody gives.
var deferWrites = dashi.ToolHook{
	Name: "defer-writes",
	Func: func(_ context.Context, p dashi.ToolPlan) (dashi.ToolDecision, error) {
		if p.Call.Name == "write" {
			return dashi.NeedsApproval("needs a person"), nil
		}
		return dashi.ToolDecision{}, nil
	},
}.Hooks()

// lostConnection makes a model call whose result is unknown.
func lostConnection(context.Context, llm.Input) (llm.Result, error) {
	return llm.Result{Usage: &llm.Usage{InputTokens: 3}}, errors.New("connection lost")
}

// A child that waits for an approval, or whose last model call has an
// unknown result, refuses a message with a tool error, holds nothing, and
// Wait returns.
func TestSendRefusesAWaitingOrResultUnknownChild(t *testing.T) {
	for _, tc := range []struct {
		state string
		child modelFunc
		want  string
	}{
		{"waiting", childCallsProbe, "waits for an approval and cannot read messages; start a new task"},
		{"result_unknown", lostConnection, "stopped on a call whose result is unknown and cannot read messages; start a new task"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			h := newHarness(t, scripted(tc.child), delegate.Config{}, nil)
			h.runner.Hooks = deferWrites
			if _, err := h.start(t.Context()); err != nil {
				t.Fatal(err)
			}
			if e := h.next(t, delegate.TaskEnded); string(e.Task.State) != tc.state {
				t.Fatalf("state=%q", e.Task.State)
			}
			out := h.send(t, sendArgs("anything", ""))
			refused(t, out.TurnView, 1, taskID()+" "+tc.want)
			h.quiet(t)
			if h.childTurns(t) != 1 || h.delivered(t, msgID(1)) != 0 {
				t.Fatal("the message reached the child")
			}
		})
	}
}

// task_send refuses past MaxTaskTurns and past MaxMessages in one parent
// turn, and refuses bad arguments.
func TestSendLimitsAndErrors(t *testing.T) {
	t.Run("MaxTaskTurns", func(t *testing.T) {
		h := newHarness(t, script("found"), delegate.Config{MaxTaskTurns: 1}, nil)
		if _, err := h.start(t.Context()); err != nil {
			t.Fatal(err)
		}
		h.next(t, delegate.TaskEnded)
		out := h.send(t, sendArgs("more", ""))
		refused(t, out.TurnView, 1, "task "+taskID()+" has run 1 turns; start a new task")
	})
	t.Run("MaxMessages", func(t *testing.T) {
		g := newGate()
		h := newHarness(t, scripted(gated(g, "found")), delegate.Config{MaxMessages: 2}, nil)
		if _, err := h.start(t.Context()); err != nil {
			t.Fatal(err)
		}
		g.waitStarted(t)
		out := h.send(t, sendArgs("a", ""), sendArgs("b", ""), sendArgs("c", ""))
		sent(t, out.TurnView, 1)
		sent(t, out.TurnView, 2)
		refused(t, out.TurnView, 3, "message limit reached: 2 messages to task "+taskID())
		// Another parent turn has its own count.
		other, err := h.sendAs(t.Context(), "parent-turn-8", sendArgs("d", ""))
		if err != nil {
			t.Fatal(err)
		}
		sent(t, other.TurnView, 1)
		g.open()
		h.quiet(t)
	})
	t.Run("arguments", func(t *testing.T) {
		h := newHarness(t, script("found"), delegate.Config{}, nil)
		if _, err := h.start(t.Context()); err != nil {
			t.Fatal(err)
		}
		h.next(t, delegate.TaskEnded)
		out := h.send(t,
			`{"task_id":"t_nope","message":"x"}`,
			sendArgs("  ", ""),
			sendArgs(strings.Repeat("x", 16<<10+1), ""),
			sendArgs("x", "later"),
		)
		refused(t, out.TurnView, 1, "known tasks: "+taskID())
		refused(t, out.TurnView, 2, "message is required")
		refused(t, out.TurnView, 3, "over the limit of 16384")
		// The runner checks the schema's enum before the tool runs.
		if got, _ := sendResult(t, out.TurnView, 4); !strings.Contains(got, "must be one of 'steer', 'queue'") {
			t.Fatalf("send-4=%q", got)
		}
		if h.childTurns(t) != 1 {
			t.Fatal("a refused message started a turn")
		}
	})
	t.Run("outside a runner", func(t *testing.T) {
		h := newHarness(t, script("found"), delegate.Config{}, nil)
		_, err := h.exec(t.Context(), delegate.SendToolName, sendArgs("x", ""))
		if !errors.Is(err, tool.ErrFatal) {
			t.Fatalf("err=%v, want a fatal error", err)
		}
	})
}

// A steer accepted by a running child whose turn then stops waiting before
// it reads the steer is undeliverable: the notice says it was not read, and
// no later turn reads it. After a crash, Recover sends nothing and says the
// same.
func TestUnreadSteerIsReportedNotHeld(t *testing.T) {
	g := newGate()
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if err := g.wait(ctx); err != nil {
			return llm.Result{}, err
		}
		return toolCall("c1", "write", `{}`), nil
	}
	h := newHarness(t, scripted(child), delegate.Config{}, nil)
	h.runner.Hooks = deferWrites
	parent, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	out := h.send(t, sendArgs("look in config/", ""))
	g.open()
	ended := h.next(t, delegate.TaskEnded)
	if ended.Task.State != "waiting" || ended.Task.UnreadMessages != 1 ||
		!strings.Contains(ended.Task.Notice().Text(), "Your message was not read.") {
		t.Fatalf("ended=%+v", ended.Task)
	}
	if h.delivered(t, msgID(1)) != 0 {
		t.Fatal("the child read the message")
	}

	// The process dies before the parent records the notice.
	sends := h.client.sends("child")
	h.newTasks(t)
	if err := h.tasks.Recover(t.Context(), h.runner,
		[]dashi.Turn{parent.Turn, out.Turn}); err != nil {
		t.Fatal(err)
	}
	ended = h.next(t, delegate.TaskEnded)
	if ended.Task.UnreadMessages != 1 ||
		!strings.Contains(ended.Task.Notice().Text(), "Your message was not read.") {
		t.Fatalf("after Recover: %+v", ended.Task)
	}
	h.none(t, delegate.TaskEnded)
	if h.client.sends("child") != sends || h.childTurns(t) != 1 ||
		h.delivered(t, msgID(1)) != 0 {
		t.Fatal("Recover sent the message")
	}
}

// Stop drops the mail its turn did not read; the notice says so, and a
// follow-up starts a turn that holds only the new message.
func TestStopDropsUnreadMail(t *testing.T) {
	g := newGate()
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if strings.Contains(inputText(in), "try again") {
			return follows(ctx, in)
		}
		if err := g.wait(ctx); err != nil {
			return llm.Result{}, err
		}
		return text("never"), nil
	}
	h := newHarness(t, scripted(child), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	h.send(t, sendArgs("first thought", ""), sendArgs("queued thought", delegate.DeliverQueue))
	if err := h.tasks.Stop(t.Context(), taskID()); err != nil {
		t.Fatal(err)
	}
	ended := h.next(t, delegate.TaskEnded)
	if ended.Task.State != "canceled" || ended.Task.UnreadMessages != 2 ||
		!strings.Contains(ended.Task.Notice().Text(), "Your 2 messages were not read.") {
		t.Fatalf("ended=%+v", ended.Task)
	}
	out, err := h.sendAs(t.Context(), "parent-turn-8", sendArgs("try again", ""))
	if err != nil {
		t.Fatal(err)
	}
	sent(t, out.TurnView, 1)
	if e := h.next(t, delegate.TaskEnded); e.Task.Reply != "read: try again" {
		t.Fatalf("ended=%+v", e.Task)
	}
	h.quiet(t)
	if in := inputText(h.client.last("child")); strings.Contains(in, "thought") {
		t.Fatalf("the dropped mail arrived: %q", in)
	}
	if h.childTurns(t) != 2 || h.delivered(t, msgID(1)) != 0 || h.delivered(t, msgID(2)) != 0 {
		t.Fatalf("turns=%d", h.childTurns(t))
	}
	h.none(t, delegate.TaskEnded)
}

// A steer that misses a turn that failed, which the session's queue does
// not follow, starts the next turn once the child's goroutine exits. The
// message reaches the child once.
func TestSteerThatMissesAFailedTurnStartsTheNextTurn(t *testing.T) {
	hold := newHoldRecorder(stopsTurnOne)
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if len(in.Messages) == 1 {
			return text(""), nil
		}
		return follows(ctx, in)
	}
	h := newHarness(t, scripted(child), delegate.Config{},
		func(m *dashi.MemoryRecorder) dashi.Recorder {
			hold.Recorder = m
			return hold
		})
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-hold.reached
	out := h.send(t, sendArgs("use the other parser", ""))
	if got := sent(t, out.TurnView, 1); !strings.Contains(got, "(running)") {
		t.Fatalf("result=%q", got)
	}
	close(hold.release)
	if e := h.next(t, delegate.TaskEnded); e.Task.State != "failed" {
		t.Fatalf("first=%+v", e.Task)
	}
	second := h.next(t, delegate.TaskEnded)
	if second.Task.TurnID != childTurn(2) || second.Task.Reply != "read: use the other parser" {
		t.Fatalf("second=%+v", second.Task)
	}
	h.quiet(t)
	if n := h.delivered(t, msgID(1)); n != 1 || h.childTurns(t) != 2 {
		t.Fatalf("delivered %d times in %d turns", n, h.childTurns(t))
	}
}

// failRecorder refuses the commits fail matches, writing nothing.
type failRecorder struct {
	dashi.Recorder
	fail atomic.Pointer[func(dashi.CommitRequest) bool]
}

func (f *failRecorder) Load(ctx context.Context, id string) (dashi.Turn, error) {
	return f.Recorder.(dashi.Loader).Load(ctx, id)
}

func (f *failRecorder) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if fail := f.fail.Load(); fail != nil && (*fail)(req) {
		return &dashi.CommitError{NothingWritten: true, Err: errors.New("store down")}
	}
	return f.Recorder.Commit(ctx, req)
}

// A turn that a message starts and that fails before the record holds it
// is no turn: task_send fails, nothing reports it, and a later send starts
// that turn number.
func TestSendWhoseTurnFailsToStartIsNotSent(t *testing.T) {
	var store *failRecorder
	h := newHarness(t, scripted(follows), delegate.Config{},
		func(m *dashi.MemoryRecorder) dashi.Recorder {
			store = &failRecorder{Recorder: m}
			return store
		})
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	fail := func(r dashi.CommitRequest) bool {
		return r.Step.TurnID == childTurn(2)
	}
	store.fail.Store(&fail)
	out := h.send(t, sendArgs("first try", ""))
	refused(t, out.TurnView, 1, "could not start a turn")
	h.quiet(t)
	h.none(t, delegate.TaskEnded)
	if v := h.tasks.List()[0]; v.Turns != 1 || v.TurnID != childTurnID() {
		t.Fatalf("view=%+v", v)
	}
	store.fail.Store(nil)
	again, err := h.sendAs(t.Context(), "parent-turn-8", sendArgs("second try", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := sent(t, again.TurnView, 1); !strings.Contains(got, "starts turn 2") {
		t.Fatalf("result=%q", got)
	}
	if e := h.next(t, delegate.TaskEnded); e.Task.TurnID != childTurn(2) ||
		e.Task.Reply != "read: second try" {
		t.Fatalf("ended=%+v", e.Task)
	}
}

// A crash at each point of a task_send to an ended child leaves the message
// in the child's record exactly once, after Recover and the parent turn's
// resume.
func TestSendAcrossACrash(t *testing.T) {
	isSend := func(r dashi.CommitRequest, kind dashi.StepKind) bool {
		return r.Step.TurnID == sendTurn && r.Step.Kind == kind &&
			r.Step.Tool != nil && r.Step.Tool.Call.ID == "send-1"
	}
	for _, tc := range []struct {
		name    string
		trigger func(*testing.T, *harness, dashi.CommitRequest) bool
	}{
		{"before send", func(_ *testing.T, _ *harness, r dashi.CommitRequest) bool {
			return isSend(r, dashi.StepToolStarted)
		}},
		{"turn start not recorded", func(_ *testing.T, _ *harness, r dashi.CommitRequest) bool {
			return r.Step.TurnID == childTurn(2) && r.Step.Kind == dashi.StepTurnStarted
		}},
		{"delivered, call not finished, turn resumed", func(t *testing.T, h *harness, r dashi.CommitRequest) bool {
			if !isSend(r, dashi.StepToolFinished) {
				return false
			}
			h.waitStop(t, childTurn(2), dashi.StopCompleted)
			return true
		}},
		{"after delivery", func(t *testing.T, h *harness, r dashi.CommitRequest) bool {
			if r.Step.TurnID != sendTurn || r.Step.Kind != dashi.StepModelCallStarted ||
				r.Step.Seq < 3 {
				return false
			}
			h.waitStop(t, childTurn(2), dashi.StopCompleted)
			return true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var crash *crashRecorder
			var h *harness
			h = newHarness(t, scripted(follows), delegate.Config{},
				func(m *dashi.MemoryRecorder) dashi.Recorder {
					crash = &crashRecorder{Recorder: m}
					return crash
				})
			if _, err := h.start(t.Context()); err != nil {
				t.Fatal(err)
			}
			h.next(t, delegate.TaskEnded)
			crash.trigger = func(r dashi.CommitRequest) bool {
				return tc.trigger(t, h, r)
			}
			out, err := h.sendCtx(t.Context(), sendArgs("now the tests", ""))
			if err == nil && out.Stop.Kind == dashi.StopCompleted {
				t.Fatal("the crash did not stop the run")
			}
			crash.trigger = nil
			crash.dead.Store(false)

			h.newTasks(t)
			if err := h.tasks.Recover(t.Context(), h.runner,
				[]dashi.Turn{h.mustLoad(t, parentTurn), h.mustLoad(t, sendTurn)}); err != nil {
				t.Fatal(err)
			}
			resumed := h.resumeSend(t)
			if resumed.Stop.Kind != dashi.StopCompleted {
				t.Fatalf("send turn stop=%+v", resumed.Stop)
			}
			sent(t, resumed.TurnView, 1)
			h.waitStop(t, childTurn(2), dashi.StopCompleted)
			h.quiet(t)
			if n := h.delivered(t, msgID(1)); n != 1 {
				t.Fatalf("delivered %d times", n)
			}
			if n := h.childTurns(t); n != 2 {
				t.Fatalf("child turns=%d, want 2", n)
			}
			if r := h.view(t, childTurn(2)).Reply; r != "read: now the tests" {
				t.Fatalf("turn 2 reply=%q", r)
			}
		})
	}
}

// A steer that a running child accepted but had not read when the process
// stopped is sent again by Recover, into the resumed turn or as the turn
// after it, once.
func TestRecoverRedeliversASteerTheChildNeverRead(t *testing.T) {
	g := newGate()
	var resumed atomic.Bool
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if strings.Contains(inputText(in), "look in config/") {
			return costed(text("read it"), "0.25"), nil
		}
		if !resumed.Load() {
			if err := g.wait(ctx); err != nil {
				return llm.Result{}, err
			}
		}
		return costed(text("first"), "0.25"), nil
	}
	h := newHarness(t, scripted(child), delegate.Config{}, nil)
	parent, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	// The steer reaches the child's inbox, which the process loses.
	out := h.send(t, sendArgs("look in config/", ""))
	if got := sent(t, out.TurnView, 1); !strings.Contains(got, "(running)") {
		t.Fatalf("result=%q", got)
	}
	h.close(t)
	h.waitStop(t, childTurnID(), dashi.StopInterrupted)

	resumed.Store(true)
	h.newTasks(t)
	if err := h.tasks.Recover(t.Context(), h.runner,
		[]dashi.Turn{parent.Turn, out.Turn}); err != nil {
		t.Fatal(err)
	}
	h.quiet(t)
	if n := h.delivered(t, msgID(1)); n != 1 {
		t.Fatalf("delivered %d times", n)
	}
	v := h.tasks.List()[0]
	if v.Reply != "read it" {
		t.Fatalf("view=%+v", v)
	}
	// A second reopen finds the message in the record and sends nothing.
	sends := h.client.sends("child")
	h.newTasks(t)
	if err := h.tasks.Recover(t.Context(), h.runner,
		[]dashi.Turn{parent.Turn, out.Turn}); err != nil {
		t.Fatal(err)
	}
	h.quiet(t)
	if h.client.sends("child") != sends || h.delivered(t, msgID(1)) != 1 {
		t.Fatal("the second Recover sent the message again")
	}
}

// A child never gets task_send, and the tool is idempotent with a loader.
func TestSendToolIsForTheParentOnly(t *testing.T) {
	h := newHarness(t, script("done"), delegate.Config{}, nil)
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	for _, name := range toolNames(h.client.last("child")) {
		if name == delegate.SendToolName {
			t.Fatal("the child was offered task_send")
		}
	}
	list, err := h.tasks.Tools().Tools(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tl := range list {
		if tl.Definition.Name == delegate.SendToolName {
			found = true
			if a := tl.Definition.Annotations.Idempotent; a == nil || !*a {
				t.Fatal("task_send is not idempotent with a loader")
			}
		}
	}
	if !found {
		t.Fatal("no task_send tool")
	}
}

// Message.Item is a valid agent-message reminder from the sender, and its
// steer is never urgent.
func TestMessageItem(t *testing.T) {
	m := delegate.Message{
		ID:       delegate.MessageID("t1", "c1"),
		From:     "parent",
		To:       "t_k3f2",
		Text:     "skip vendor/",
		Delivery: delegate.DeliverSteer,
	}
	if m.ID != "msg:t1/c1" {
		t.Fatalf("id=%q", m.ID)
	}
	item := m.Item()
	if err := item.Validate(); err != nil {
		t.Fatal(err)
	}
	if item.ID != m.ID || item.Origin != dashi.ContextAgent {
		t.Fatalf("item=%+v", item)
	}
	r := item.Message.Content[0].Reminder
	if r == nil || r.Name != "agent-message" || r.From != "parent" || r.Content != m.Text {
		t.Fatalf("reminder=%+v", r)
	}
	if m.Steer().Urgent {
		t.Fatal("agent mail is urgent")
	}
}

// A queue message to a running child that Stop cancels before the child
// reads it is not sent after a restart: Recover does not restart the
// canceled child, and the notice says the message was not read.
func TestRecoverKeepsAStoppedChildStopped(t *testing.T) {
	g := newGate()
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if strings.Contains(inputText(in), "queued thought") {
			return follows(ctx, in)
		}
		if err := g.wait(ctx); err != nil {
			return llm.Result{}, err
		}
		return text("never"), nil
	}
	h := newHarness(t, scripted(child), delegate.Config{}, nil)
	parent, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	out := h.send(t, sendArgs("queued thought", delegate.DeliverQueue))
	if err := h.tasks.Stop(t.Context(), taskID()); err != nil {
		t.Fatal(err)
	}
	if e := h.next(t, delegate.TaskEnded); e.Task.State != "canceled" ||
		e.Task.UnreadMessages != 1 {
		t.Fatalf("ended=%+v", e.Task)
	}

	// The process dies before the parent records the notice.
	sends := h.client.sends("child")
	h.newTasks(t)
	if err := h.tasks.Recover(t.Context(), h.runner,
		[]dashi.Turn{parent.Turn, out.Turn}); err != nil {
		t.Fatal(err)
	}
	ended := h.next(t, delegate.TaskEnded)
	if ended.Task.State != "canceled" || ended.Task.UnreadMessages != 1 ||
		!strings.Contains(ended.Task.Notice().Text(), "Your message was not read.") {
		t.Fatalf("after Recover: %+v", ended.Task)
	}
	h.quiet(t)
	h.none(t, delegate.TaskEnded)
	if h.client.sends("child") != sends || h.childTurns(t) != 1 ||
		h.delivered(t, msgID(1)) != 0 {
		t.Fatal("Recover restarted the stopped child")
	}
}

// A message to a child that Stop is stopping is refused, not accepted and
// left where no turn reads it; once the child has stopped, a send starts its
// next turn.
func TestSendRefusesAStoppingChild(t *testing.T) {
	g := newGate()
	hold := newHoldRecorder(stopsTurnOne)
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if strings.Contains(inputText(in), "try again") {
			return follows(ctx, in)
		}
		if err := g.wait(ctx); err != nil {
			return llm.Result{}, err
		}
		return text("never"), nil
	}
	h := newHarness(t, scripted(child), delegate.Config{},
		func(m *dashi.MemoryRecorder) dashi.Recorder {
			hold.Recorder = m
			return hold
		})
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	stopped := make(chan error, 1)
	go func() { stopped <- h.tasks.Stop(t.Context(), taskID()) }()
	<-hold.reached
	out := h.send(t, sendArgs("late thought", delegate.DeliverQueue))
	refused(t, out.TurnView, 1, "task "+taskID()+" is stopping; try again")
	close(hold.release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if e := h.next(t, delegate.TaskEnded); e.Task.State != "canceled" ||
		e.Task.UnreadMessages != 0 {
		t.Fatalf("ended=%+v", e.Task)
	}
	again, err := h.sendAs(t.Context(), "parent-turn-8", sendArgs("try again", ""))
	if err != nil {
		t.Fatal(err)
	}
	sent(t, again.TurnView, 1)
	if e := h.next(t, delegate.TaskEnded); e.Task.Reply != "read: try again" {
		t.Fatalf("ended=%+v", e.Task)
	}
	h.quiet(t)
}

// When two task_send calls run at once and the turn that one starts fails
// before the record holds it, a call that waits on that turn fails with it:
// the other call either arrived while the turn started and fails too, or
// came later and started its own turn. Either way, a call that returned
// success reached the child once, and a call that failed never did.
func TestFailedTurnStartFailsTheSendsThatWaitOnIt(t *testing.T) {
	var store *failRecorder
	h := newHarness(t, scripted(follows), delegate.Config{},
		func(m *dashi.MemoryRecorder) dashi.Recorder {
			store = &failRecorder{Recorder: m}
			return store
		})
	if _, err := h.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.next(t, delegate.TaskEnded)
	var calls atomic.Int32
	both := make(chan struct{})
	var once sync.Once
	fail := func(r dashi.CommitRequest) bool {
		if r.Step.TurnID == sendTurn && r.Step.Kind == dashi.StepToolStarted &&
			calls.Add(1) == 2 {
			close(both)
		}
		if r.Step.TurnID != childTurn(2) || r.Step.Kind != dashi.StepTurnStarted {
			return false
		}
		failed := false
		once.Do(func() {
			// Fail the first start once both calls run, so the second call
			// most often arrives while the turn starts.
			select {
			case <-both:
				time.Sleep(20 * time.Millisecond)
			case <-time.After(5 * time.Second):
			}
			failed = true
		})
		return failed
	}
	store.fail.Store(&fail)

	a := agent("sender")
	a.Limits.ToolConcurrency = 2
	out, err := h.runner.Run(t.Context(), dashi.Turn{ID: sendTurn}, dashi.Start{
		CommandID: "start",
		Agent:     a,
		Input: []llm.Message{llm.UserMessage(llm.Text(strings.Join([]string{
			sendArgs("note one", delegate.DeliverQueue),
			sendArgs("note two", delegate.DeliverQueue),
		}, "\n")))},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.quiet(t)
	failures, turns := 0, 1
	for n := 1; n <= 2; n++ {
		got, failed := sendResult(t, out.TurnView, n)
		want := 1
		if failed {
			failures++
			want = 0
			if !strings.Contains(got, "could not start a turn") {
				t.Fatalf("send-%d: %q", n, got)
			}
		} else {
			turns++
		}
		if d := h.delivered(t, msgID(n)); d != want {
			t.Fatalf("send-%d failed=%v: delivered %d times", n, failed, d)
		}
	}
	if failures == 0 {
		t.Fatal("no send failed")
	}
	if n := h.childTurns(t); n != turns {
		t.Fatalf("child turns=%d, want %d", n, turns)
	}
	for _, v := range h.tasks.List() {
		if v.UnreadMessages != 0 {
			t.Fatalf("view=%+v", v)
		}
	}
}

// A queue message sent with success to a running child, whose next turn
// the store then refuses to start, is not lost without a word: the notice
// of the turn it waited on says it was not read, and the session's queue
// never delivers it later. After a restart, Recover sends it again; while
// the store still refuses, Recover returns an error that names it.
func TestQueuedMessageWhoseTurnFailsToStartIsReportedUnread(t *testing.T) {
	var store *failRecorder
	g := newGate()
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if strings.Contains(inputText(in), "thought") ||
			strings.Contains(inputText(in), "try again") {
			return follows(ctx, in)
		}
		if err := g.wait(ctx); err != nil {
			return llm.Result{}, err
		}
		return text("first"), nil
	}
	h := newHarness(t, scripted(child), delegate.Config{},
		func(m *dashi.MemoryRecorder) dashi.Recorder {
			store = &failRecorder{Recorder: m}
			return store
		})
	parent, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	out := h.send(t, sendArgs("queued thought", delegate.DeliverQueue))
	if got := sent(t, out.TurnView, 1); !strings.Contains(got, "after its current turn ends") {
		t.Fatalf("result=%q", got)
	}
	fail := func(r dashi.CommitRequest) bool { return r.Step.TurnID == childTurn(2) }
	store.fail.Store(&fail)
	g.open()
	ended := h.next(t, delegate.TaskEnded)
	if ended.Task.TurnID != childTurnID() || ended.Task.State != "completed" ||
		ended.Task.UnreadMessages != 1 ||
		!strings.Contains(ended.Task.Notice().Text(), "Your message was not read.") {
		t.Fatalf("ended=%+v", ended.Task)
	}
	h.quiet(t)
	h.none(t, delegate.TaskEnded)
	if h.childTurns(t) != 1 {
		t.Fatalf("child turns=%d", h.childTurns(t))
	}

	store.fail.Store(nil)
	again, err := h.sendAs(t.Context(), "parent-turn-8", sendArgs("try again", ""))
	if err != nil {
		t.Fatal(err)
	}
	sent(t, again.TurnView, 1)
	if e := h.next(t, delegate.TaskEnded); e.Task.Reply != "read: try again" {
		t.Fatalf("ended=%+v", e.Task)
	}
	h.quiet(t)
	if h.childTurns(t) != 2 || h.delivered(t, msgID(1)) != 0 {
		t.Fatalf("turns=%d: the dropped message arrived", h.childTurns(t))
	}

	// A restart: the record shows a finished send the child lacks.
	turns := []dashi.Turn{parent.Turn, out.Turn, again.Turn}
	fail = func(r dashi.CommitRequest) bool { return r.Step.TurnID == childTurn(3) }
	store.fail.Store(&fail)
	h.newTasks(t)
	err = h.tasks.Recover(t.Context(), h.runner, turns)
	if err == nil || !strings.Contains(err.Error(), msgID(1)) {
		t.Fatalf("Recover err=%v, want one that names the message", err)
	}
	store.fail.Store(nil)
	h.newTasks(t)
	if err := h.tasks.Recover(t.Context(), h.runner, turns); err != nil {
		t.Fatal(err)
	}
	h.quiet(t)
	if h.delivered(t, msgID(1)) != 1 || h.childTurns(t) != 3 {
		t.Fatalf("turns=%d delivered=%d", h.childTurns(t), h.delivered(t, msgID(1)))
	}
}

// dropSendData replaces every task_send result, so the record keeps no
// Outcome.Data for it.
var dropSendData = dashi.ResultHook{
	Name: "reword-sends",
	Func: func(_ context.Context, p dashi.ResultPlan) (dashi.ResultDecision, error) {
		if p.Call.Name != delegate.SendToolName {
			return dashi.ResultDecision{}, nil
		}
		r := p.Result
		r.Output = r.Output.MapText(func(s string) string { return "(reworded) " + s })
		return dashi.ResultDecision{Replace: &r}, nil
	},
}.Hooks()

// A queue message whose task_send result a hook replaced, sent to a turn
// that Stop canceled before the child read it, is not sent after a restart,
// even when a later follow-up turn completed: Recover finds the turn it
// waited on from the order of the parent's calls.
func TestRecoverWithoutSendDataKeepsDroppedMailDropped(t *testing.T) {
	g := newGate()
	child := func(ctx context.Context, in llm.Input) (llm.Result, error) {
		if strings.Contains(inputText(in), "try again") {
			return follows(ctx, in)
		}
		if err := g.wait(ctx); err != nil {
			return llm.Result{}, err
		}
		return text("never"), nil
	}
	h := newHarness(t, scripted(child), delegate.Config{}, nil)
	parent, err := h.start(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
	h.runner.Hooks = dropSendData
	out := h.send(t, sendArgs("queued thought", delegate.DeliverQueue))
	sent(t, out.TurnView, 1)
	if d := out.Tools["send-1"].Outcome.Data; len(d) != 0 {
		t.Fatalf("the record kept the send's data: %s", d)
	}
	if err := h.tasks.Stop(t.Context(), taskID()); err != nil {
		t.Fatal(err)
	}
	if e := h.next(t, delegate.TaskEnded); e.Task.State != "canceled" ||
		e.Task.UnreadMessages != 1 {
		t.Fatalf("ended=%+v", e.Task)
	}
	again, err := h.sendAs(t.Context(), "parent-turn-8", sendArgs("try again", ""))
	if err != nil {
		t.Fatal(err)
	}
	sent(t, again.TurnView, 1)
	if e := h.next(t, delegate.TaskEnded); e.Task.TurnID != childTurn(2) ||
		e.Task.Reply != "read: try again" {
		t.Fatalf("ended=%+v", e.Task)
	}
	h.quiet(t)

	// The process dies before the parent records either notice.
	sends := h.client.sends("child")
	h.newTasks(t)
	if err := h.tasks.Recover(t.Context(), h.runner,
		[]dashi.Turn{parent.Turn, out.Turn, again.Turn}); err != nil {
		t.Fatal(err)
	}
	first := h.next(t, delegate.TaskEnded)
	if first.Task.State != "canceled" || first.Task.UnreadMessages != 1 {
		t.Fatalf("after Recover: %+v", first.Task)
	}
	h.next(t, delegate.TaskEnded)
	h.quiet(t)
	h.none(t, delegate.TaskEnded)
	if h.client.sends("child") != sends || h.childTurns(t) != 2 ||
		h.delivered(t, msgID(1)) != 0 {
		t.Fatal("Recover sent the dropped message")
	}
}
