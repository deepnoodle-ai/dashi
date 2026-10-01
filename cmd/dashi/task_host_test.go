package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi/filerecorder"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

// Requests are selected by recorded child instructions, not goroutine order.
// Thus concurrent parent/child dispatches exercise the actual host bindings.
type taskFixtureClient struct {
	fakeClient
	muTask                  sync.Mutex
	parentCalls, childCalls int
	childHold, parentHold   <-chan struct{}
	childStarted            chan struct{}
	startOnce               sync.Once
	effect                  bool
	secondTask              bool
	childAttempts           map[string]int
	loopUntil               int
}

func (c *taskFixtureClient) Send(ctx context.Context, call llm.Call) (res llm.Result, err error) {
	defer func() {
		if res.Requested.Provider == "" {
			res.Requested = llm.ModelRef{Provider: "test", Model: "fixture"}
		}
	}()
	in := call.Request.Snapshot().Input
	child := strings.Contains(in.System, "You are a subagent.")
	c.muTask.Lock()
	n := c.parentCalls
	if child {
		n = c.childCalls
		c.childCalls++
		if c.secondTask {
			if c.childAttempts == nil {
				c.childAttempts = map[string]int{}
			}
			key := "first"
			for _, m := range in.Messages {
				if strings.Contains(m.Text(), "second task") {
					key = "second"
				}
			}
			n = c.childAttempts[key]
			c.childAttempts[key]++
		}
	} else {
		c.parentCalls++
	}
	c.muTask.Unlock()
	if child {
		c.startOnce.Do(func() {
			if c.childStarted != nil {
				close(c.childStarted)
			}
		})
		if c.childHold != nil {
			select {
			case <-c.childHold:
			case <-ctx.Done():
				return llm.Result{}, ctx.Err()
			}
		}
		if n < c.loopUntil {
			return callTool("read-loop-"+string(rune('A'+n)), "list_directory", `{}`)(ctx, call)
		}
		if c.effect && n == 0 {
			return callTool("effect-call", "external_effect", `{"target":"once"}`)(ctx, call)
		}
		return say("Child result, recorded once.")(ctx, call)
	}
	if n == 0 {
		res, err := callTool("task-call", "task", `{"subagent":"general","description":"Check the exact effect","prompt":"Inspect the focused task"}`)(ctx, call)
		if c.secondTask {
			second := tool.Call{ID: "second-task-call", Name: "task", Args: []byte(`{"subagent":"general","description":"Keep the other work","prompt":"Inspect second task"}`)}
			res.Message.Content = append(res.Message.Content, llm.Content{Kind: llm.ContentToolCall, ToolCall: &second})
		}
		return res, err
	}
	if n == 1 {
		if c.parentHold != nil {
			select {
			case <-c.parentHold:
			case <-ctx.Done():
				return llm.Result{}, ctx.Err()
			}
		}
		return say("Parent remains available.")(ctx, call)
	}
	return say("Task result received.")(ctx, call)
}
func (c *taskFixtureClient) counts() (int, int) {
	c.muTask.Lock()
	defer c.muTask.Unlock()
	return c.parentCalls, c.childCalls
}

