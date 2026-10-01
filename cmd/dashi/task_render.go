package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/content"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/workspace"
)

func taskRow(view delegate.TaskView) Block {
	if view.State == "record unavailable" {
		return Block{ID: "task:" + view.SessionID, Kind: KindWarning, Text: fmt.Sprintf("Task %s · %s · latest record unavailable\n%s\nParent %s · call %s\n%s\nRecorded state/counts/spend are incomplete; no completion is inferred. Inspect: /task %s", view.ID, view.Subagent, view.Description, view.Parent.TurnID, view.Parent.CallID, view.Reason, view.ID), Done: true}
	}
	text := fmt.Sprintf("Task %s · %s · %s · %d model attempts · %d tool calls\n%s\nChild spend: %s\nInspect: /task %s", view.ID, view.Subagent, view.State, view.ModelCalls, view.ToolCalls, view.Description, view.Spend.String(), view.ID)
	return Block{ID: "task:" + view.TurnID, Kind: KindNotice, Text: text, Done: !view.Running()}
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
		prefix := ""
		if p.Owner.TaskID != "" {
			if c.eng != nil {
				if host := c.eng.taskHost(); host != nil {
					for _, task := range host.list() {
						if task.SessionID == p.Owner.TaskID {
							task.State = "approval pending"
							c.emit(taskRow(task))
							break
						}
					}
				}
			}
			prefix = fmt.Sprintf("Task %s · Turn %s", delegate.TaskID(p.Owner.TaskID), p.Owner.Call.TurnID)
			if p.Owner.Call.CallID != "" {
				prefix += " · call " + p.Owner.Call.CallID
			}
			prefix += "\n"
		} else if p.Owner.Call.TurnID != "" {
			prefix = fmt.Sprintf("Turn %s · call %s\n", p.Owner.Call.TurnID, p.Owner.Call.CallID)
		}
		var response brokerResponse
		var err error
		if p.Approval != nil {
			request := *p.Approval
			request.Summary = prefix + request.Summary
			response.approval, err = c.approveUI(p.ctx, request)
		} else {
			question := *p.Question
			question.Prompt = prefix + question.Prompt
			response.answer, err = c.askUI(p.ctx, question)
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
	if c.eng.HasPending() {
		c.emit(Block{ID: "task-delivery", Kind: KindNotice, Text: "Task results are waiting behind the pending parent admission. Inspection does not consume them.", Done: true})
		return
	}
	if last := c.eng.Last(c.base); last != nil && last.Open() {
		c.emit(Block{ID: "task-delivery", Kind: KindNotice, Text: "Task results are waiting for the stopped parent. Resolve the parent with its existing controls; inspection does not consume results.", Done: true})
		return
	}
	c.mu.Lock()
	if c.mode != ModeIdle || c.taskPump || c.quit {
		c.mu.Unlock()
		return
	}
	c.taskPump = true
	c.mu.Unlock()
	// startRun retains the sole parent command owner. A pending admission or a
	// stopped parent yields without continuation or an inferred permission.
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
		view, err := dashi.Replay(turn)
		if err != nil {
			return taskSnapshot{}, err
		}
		if view.Parent == nil || *view.Parent != task.Parent {
			return taskSnapshot{}, fmt.Errorf("task parent identity differs")
		}
		return taskSnapshot{Task: task, Turn: turn, View: view, Target: delegate.TaskTarget{ID: id, TurnID: turn.ID, Revision: view.Revision}, Bound: true}, nil
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
	fmt.Fprintf(&b, "Task %s · %s\nChild Turn: %s · revision %d\nParent: %s · call %s\nState: %s · stop %s · terminal %t\nDestination: %s / %s / %s\n%s\n", snapshot.Task.ID, snapshot.Task.Subagent, v.ID, v.Revision, snapshot.Task.Parent.TurnID, snapshot.Task.Parent.CallID, snapshot.Task.State, v.Stop.Kind, v.Stop.Terminal, v.Agent.Provider, v.Agent.API, v.Agent.Model, snapshot.Task.Description)
	if snapshot.Prompt != nil {
		fmt.Fprintf(&b, "Live approval/answer pending · Turn %s · original call %s. Inspection does not answer it.\n", snapshot.Prompt.TurnID, snapshot.Prompt.CallID)
	}
	fmt.Fprintf(&b, "Child spend: %s\nModel attempts: %d · tool calls: %d\nEstimated usage calls: %d · usage incomplete %t\nAttempt cap: %d; no child monetary cap. Parent caps exclude children.\n", snapshot.Task.Spend.String(), snapshot.Task.ModelCalls, snapshot.Task.ToolCalls, snapshot.Task.Spend.Estimated, snapshot.Task.Spend.UsageIncomplete, v.Agent.Limits.ModelAttempts)
	calls, unresolved := taskDispatchCounts(snapshot.Turn)
	fmt.Fprintf(&b, "This Turn: %d confirmed model dispatches · %d unresolved intents. Attempts and estimates do not prove Send.\nTokens: input %d · output %d · cache writes %d · cache reads %d; cache-write reporting unavailable %t · total mismatch %t.\n", calls, unresolved, v.Spend.Usage.InputTokens, v.Spend.Usage.OutputTokens, v.Spend.Usage.CacheCreationInputTokens, v.Spend.Usage.CacheReadInputTokens, v.Spend.Usage.CacheCreationInputTokensUnavailable, v.Spend.Usage.TotalTokenCountMismatch)
	if len(snapshot.Turn.Steps) > 0 && snapshot.Turn.Steps[0].Started != nil {
		for _, m := range snapshot.Turn.Steps[0].Started.Input {
			if m.Context != nil {
				fmt.Fprintf(&b, "Admitted resource: %s / %s (recorded snapshot; no file reads)\n", m.Context.Origin, m.Context.Name)
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
					fmt.Fprintf(&b, "Admitted evidence: %s · %d bytes · SHA256 %s · ref %s (unresolved)\n", source.MIMEType, source.Ref.Size, source.Ref.SHA256, source.Ref.ID)
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
	} else if !v.Stop.Terminal {
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

// A model intent establishes an attempt; only settlement evidence establishes
// a confirmed dispatch. Unsettled/unknown outcomes keep separate uncertainty.
func taskDispatchCounts(turn dashi.Turn) (int, int) {
	intents, settled, dispatched := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, step := range turn.Steps {
		f := step.Model
		if f == nil {
			continue
		}
		if step.Kind == dashi.StepModelCallStarted {
			intents[f.AttemptID] = true
			continue
		}
		if step.Kind != dashi.StepModelCallFinished && step.Kind != dashi.StepModelCallFinishedByHost {
			continue
		}
		settled[f.AttemptID] = !f.Uncertain && f.Failure != llm.FailureUnknown
		if f.NoDispatch || f.Failure == llm.FailureNotDispatched {
			continue
		}
		r := f.Result
		if f.Failure == llm.FailureRejected || r != nil && (r.ID != "" || len(r.Message.Content) > 0 || r.Usage != nil || r.Stop.Kind != "") {
			dispatched[f.AttemptID] = true
		}
	}
	unresolved := 0
	for id := range intents {
		if !settled[id] {
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
