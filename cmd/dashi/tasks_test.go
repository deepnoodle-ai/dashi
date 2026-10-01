package main

import (
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
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
	client := &taskFixtureClient{}
	ui := newRecorderUI()
	a, _ := taskTestApp(t, client, ui)
	result, err := a.eng.Prompt(t.Context(), "Delegate this job")
	if err != nil {
		t.Fatal(err)
	}
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
// is delivered: no task-ended reminder follows and no extra turn runs, now or
// after a reopen. Either order of the child's end and the read holds.
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
			if parent != 3 || child != 1 || client.parentNotices != 0 {
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