func taskTestApp(t *testing.T, client *taskFixtureClient, ui UI) (*app, appConfig) {
	t.Helper()
	cfg := testConfig(t)
	cfg.Client = client
	a, err := buildApp(t.Context(), cfg, ui)
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
	return a, cfg
}
func waitTask(t *testing.T, a *app, ready func(delegate.TaskView) bool) delegate.TaskView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, v := range a.eng.taskHost().tasks.List() {
			if ready(v) {
				return v
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("task state not reached: %+v\n", a.eng.taskHost().tasks.List())
	return delegate.TaskView{}
}
func installTaskEffect(t *testing.T, runs *atomic.Int32, wait bool) {
	t.Helper()
	old := adapters
	adapters = append(append([]func(Env) (wiring, error){}, adapters...), func(Env) (wiring, error) {
		entry, err := tool.Func("external_effect", "An effect requiring approval", func(context.Context, tool.Call, struct {
			Target string `json:"target"`
		}) (tool.Outcome, error) {
			runs.Add(1)
			if wait {
				return tool.Waiting("Verify external completion"), nil
			}
			return tool.Succeeded("effect recorded"), nil
		})
		return wiring{Tools: tool.List{entry}}, err
	})
	t.Cleanup(func() { adapters = old })
}
func TestTaskHostQuietDeliversOneRecordedNoticeAndKeepsChildTextSeparate(t *testing.T) {
	client := &taskFixtureClient{}
	ui := newRecorderUI()
	a, _ := taskTestApp(t, client, ui)
	result, err := a.eng.Prompt(t.Context(), "Delegate this job")
	if err != nil {
		t.Fatal(err)
	}
	result, err = a.eng.QuietTasks(t.Context(), result)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Reply != "Task result received." {
		t.Fatalf("quiet answer=%q", result.Outcome.Reply)
	}
	task := waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopCompleted) })
	count := 0
	for turn, err := range a.eng.handle().Turns(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range turn.Steps {
			if step.Kind == dashi.StepTurnStarted && strings.Contains(step.CommandID, delegate.NoticeID(task.TurnID)) {
				count++
			}
		}
	}
	if count != 1 {
		t.Fatalf("recorded notices=%d", count)
	}
	beforeParent, beforeChild := client.counts()
	// A callback duplicate and a fresh projection cannot deliver again.
	item := task.Notice().Item()
	host := a.eng.taskHost()
	host.mu.Lock()
	host.notices[item.ID] = item
	host.mu.Unlock()
	if _, err = a.eng.DriveTaskNotices(t.Context()); err != nil {
		t.Fatal(err)
	}
	parent, child := client.counts()
	if parent != beforeParent || child != beforeChild {
		t.Fatalf("duplicate dispatches parent=%d child=%d", parent, child)
	}
	for _, block := range ui.blocks() {
		if block.Kind == KindAssistant && strings.Contains(block.Text, "Child result") {
			t.Fatal("child answer entered parent assistant transcript")
		}
	}
}
func TestTaskDoneReopenContinuesWithoutRepeatingEffect(t *testing.T) {
	var runs atomic.Int32
	installTaskEffect(t, &runs, true)
	client := &taskFixtureClient{effect: true}
	var confirmations []Question
	ui := &questionFixtureUI{recorderUI: newRecorderUI(), ask: func(_ context.Context, q Question) (questionAnswer, error) {
		confirmations = append(confirmations, q)
		return questionAnswer{choices: []int{1}}, nil
	}}
	a, cfg := taskTestApp(t, client, ui)
	result, err := a.eng.Prompt(t.Context(), "Delegate this effect")
	if err != nil {
		t.Fatal(err)
	}
	task := waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopWaiting) })
	if _, err = a.eng.QuietTasks(t.Context(), result); err == nil {
		t.Fatal("waiting child reported successful quiet exit")
	}
	originalID := task.ID
	sessionID := a.eng.Session().ID
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
	t.Cleanup(b.eng.Close)
	if err = b.eng.recoverTasks(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := b.eng.inspectTask(t.Context(), originalID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.View.Next() == dashi.NextNone || runs.Load() != 1 {
		t.Fatalf("waiting state=%+v effects=%d", snapshot.View.Stop, runs.Load())
	}
	if err = b.ctrl.taskAction(t.Context(), originalID, "done", "wrong-call"); err == nil {
		t.Fatal("wrong call accepted")
	}
	if err = b.ctrl.taskAction(t.Context(), originalID, "continue", ""); err == nil {
		t.Fatal("uncertain effect retried")
	}
	if err = b.ctrl.taskAction(t.Context(), originalID, "done", "effect-call"); err != nil {
		t.Fatal(err)
	}
	if len(confirmations) != 1 || !strings.Contains(confirmations[0].Prompt, "call effect-call") || !strings.Contains(confirmations[0].Prompt, "Arguments:") {
		t.Fatalf("original confirmation=%+v", confirmations)
	}
	if err = b.ctrl.taskAction(t.Context(), originalID, "done", "effect-call"); err == nil {
		t.Fatal("already settled effect accepted")
	}
	if err = b.ctrl.taskAction(t.Context(), originalID, "continue", ""); err != nil {
		t.Fatal(err)
	}
	_, err = b.eng.QuietTasks(t.Context(), Result{})
	if err != nil {
		t.Fatal(err)
	}
	if runs.Load() != 1 {
		t.Fatalf("effects repeated=%d", runs.Load())
	}
	parent, child := client.counts()
	if parent != 3 || child != 2 {
		t.Fatalf("calls=%d/%d", parent, child)
	}
	snapshot, err = b.eng.inspectTask(t.Context(), originalID)
	if err != nil || snapshot.View.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("final=%+v %v", snapshot.View.Stop, err)
	}
	if err = b.ctrl.taskAction(t.Context(), originalID, "discard", ""); err == nil {
		t.Fatal("completed task canceled")
	}
	// Session inspection remains own-session. Task-inclusive reports add each
	// child once and do not overwrite that established scope.
	own, err := cfg.Store.Inspect(t.Context(), cfg.Dir, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if own.Accounting.Spend.Usage.InputTokens != 250 || own.Accounting.ModelCalls != 3 {
		t.Fatalf("own-session changed: %+v", own.Accounting)
	}
	var parentSpend, childSpend dashi.Spend
	loader := b.eng.handle().Runner.Recorder.(dashi.Loader)
	for turn, loadErr := range b.eng.handle().Turns(t.Context()) {
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		view, err := dashi.Replay(turn)
		if err != nil {
			t.Fatal(err)
		}
		report, err := delegate.Report(t.Context(), loader, view)
		if err != nil {
			t.Fatal(err)
		}
		parentSpend = parentSpend.Add(report.Parent)
		childSpend = childSpend.Add(report.Children)
	}
	if parentSpend.Usage.InputTokens != 250 || childSpend.Usage.InputTokens != 150 || parentSpend.Add(childSpend).Usage.InputTokens != 400 {
		t.Fatalf("scope/dedup parent=%+v child=%+v", parentSpend, childSpend)
	}
}
func TestTaskStopDoesNotCancelForegroundParent(t *testing.T) {
	childHold, parentHold := make(chan struct{}), make(chan struct{})
	client := &taskFixtureClient{childHold: childHold, parentHold: parentHold, childStarted: make(chan struct{})}
	a, _ := taskTestApp(t, client, newRecorderUI())
	a.ctrl.Input("Delegate and keep working")
	select {
	case <-client.childStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("child never sent")
	}
	task := waitTask(t, a, func(v delegate.TaskView) bool { return v.Running() })
	if err := a.ctrl.taskAction(t.Context(), task.ID, "stop", ""); err != nil {
		t.Fatal(err)
	}
	if a.ctrl.Mode() != ModeRunning {
		t.Fatalf("parent mode changed=%v", a.ctrl.Mode())
	}
	close(parentHold)
	a.ctrl.Wait()
	if last := a.eng.Last(t.Context()); last == nil || last.View.Stop.Kind != dashi.StopCompleted {
		t.Fatalf("parent stopped=%+v", last)
	}
	snapshot, err := a.eng.inspectTask(t.Context(), task.ID)
	if err != nil || snapshot.View.Stop.Kind != dashi.StopCanceled || snapshot.View.Stop.Projected {
		t.Fatalf("stop=%+v %v", snapshot.View.Stop, err)
	}
}
func TestTaskPrintQuietAndPipeAttentionHaveCleanExit(t *testing.T) {
	cfg := testConfig(t)
	cfg.Client = &taskFixtureClient{}
	var out, notice bytes.Buffer
	if code := runPrint(t.Context(), cfg, Options{Print: "delegate", PrintSet: true, Yes: true}, &out, &notice); code != 0 {
		t.Fatalf("print code=%d stderr=%s", code, notice.String())
	}
	if out.String() != "Task result received.\n" {
		t.Fatalf("stdout=%q", out.String())
	}
	mustHave(t, notice.String(), "Task ")
	var runs atomic.Int32
	installTaskEffect(t, &runs, true)
	cfg = testConfig(t)
	cfg.Client = &taskFixtureClient{effect: true}
	out.Reset()
	notice.Reset()
	if code := runPrint(t.Context(), cfg, Options{Print: "delegate", PrintSet: true, Yes: true}, &out, &notice); code != 3 || out.String() != "Parent remains available.\n" {
		t.Fatalf("unresolved print code=%d stdout=%q stderr=%s", code, out.String(), notice.String())
	}
	mustHave(t, notice.String(), "need attention")
	// EOF cannot invent a reply for a child question or hang waiting for it.
	ui := newLineUI(&out)
	cfg = testConfig(t)
	cfg.Client = &taskFixtureClient{effect: true}
	a, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	defer a.eng.Close()
	ui.ctrl = a.ctrl
	if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- ui.serveLines(strings.NewReader("delegate\ny\ny\n"), true) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pipe attention returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pipe hung")
	}
}
func TestTaskCommandIDUsesFullCanonicalPayload(t *testing.T) {
	target := delegate.TaskTarget{ID: "t_fixture", TurnID: "child/1", Revision: 7}
	a, err := taskCommandID(target, "continue", "", dashi.Continue{ModelCalls: 64})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := taskCommandID(target, "continue", "", dashi.Continue{ModelCalls: 65})
	if a == b {
		t.Fatal("distinct limit changes shared command identity")
	}
	c, _ := taskCommandID(target, "discard", "", dashi.Cancel{Reason: "one"})
	d, _ := taskCommandID(target, "discard", "", dashi.Cancel{Reason: "two"})
	if c == d {
		t.Fatal("different cancellation payload shared identity")
	}
	changed := target
	changed.Revision++
	e, _ := taskCommandID(changed, "continue", "", dashi.Continue{ModelCalls: 64})
	if e == a {
		t.Fatal("revision excluded")
	}
}

