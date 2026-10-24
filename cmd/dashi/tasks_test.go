package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/session"
)

// parentTurnCount counts the recorded turns of the open session.
func parentTurnCount(t *testing.T, e *Engine) int {
	t.Helper()
	n := 0
	for _, err := range e.handle().Turns(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	return n
}

// The user's /tasks and /task do not count as delivery: the parent still
// gets the result after the user looked at it.
func TestUserInspectionDoesNotDeliverTheResult(t *testing.T) {
	// Hold the child until the parent's turn ends. A child that ends during
	// that turn steers its notice into it, which is a delivery.
	hold := make(chan struct{})
	client := &taskFixtureClient{childHold: hold}
	ui := newRecorderUI()
	a, _ := taskTestApp(t, client, ui)
	result, err := a.eng.Prompt(t.Context(), "Delegate this job")
	if err != nil {
		t.Fatal(err)
	}
	close(hold)
	task := waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopCompleted) })
	if _, err := a.eng.taskHost().tasks.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.ctrl.Input("/tasks")
	a.ctrl.Wait()
	a.ctrl.Input("/task " + task.ID)
	a.ctrl.Wait()
	result, err = a.eng.QuietTasks(t.Context(), result)
	if err != nil || result.Outcome.Reply != "Task result received." {
		t.Fatalf("reply=%q err=%v: inspection kept the result from the parent", result.Outcome.Reply, err)
	}
}

// When the parent reads a task's final result with task_status, the result
// is delivered: no extra turn runs, now or after a reopen. Either order of
// the child's end and the read holds. As with any delegate.Config.Session,
// the result's notice may also reach the running turn, so the model may
// see it a second time in that turn.
func TestTaskStatusReadDeliversTheResultWithoutAnotherTurn(t *testing.T) {
	for _, order := range []string{"read waits for the end", "child ends before the read"} {
		t.Run(order, func(t *testing.T) {
			hold := make(chan struct{})
			client := &taskFixtureClient{childHold: hold, statusRead: true}
			ui := newRecorderUI()
			a, cfg := taskTestApp(t, client, ui)
			client.beforeStatus = func() {
				if order == "read waits for the end" {
					go func() { time.Sleep(50 * time.Millisecond); close(hold) }()
					return
				}
				close(hold)
				waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopCompleted) })
				// The host has the notice in hand before the read.
				if _, err := a.eng.taskHost().tasks.Wait(t.Context()); err != nil {
					t.Error(err)
				}
			}
			result, err := a.eng.Prompt(t.Context(), "Delegate and read the result")
			if err != nil || result.Outcome.Reply != "Parent read the result." {
				t.Fatalf("reply=%q err=%v", result.Outcome.Reply, err)
			}
			if _, err = a.eng.QuietTasks(t.Context(), result); err != nil {
				t.Fatal(err)
			}
			parent, child := client.counts()
			if parent != 3 || child != 1 || client.parentNotices > 1 {
				t.Fatalf("calls=%d/%d notices=%d: the read result was delivered again", parent, child, client.parentNotices)
			}
			if n := parentTurnCount(t, a.eng); n != 1 {
				t.Fatalf("parent turns=%d, want 1", n)
			}
			// A reopened session does not deliver it either.
			sessionID := a.eng.Session().ID
			if err := a.eng.closeTasks(); err != nil {
				t.Fatal(err)
			}
			a.eng.Close()
			b, err := buildApp(t.Context(), cfg, ui)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = b.openSession(cfg.Store, cfg.Dir, Options{Resume: true, ResumeID: sessionID}); err != nil {
				t.Fatal(err)
			}
			b.ctrl.mu.Lock()
			b.ctrl.autoTaskDelivery = false
			b.ctrl.mu.Unlock()
			t.Cleanup(func() { b.ctrl.CancelWork(); b.eng.closeTasks(); b.ctrl.Wait(); b.eng.Close() })
			if err = b.eng.recoverTasks(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err = b.eng.QuietTasks(t.Context(), Result{}); err != nil {
				t.Fatal(err)
			}
			if again, _ := client.counts(); again != parent || parentTurnCount(t, b.eng) != 1 {
				t.Fatalf("after reopen parent calls=%d turns=%d", again, parentTurnCount(t, b.eng))
			}
		})
	}
}