type taskQueueCrashRecorder struct {
	*filerecorder.Recorder
	mu         sync.Mutex
	mode       string
	lost       bool
	target     string
	unreadable bool
}

func (r *taskQueueCrashRecorder) Commit(ctx context.Context, req dashi.CommitRequest) error {
	r.mu.Lock()
	lose := !r.lost && req.Step.Kind == dashi.StepTurnStarted && strings.HasPrefix(req.Step.CommandID, "queue:")
	if lose {
		r.lost = true
		r.target = req.Step.TurnID
	}
	mode := r.mode
	r.mu.Unlock()
	if lose && mode == "absent" {
		return errors.New("queue Start absent acknowledgement")
	}
	if err := r.Recorder.Commit(ctx, req); err != nil {
		return err
	}
	if lose {
		return errors.New("queue Start acknowledgement lost")
	}
	return nil
}
func (r *taskQueueCrashRecorder) Load(ctx context.Context, id string) (dashi.Turn, error) {
	r.mu.Lock()
	blocked := r.unreadable && r.lost && id == r.target
	r.mu.Unlock()
	if blocked {
		return dashi.Turn{}, errors.New("queue record temporarily unavailable")
	}
	return r.Recorder.Load(ctx, id)
}
func TestTaskNoticeUnknownAdmissionPausesUntilExactReconciliation(t *testing.T) {
	for _, mode := range []string{"absent", "committed"} {
		t.Run(mode, func(t *testing.T) {
			client := &taskFixtureClient{}
			ui := newRecorderUI()
			cfg := testConfig(t)
			cfg.Client = client
			var recorder *taskQueueCrashRecorder
			cfg.recorder = func(r *filerecorder.Recorder) dashi.Recorder {
				recorder = &taskQueueCrashRecorder{Recorder: r, mode: mode, unreadable: true}
				return recorder
			}
			a, err := buildApp(t.Context(), cfg, ui)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = a.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
				t.Fatal(err)
			}
			defer a.eng.Close()
			a.ctrl.mu.Lock()
			a.ctrl.autoTaskDelivery = false
			a.ctrl.mu.Unlock()
			if _, err = a.eng.Prompt(t.Context(), "delegate"); err != nil {
				t.Fatal(err)
			}
			waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopCompleted) })
			// Wait also establishes that the notice callback returned.
			if _, err = a.eng.taskHost().tasks.Wait(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err = a.eng.DriveTaskNotices(t.Context()); err == nil {
				t.Fatal("unknown queue admission returned success")
			}
			parent, child := client.counts()
			if parent != 2 || child != 1 {
				t.Fatalf("unknown admission dispatched %d/%d", parent, child)
			}
			a.ctrl.mu.Lock()
			a.ctrl.autoTaskDelivery = true
			a.ctrl.mu.Unlock()
			a.ctrl.scheduleTaskNotices()
			a.ctrl.Wait()
			again, _ := client.counts()
			if again != parent {
				t.Fatal("automatic unknown-admission retry dispatched")
			}
			a.ctrl.Input("unrelated new prompt")
			a.ctrl.Wait()
			again, _ = client.counts()
			if again != parent {
				t.Fatal("unrelated work replaced queued admission")
			}
			recorder.mu.Lock()
			recorder.unreadable = false
			recorder.mu.Unlock()
			a.ctrl.Input("")
			a.ctrl.Wait()
			parent, child = client.counts()
			if parent != 3 || child != 1 {
				t.Fatalf("reconciled calls=%d/%d transcript=%s", parent, child, ui.text())
			}
			task := a.eng.taskHost().tasks.List()[0]
			count := 0
			for turn, err := range a.eng.handle().Turns(t.Context()) {
				if err != nil {
					t.Fatal(err)
				}
				for _, step := range turn.Steps {
					if step.Kind == dashi.StepTurnStarted && strings.Contains(step.CommandID, delegate.NoticeID(task.TurnID)) {
						count++
					}
				}
			}
			if count != 1 {
				t.Fatalf("completion admission count=%d", count)
			}
		})
	}
}
func TestTaskDetailsSeparateAttemptEstimateFromDispatchAndHidePrivateContent(t *testing.T) {
	turn := dashi.Turn{ID: "child", Steps: []dashi.Step{
		{Kind: dashi.StepModelCallStarted, Model: &dashi.ModelFact{ModelCallID: "not-sent", EstimatedInputTokens: 1234}},
		{Kind: dashi.StepModelCallFinished, Model: &dashi.ModelFact{ModelCallID: "not-sent", NotSent: true, EstimatedUsage: true}},
		{Kind: dashi.StepModelCallStarted, Model: &dashi.ModelFact{ModelCallID: "unknown"}},
		{Kind: dashi.StepModelCallFinished, Model: &dashi.ModelFact{ModelCallID: "unknown", SendStatus: llm.SendStatusUnknown, ResultUnknown: true}},
		{Kind: dashi.StepModelCallStarted, Model: &dashi.ModelFact{ModelCallID: "sent"}},
		{Kind: dashi.StepModelCallFinished, Model: &dashi.ModelFact{ModelCallID: "sent", Result: &llm.Result{Message: llm.AssistantMessage(llm.Text("public answer"))}}},
		{Kind: dashi.StepModelCallFinished, Model: &dashi.ModelFact{Purpose: dashi.ModelPurposeCompaction, Result: &llm.Result{Message: llm.AssistantMessage(llm.Text("private summary"))}}},
	}}
	calls, unresolved := taskDispatchCounts(turn)
	if calls != 2 || unresolved != 1 {
		t.Fatalf("counts=%d/%d", calls, unresolved)
	}
	details := taskDetails(taskSnapshot{Bound: true, Turn: turn, View: dashi.TurnView{ID: "child"}})
	if strings.Contains(details, "private summary") || !strings.Contains(details, "public answer") || !strings.Contains(details, "2 confirmed model dispatches") {
		t.Fatal(details)
	}
	row := taskRow(delegate.TaskView{TurnID: "child", ModelCalls: 3})
	if !strings.Contains(row.Text, "3 model attempts") {
		t.Fatal(row.Text)
	}
}