// messageLines returns the plain message lines among blocks.
func messageLines(blocks []Block) []string {
	var out []string
	for _, b := range blocks {
		if b.Kind == KindTaskMessage {
			out = append(out, FormatBlock(b))
		}
	}
	return out
}

// A task's message_parent message reaches the parent agent once, and the
// parent's task_send message starts the task's next turn. Both show as
// message lines, live and replayed, and /tasks ID shows each turn's reply
// with the messages between them.
func TestTaskMessagesReachTheParentAndShowLiveAndReplayed(t *testing.T) {
	const up, down = "Parse is in config/load.go; env overrides too?", "Yes, check env overrides too"
	client := &taskFixtureClient{messageParent: up, messageDeliver: "queue", sendQueue: down}
	ui := newRecorderUI()
	a, _ := taskTestApp(t, client, ui)
	result, err := a.eng.Prompt(t.Context(), "Delegate this job")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.eng.QuietTasks(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	task := waitTask(t, a, func(v delegate.TaskView) bool { return v.Turns == 2 && !v.Running() })

	// The parent's record holds the task's message.
	var turns []dashi.Turn
	for turn, err := range a.eng.handle().Turns(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		turns = append(turns, turn)
	}
	if got := delegate.DeliveredMessages(turns); len(got) != 1 {
		t.Fatalf("parent holds messages %v, want the task's one", got)
	}

	want := []string{
		"  → " + task.ID + " (queued)  " + down,
		"  ← " + task.ID + "  " + up,
	}
	ui.mu.Lock()
	live := messageLines(ui.tr.Blocks)
	var turnTwo bool
	for _, b := range ui.log {
		turnTwo = turnTwo || b.Kind == KindTaskStatus && strings.HasPrefix(FormatBlock(b), "  ↳ task "+task.ID+" (general) turn 2 ")
	}
	ui.mu.Unlock()
	replay := messageLines(a.eng.Scrollback(t.Context()))
	if !slices.Equal(live, want) || !slices.Equal(replay, want) {
		t.Fatalf("live=%q\nreplay=%q\nwant=%q", live, replay, want)
	}
	if !turnTwo {
		t.Fatal("no status line for turn 2")
	}

	got, err := a.eng.taskConversation(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantText := "Task " + task.ID + " · general · Check the exact effect\n" +
		"Turn 1 · completed\n" +
		"  ← (queued) " + up + "\n" +
		"  Reply:\n" +
		"    Child result, recorded once.\n" +
		"Turn 2 · completed\n" +
		"  → " + down + "\n" +
		"  Reply:\n" +
		"    Child result, recorded once.\n"
	if got != wantText {
		t.Fatalf("conversation=\n%s\nwant\n%s", got, wantText)
	}
	a.ctrl.Input("/tasks " + task.ID)
	a.ctrl.Wait()
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if last := ui.log[len(ui.log)-1]; last.Kind != KindNotice || last.Text != wantText {
		t.Fatalf("/tasks ID printed %+v", last)
	}
}

// messageDeliveries counts the places where the parent's record gives the
// model a task's message: a context item, a wait's result that returned its
// text, or a queue turn that carried it.
func messageDeliveries(t *testing.T, e *Engine) map[string]int {
	t.Helper()
	out := map[string]int{}
	for turn, err := range e.handle().Turns(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]int{}
		for _, step := range turn.Steps {
			for _, item := range step.Items() {
				if strings.HasPrefix(item.ID, "msg:") {
					seen[item.ID]++
				}
			}
			if step.Kind != dashi.StepToolFinished || step.Tool == nil || step.Tool.Outcome == nil {
				continue
			}
			var data struct {
				IDs []string `json:"delivered_message_ids"`
			}
			if json.Unmarshal(step.Tool.Outcome.Data, &data) == nil {
				for _, id := range data.IDs {
					seen[id]++
				}
			}
		}
		// A queue turn records its items in its command ID.
		for id := range delegate.DeliveredMessages([]dashi.Turn{turn}) {
			seen[id] = max(seen[id], 1)
		}
		for id, n := range seen {
			out[id] += n
		}
	}
	return out
}

// waitUntil polls ready until it holds, or fails the test.
func waitUntil(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A steer message from a task reaches the running parent turn before its
// next model call, not in a later queue turn.
func TestTaskSteerMessageReachesTheRunningParentTurn(t *testing.T) {
	childHold, parentHold := make(chan struct{}), make(chan struct{})
	client := &taskFixtureClient{messageParent: "Blocker: config/load.go is generated", childHold: childHold, parentHold: parentHold}
	a, _ := taskTestApp(t, client, newRecorderUI())
	type prompted struct {
		result Result
		err    error
	}
	done := make(chan prompted, 1)
	go func() {
		result, err := a.eng.Prompt(t.Context(), "Delegate this job")
		done <- prompted{result, err}
	}()
	// The parent's second model call is in flight when the task sends.
	waitUntil(t, "the parent's second model call", func() bool { parent, _ := client.counts(); return parent == 2 })
	close(childHold)
	waitTask(t, a, func(v delegate.TaskView) bool { return v.Turns == 1 && !v.Running() })
	close(parentHold)
	p := <-done
	if p.err != nil {
		t.Fatal(p.err)
	}
	client.muTask.Lock()
	calls := slices.Clone(client.parentMessageCalls)
	client.muTask.Unlock()
	if len(calls) == 0 || calls[0] != 2 {
		t.Fatalf("parent calls with the message %v, want first call 2, the next one in turn 1", calls)
	}
	if n := parentTurnCount(t, a.eng); n != 1 {
		t.Fatalf("parent turns = %d before the notice, want 1", n)
	}
	if _, err := a.eng.QuietTasks(t.Context(), p.result); err != nil {
		t.Fatal(err)
	}
	got := messageDeliveries(t, a.eng)
	if len(got) != 1 {
		t.Fatalf("deliveries %v, want one message", got)
	}
	for id, n := range got {
		if n != 1 {
			t.Fatalf("message %s delivered %d times, want once", id, n)
		}
	}
}

// statusResults returns the text the model saw of each task_status call in
// the parent's first turn, and the message IDs its record names.
func statusResults(t *testing.T, e *Engine) (string, []string) {
	t.Helper()
	var text strings.Builder
	var ids []string
	for turn, err := range e.handle().Turns(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		view, err := turn.View()
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range view.Tools {
			if call.Call.Name != "task_status" || call.Outcome == nil {
				continue
			}
			for _, b := range call.Result().Output.Content {
				if b.Text != nil {
					text.WriteString(b.Text.Text)
				}
			}
			var data struct {
				IDs []string `json:"delivered_message_ids"`
			}
			_ = json.Unmarshal(call.Outcome.Data, &data)
			ids = append(ids, data.IDs...)
		}
		break
	}
	return text.String(), ids
}

// A task_status wait that ends on a steer message names only its sender,
// and the next model call gets the message, once.
func TestTaskSteerMessageReadByTaskStatusIsDeliveredOnce(t *testing.T) {
	const text = "Blocker: config/load.go is generated"
	childHold := make(chan struct{})
	client := &taskFixtureClient{messageParent: text, childHold: childHold, statusRead: true}
	client.beforeStatus = func() {
		// Let the task send once the wait has begun.
		time.AfterFunc(200*time.Millisecond, func() { close(childHold) })
	}
	a, _ := taskTestApp(t, client, newRecorderUI())
	result, err := a.eng.Prompt(t.Context(), "Delegate this job")
	if err != nil {
		t.Fatal(err)
	}
	task := waitTask(t, a, func(v delegate.TaskView) bool { return v.Turns == 1 && !v.Running() })
	id := delegate.MessageID(session.TurnID(task.SessionID, 1), "message-call")
	saw, ids := statusResults(t, a.eng)
	if strings.Contains(saw, text) || !strings.Contains(saw, "A message from task "+task.ID+" (general) follows.") || len(ids) != 0 {
		t.Fatalf("wait result %q with IDs %v, want the sender only", saw, ids)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if _, err := a.eng.QuietTasks(ctx, result); err != nil {
		t.Fatal(err)
	}
	if got := messageDeliveries(t, a.eng)[id]; got != 1 {
		t.Fatalf("message %s delivered %d times, want once", id, got)
	}
}

// A task's result that reaches an idle parent behind a /queue prompt does
// not run that prompt early: the task wake runs nothing, and the user's
// next prompt takes both.
func TestTaskWakeLeavesQueuedUserPromptForTheNextPrompt(t *testing.T) {
	hold := make(chan struct{})
	client := &taskFixtureClient{childHold: hold}
	a, _ := taskTestApp(t, client, newRecorderUI())
	if _, err := a.eng.Prompt(t.Context(), "Delegate this job"); err != nil {
		t.Fatal(err)
	}
	if err := a.eng.Queue("Also check the tests"); err != nil {
		t.Fatal(err)
	}
	close(hold)
	waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopCompleted) })
	if _, err := a.eng.taskHost().tasks.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st, err := a.eng.handle().Open(t.Context()); err != nil || st.Queued != 2 || st.QueuedUser != 1 {
		t.Fatalf("state=%+v err=%v, want the user prompt and the task's notice queued", st, err)
	}
	if n := a.eng.queuedTaskItems(); n != 0 {
		t.Fatalf("queued task items = %d, want 0 behind a user prompt", n)
	}
	if _, err := a.eng.DriveTaskNotices(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.eng.QuietTasks(t.Context(), Result{}); err != nil {
		t.Fatal(err)
	}
	if parent, _ := client.counts(); parent != 2 || parentTurnCount(t, a.eng) != 1 {
		t.Fatalf("parent calls=%d turns=%d: the task wake ran the queued prompt", parent, parentTurnCount(t, a.eng))
	}
	if _, err := a.eng.Prompt(t.Context(), "Next prompt"); err != nil {
		t.Fatal(err)
	}
	client.muTask.Lock()
	inputs, notices := slices.Clone(client.parentInputs), client.parentNotices
	client.muTask.Unlock()
	last := inputs[len(inputs)-1]
	if notices != 1 || !strings.Contains(last, "Also check the tests") || !strings.Contains(last, "Next prompt") {
		t.Fatalf("notices=%d last input %q: want the notice, the queued prompt, and the next prompt", notices, last)
	}
	if st, err := a.eng.handle().Open(t.Context()); err != nil || st.Queued != 0 {
		t.Fatalf("state=%+v err=%v, want an empty queue", st, err)
	}
}

// A turn that a finished task starts streams like the user's own turns: it
// writes --turn-events lines and returns its result.
func TestDriveTaskNoticesStreamsTheQueueTurn(t *testing.T) {
	hold := make(chan struct{})
	client := &taskFixtureClient{childHold: hold}
	cfg := testConfig(t)
	cfg.Client = client
	path := filepath.Join(t.TempDir(), "turn-events.jsonl")
	log, err := openTurnEvents(path, cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	cfg.TurnEvents = log
	a, err := buildApp(t.Context(), cfg, newRecorderUI())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	a.ctrl.mu.Lock()
	a.ctrl.autoTaskDelivery = false
	a.ctrl.mu.Unlock()
	t.Cleanup(func() { a.ctrl.CancelWork(); a.eng.closeTasks(); a.ctrl.Wait(); a.eng.Close() })
	if _, err := a.eng.Prompt(t.Context(), "Delegate this job"); err != nil {
		t.Fatal(err)
	}
	close(hold)
	waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopCompleted) })
	if _, err := a.eng.taskHost().tasks.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	result, err := a.eng.DriveTaskNotices(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	id := a.eng.Session().ID
	if result.Outcome.Turn.ID != id+"-2" || result.Outcome.Reply != "Task result received." {
		t.Fatalf("result turn=%s reply=%q", result.Outcome.Turn.ID, result.Outcome.Reply)
	}
	if err := log.idle(nil); err != nil {
		t.Fatal(err)
	}
	got := readTurnEvents(t, path)
	if n := len(got); n < 2 || names(got[n-2:]) != "UserPromptSubmit Stop" ||
		got[n-2]["prompt_id"] != id+"-2" || got[n-1]["last_assistant_message"] != "Task result received." {
		t.Fatalf("records: %v", got)
	}
}