type unavailableTaskClient struct{ fakeClient }

func (*unavailableTaskClient) Provider() string { return "changed-provider" }
func TestTaskReopenInspectRecordedChildWithoutAvailableBinding(t *testing.T) {
	client := &taskFixtureClient{}
	ui := newRecorderUI()
	a, cfg := taskTestApp(t, client, ui)
	result, err := a.eng.Prompt(t.Context(), "delegate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.eng.QuietTasks(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	task := a.eng.taskHost().tasks.List()[0]
	sessionID := a.eng.Session().ID
	a.eng.Close()
	unavailable := &unavailableTaskClient{}
	cfg.Client = unavailable
	b, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.openSession(cfg.Store, cfg.Dir, Options{Resume: true, ResumeID: sessionID}); err != nil {
		t.Fatal(err)
	}
	defer b.eng.Close()
	b.startMCP(true)
	b.ctrl.Wait()
	snapshot, err := b.eng.inspectTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Bound || snapshot.View.Stop.Kind != dashi.StopCompleted || snapshot.Task.ModelCalls != 1 {
		t.Fatalf("recorded child=%+v", snapshot)
	}
	before := unavailable.sends
	b.ctrl.Input("/tasks")
	b.ctrl.Wait()
	if unavailable.sends != before {
		t.Fatal("offline inspection dispatched model")
	}
	mustHave(t, ui.text(), "Children:")
	mustHave(t, taskDetails(snapshot), "live binding is unavailable")
	if err = b.ctrl.taskAction(t.Context(), task.ID, "continue", ""); err == nil {
		t.Fatal("binding failure widened controls")
	}
}

func TestTaskAttemptLimitRequiresExactConfirmedRaise(t *testing.T) {
	client := &taskFixtureClient{loopUntil: 32}
	questions := 0
	ui := &questionFixtureUI{recorderUI: newRecorderUI(), ask: func(_ context.Context, q Question) (questionAnswer, error) {
		questions++
		if !strings.Contains(q.Prompt, "from 32 to 64") || !strings.Contains(q.Prompt, "not a dollar cap") {
			t.Error(q.Prompt)
		}
		return questionAnswer{choices: []int{1}}, nil
	}}
	a, _ := taskTestApp(t, client, ui)
	if _, err := a.eng.Prompt(t.Context(), "delegate"); err != nil {
		t.Fatal(err)
	}
	task := waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopLimited) })
	snapshot, err := a.eng.inspectTask(t.Context(), task.ID)
	if err != nil || snapshot.View.Next() != dashi.NextRaiseModelCalls {
		t.Fatalf("limit=%+v err=%v", snapshot.View.Stop, err)
	}
	oldTurn := snapshot.View.ID
	if err = a.ctrl.taskAction(t.Context(), task.ID, "continue", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = a.eng.QuietTasks(t.Context(), Result{}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = a.eng.inspectTask(t.Context(), task.ID)
	if err != nil || snapshot.View.ID != oldTurn || snapshot.View.Stop.Kind != dashi.StopCompleted || questions != 1 {
		t.Fatalf("raised=%+v questions=%d err=%v", snapshot.View.Stop, questions, err)
	}
}
func TestTaskDiscardExplicitlyClosesWaitingEffectWithoutRepeat(t *testing.T) {
	var runs atomic.Int32
	installTaskEffect(t, &runs, true)
	client := &taskFixtureClient{effect: true}
	ui := &questionFixtureUI{recorderUI: newRecorderUI(), ask: func(_ context.Context, q Question) (questionAnswer, error) {
		if !strings.Contains(q.Prompt, "Uncertain effects may already have run") {
			t.Error(q.Prompt)
		}
		return questionAnswer{choices: []int{1}}, nil
	}}
	a, _ := taskTestApp(t, client, ui)
	if _, err := a.eng.Prompt(t.Context(), "delegate"); err != nil {
		t.Fatal(err)
	}
	task := waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopWaiting) })
	if err := a.ctrl.taskAction(t.Context(), task.ID, "discard", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.eng.QuietTasks(t.Context(), Result{}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := a.eng.inspectTask(t.Context(), task.ID)
	if err != nil || snapshot.View.Stop.Kind != dashi.StopCanceled || runs.Load() != 1 {
		t.Fatalf("discard=%+v runs=%d err=%v", snapshot.View.Stop, runs.Load(), err)
	}
}

func TestTaskUnavailableChildStaysVisibleAndBlocksNewWork(t *testing.T) {
	for _, damage := range []string{"corrupt", "missing", "missing-before-notice"} {
		t.Run(damage, func(t *testing.T) {
			var effects atomic.Int32
			installTaskEffect(t, &effects, false)
			client := &taskFixtureClient{effect: true}
			ui := newRecorderUI()
			a, cfg := taskTestApp(t, client, ui)
			result, err := a.eng.Prompt(t.Context(), "delegate")
			if err != nil {
				t.Fatal(err)
			}
			if damage != "missing-before-notice" {
				if _, err = a.eng.QuietTasks(t.Context(), result); err != nil {
					t.Fatal(err)
				}
			} else {
				waitTask(t, a, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopCompleted) })
			}
			task := a.eng.taskHost().tasks.List()[0]
			meta := a.eng.Session()
			rec, err := cfg.Store.Recorder(meta)
			if err != nil {
				t.Fatal(err)
			}
			path := rec.Path(task.TurnID)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			beforeParent, beforeChild := client.counts()
			a.eng.Close()
			if strings.HasPrefix(damage, "missing") {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte("damaged record\n"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			b, err := buildApp(t.Context(), cfg, ui)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = b.openSession(cfg.Store, cfg.Dir, Options{Resume: true, ResumeID: meta.ID}); err != nil {
				t.Fatal(err)
			}
			defer b.eng.Close()
			if err = b.eng.recoverTasks(t.Context()); err == nil {
				t.Fatal("unavailable child recovery succeeded")
			}
			listed := b.eng.taskHost().list()
			if len(listed) != 1 || listed[0].ID != task.ID || listed[0].State != "record unavailable" {
				t.Fatalf("damaged child disappeared: %+v", listed)
			}
			if _, err = b.eng.inspectTask(t.Context(), task.ID); err == nil {
				t.Fatal("damage inferred success")
			}
			if _, err = b.eng.QuietTasks(t.Context(), Result{}); err == nil {
				t.Fatal("damaged child reported quiet success")
			}
			b.ctrl.taskSpend()
			if !strings.Contains(ui.text(), "accounting incomplete") {
				t.Fatal("missing records claimed exact accounting", ui.text())
			}
			if _, err = b.eng.Prompt(t.Context(), "unrelated new work"); err == nil {
				t.Fatal("unavailable child permitted new work")
			}
			if _, err = b.eng.DriveTaskNotices(t.Context()); err == nil {
				t.Fatal("unavailable child permitted notice dispatch")
			}
			if err = b.ctrl.taskAction(t.Context(), task.ID, "continue", ""); err == nil {
				t.Fatal("unavailable child allowed replacement continuation")
			}
			parent, child := client.counts()
			if parent != beforeParent || child != beforeChild || effects.Load() != 1 {
				t.Fatalf("damaged child retried: %d/%d", parent, child)
			}

			b.eng.Close()
			if err = os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := buildApp(t.Context(), cfg, ui)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.openSession(cfg.Store, cfg.Dir, Options{Resume: true, ResumeID: meta.ID}); err != nil {
				t.Fatal(err)
			}
			defer c.eng.Close()
			if err = c.eng.recoverTasks(t.Context()); err != nil {
				t.Fatal(err)
			}
			snapshot, err := c.eng.inspectTask(t.Context(), task.ID)
			if err != nil || snapshot.View.Stop.Kind != dashi.StopCompleted || snapshot.Task.ID != task.ID {
				t.Fatal("restored task unavailable", err)
			}
			afterParent, afterChild := client.counts()
			if afterParent != beforeParent || afterChild != beforeChild || effects.Load() != 1 {
				t.Fatal("restoration repeated dispatch/effect")
			}

		})
	}
}

func TestTaskQuitDuringChildApprovalInterruptsBeforeDialogDismissal(t *testing.T) {
	for _, action := range []string{"quit", "ctrl-d"} {
		t.Run(action, func(t *testing.T) {
			var runs atomic.Int32
			installTaskEffect(t, &runs, true)
			client := &taskFixtureClient{effect: true}
			ui := newRecorderUI()
			asked := make(chan struct{})
			ui.answer = func(ctx context.Context, r ApprovalRequest) (Approval, error) {
				if r.Tool != "external_effect" {
					return Approval{Allow: true}, nil
				}
				close(asked)
				<-ctx.Done()
				return Approval{}, ctx.Err()
			}
			a, cfg := taskTestApp(t, client, ui)
			a.ctrl.Input("delegate")
			select {
			case <-asked:
			case <-time.After(5 * time.Second):
				t.Fatal("child approval missing")
			}
			task := a.eng.taskHost().tasks.List()[0]
			meta := a.eng.Session()
			if action == "quit" {
				a.ctrl.QuitDuringRun("/quit")
			} else {
				a.ctrl.QuitDuringRun("Ctrl-D")
			}
			done := make(chan struct{})
			go func() { a.ctrl.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("exit hung behind child approval")
			}
			snapshot, err := a.eng.inspectTask(t.Context(), task.ID)
			if err != nil || snapshot.View.Stop.Kind != dashi.StopInterrupted || runs.Load() != 0 {
				t.Fatalf("child stop=%+v effects=%d err=%v", snapshot.View.Stop, runs.Load(), err)
			}
			a.eng.Close()
			nextUI := newRecorderUI()
			b, err := buildApp(t.Context(), cfg, nextUI)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = b.openSession(cfg.Store, cfg.Dir, Options{Resume: true, ResumeID: meta.ID}); err != nil {
				t.Fatal(err)
			}
			defer b.eng.Close()
			b.ctrl.mu.Lock()
			b.ctrl.autoTaskDelivery = false
			b.ctrl.mu.Unlock()
			if err = b.eng.recoverTasks(t.Context()); err != nil {
				t.Fatal(err)
			}
			recovered := waitTask(t, b, func(v delegate.TaskView) bool { return v.State == delegate.TaskState(dashi.StopWaiting) })
			b.ctrl.Wait()
			if recovered.ID != task.ID || runs.Load() != 1 || len(nextUI.approvals) != 1 || !strings.Contains(nextUI.approvals[0].Summary, "call effect-call") {
				t.Fatalf("approval lost/duplicated task=%+v effects=%d prompts=%+v", recovered, runs.Load(), nextUI.approvals)
			}
		})
	}
}

func TestChildApprovalInspectionAndStopRetainsOtherOriginalCall(t *testing.T) {
	var effects atomic.Int32
	installTaskEffect(t, &effects, true)
	childHold := make(chan struct{})
	client := &taskFixtureClient{effect: true, secondTask: true, childHold: childHold, parentHold: make(chan struct{})}
	ui := newRecorderUI()
	shown := make(chan ApprovalRequest, 4)
	// Use the actual line answer channel, bound to each broker prompt.
	a, _ := taskTestApp(t, client, ui)
	ui.answer = func(ctx context.Context, r ApprovalRequest) (Approval, error) {
		if r.Tool != "external_effect" {
			return Approval{Allow: true}, nil
		}
		shown <- r
		select {
		case <-a.ctrl.Answers():
			return Approval{Allow: true}, nil
		case <-ctx.Done():
			return Approval{}, ctx.Err()
		}
	}
	a.ctrl.Input("delegate two")
	deadline := time.Now().Add(5 * time.Second)
	for len(a.eng.taskHost().tasks.List()) != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(a.eng.taskHost().tasks.List()) != 2 {
		t.Fatal("two children not admitted")
	}
	close(childHold)
	var first ApprovalRequest
	select {
	case first = <-shown:
	case <-time.After(5 * time.Second):
		t.Fatal("missing child prompt")
	}
	owner, ok := a.ctrl.broker.activeOwner()
	if !ok || owner.TaskID == "" {
		t.Fatal("child owner missing")
	}
	id := delegate.TaskID(owner.TaskID)
	a.ctrl.Input("/tasks")
	a.ctrl.Input("/task " + id)
	deadline = time.Now().Add(5 * time.Second)
	for !strings.Contains(ui.text(), "Child Turn: "+owner.Call.TurnID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(ui.text(), "Child Turn: "+owner.Call.TurnID) || !strings.Contains(ui.text(), "Arguments: {\"target\":\"once\"}") {
		t.Fatal("dialog inspection failed", ui.text())
	}
	if a.ctrl.Interrupt() {
		t.Fatal("Ctrl-C exited during child prompt")
	}
	// The foreground parent is interrupted; both children retain their own owners.
	deadline = time.Now().Add(5 * time.Second)
	for a.eng.Last(t.Context()).View.Stop.Kind != dashi.StopInterrupted && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	after, active := a.ctrl.broker.activeOwner()
	if !active || after != owner || effects.Load() != 0 {
		t.Fatal("foreground interrupt changed child owner", after)
	}
	a.ctrl.Input("/task-stop " + id)
	var second ApprovalRequest
	select {
	case second = <-shown:
	case <-time.After(5 * time.Second):
		t.Fatal("selective stop did not expose other child prompt", ui.text())
	}
	other, active := a.ctrl.broker.activeOwner()
	if !active || other.TaskID == owner.TaskID || second.Arguments != first.Arguments || second.Tool != first.Tool {
		t.Fatal("wrong replacement approval", other, second)
	}
	waitTask(t, a, func(v delegate.TaskView) bool { return v.ID == id && v.State == delegate.TaskState(dashi.StopCanceled) })
	snapshot, err := a.eng.inspectTask(t.Context(), id)
	if err != nil || snapshot.View.Stop.Kind != dashi.StopCanceled || effects.Load() != 0 {
		t.Fatal("selective stop did not cancel exact child", snapshot.View.Stop, err)
	}
	a.ctrl.Input("/tasks")
	a.ctrl.Input("/task " + delegate.TaskID(other.TaskID))
	a.ctrl.Input("yes")
	waitTask(t, a, func(v delegate.TaskView) bool {
		return v.ID == delegate.TaskID(other.TaskID) && v.State == delegate.TaskState(dashi.StopWaiting)
	})
	a.ctrl.Wait()
	if effects.Load() != 1 || a.ctrl.Mode() != ModeIdle {
		t.Fatal("other child/process lost", effects.Load(), a.ctrl.Mode())
	}
	if snapshot, err = a.eng.inspectTask(t.Context(), id); err != nil || snapshot.View.Stop.Kind != dashi.StopCanceled {
		t.Fatal("stale answer revived canceled task")
	}
}

func TestIdleInterruptKeepsRunningChildrenUntilExplicitExit(t *testing.T) {
	client := &taskFixtureClient{childHold: make(chan struct{}), childStarted: make(chan struct{})}
	ui := newRecorderUI()
	a, _ := taskTestApp(t, client, ui)
	if _, err := a.eng.Prompt(t.Context(), "delegate"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.childStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("missing child")
	}
	if a.ctrl.Interrupt() {
		t.Fatal("idle Ctrl-C exited with running child")
	}
	task := a.eng.taskHost().tasks.List()[0]
	if !task.Running() {
		t.Fatal("idle interrupt stopped child")
	}
	mustHave(t, ui.text(), "No foreground work to interrupt; 1 child tasks continue")
	if err := a.ctrl.taskAction(t.Context(), task.ID, "stop", ""); err != nil {
		t.Fatal(err)
	}
}

func TestTaskDiscoverySkipsUnrelatedParentWithoutClaimingCompleteInventory(t *testing.T) {
	h := newHarness(t, say("First."), say("Second."), say("New request works."))
	h.input("first")
	h.input("second")
	rec, err := h.store.Recorder(h.app.eng.Session())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(rec.Path(session.TurnID(h.app.eng.Session().ID, 1)), []byte("damaged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h.app.eng.Close()
	h.ui = newRecorderUI()
	h.build(Options{Resume: true})
	if err = h.app.eng.recoverTasks(t.Context()); err != nil {
		t.Fatal("unrelated parent blocked discovery", err)
	}
	h.input("new request")
	mustHave(t, h.ui.text(), "New request works.")
	if _, err = h.app.eng.QuietTasks(t.Context(), Result{}); err == nil {
		t.Fatal("skipped parent certified quiet inventory")
	}
	h.app.ctrl.Input("/tasks")
	mustHave(t, h.ui.text(), "Task inventory incomplete")
	if strings.Contains(h.ui.text(), "No delegated tasks in this session.") {
		t.Fatal("skipped parent certified empty inventory")
	}
	h.app.ctrl.taskSpend()
	mustHave(t, h.ui.text(), "Task-inclusive accounting incomplete")
}

func TestReadableTaskNoticeRequiresUnreadableOriginalParent(t *testing.T) {
	client := &taskFixtureClient{}
	ui := newRecorderUI()
	a, cfg := taskTestApp(t, client, ui)
	result, err := a.eng.Prompt(t.Context(), "delegate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.eng.QuietTasks(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	task := a.eng.taskHost().tasks.List()[0]
	meta := a.eng.Session()
	rec, err := cfg.Store.Recorder(meta)
	if err != nil {
		t.Fatal(err)
	}
	a.eng.Close()
	if err = os.WriteFile(rec.Path(task.Parent.TurnID), []byte("damaged parent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	parentCalls, childCalls := client.counts()
	b, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.openSession(cfg.Store, cfg.Dir, Options{Resume: true, ResumeID: meta.ID}); err != nil {
		t.Fatal(err)
	}
	defer b.eng.Close()
	if err = b.eng.recoverTasks(t.Context()); err == nil || !strings.Contains(err.Error(), "required parent") {
		t.Fatal("notice lost required parent", err)
	}
	listed := b.eng.taskHost().list()
	if len(listed) != 1 || listed[0].ID != task.ID || listed[0].State != "record unavailable" {
		t.Fatal("required parent task disappeared", listed)
	}
	if _, err = b.eng.Prompt(t.Context(), "replacement"); err == nil {
		t.Fatal("required parent allowed replacement")
	}
	if _, err = b.eng.QuietTasks(t.Context(), Result{}); err == nil {
		t.Fatal("required parent claimed quiet")
	}
	if _, err = b.eng.DriveTaskNotices(t.Context()); err == nil {
		t.Fatal("required parent allowed redelivery")
	}
	afterParent, afterChild := client.counts()
	if afterParent != parentCalls || afterChild != childCalls {
		t.Fatal("required parent repeated dispatch")
	}
}

func TestUnreadableParentBeforeNoticeBlocksHiddenChildAndAllowsNewSession(t *testing.T) {
	client := &taskFixtureClient{childHold: make(chan struct{}), childStarted: make(chan struct{})}
	ui := newRecorderUI()
	a, cfg := taskTestApp(t, client, ui)
	if _, err := a.eng.Prompt(t.Context(), "delegate"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.childStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not start")
	}
	task := a.eng.taskHost().tasks.List()[0]
	meta := a.eng.Session()
	var parents []dashi.Turn
	for turn, err := range a.eng.handle().Turns(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		parents = append(parents, turn)
	}
	if len(recordedTaskNotices(parents)) != 0 {
		t.Fatal("fixture already contains a parent notice")
	}
	rec, err := cfg.Store.Recorder(meta)
	if err != nil {
		t.Fatal(err)
	}
	a.eng.Close()
	if err = os.WriteFile(rec.Path(task.Parent.TurnID), []byte("damaged parent\n"), 0600); err != nil {
		t.Fatal(err)
	}
	parentCalls, childCalls := client.counts()
	b, err := buildApp(t.Context(), cfg, ui)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.openSession(cfg.Store, cfg.Dir, Options{Resume: true, ResumeID: meta.ID}); err != nil {
		t.Fatal(err)
	}
	defer b.eng.Close()
	if _, err = b.eng.Prompt(t.Context(), "replacement"); err == nil || !strings.Contains(err.Error(), "may hide child") {
		t.Fatal("hidden child admitted new work", err)
	}
	if _, err = b.eng.QuietTasks(t.Context(), Result{}); err == nil {
		t.Fatal("hidden child certified quiet")
	}
	b.ctrl.Input("/tasks")
	mustHave(t, ui.text(), "Task inventory incomplete")
	afterParent, afterChild := client.counts()
	if afterParent != parentCalls || afterChild != childCalls {
		t.Fatal("recovery dispatched work")
	}
	if _, err = b.openSession(cfg.Store, cfg.Dir, Options{}); err != nil {
		t.Fatal(err)
	}
	if b.eng.Session().ID == meta.ID {
		t.Fatal("recovery decision retained damaged session")
	}
	if _, err = b.eng.Prompt(t.Context(), "separate work"); err != nil {
		t.Fatal("new session blocked", err)
	}
}
