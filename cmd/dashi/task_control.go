package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/tool"
	"github.com/deepnoodle-ai/dashi/workspace"
)

func (c *Controller) taskInput(line string) bool {
	name, arg := splitCommand(line)
	if name != "/tasks" && name != "/task" && name != "/task-stop" {
		return false
	}
	if name == "/tasks" {
		if strings.ContainsAny(arg, " \t") {
			c.notice("Usage: /tasks [ID]")
			return true
		}
		if arg != "" {
			// One task's turns, with the messages between them.
			show := func(ctx context.Context) {
				text, err := c.eng.taskConversation(ctx, arg)
				if err != nil {
					c.notice("Task %s unavailable: %v", arg, err)
					return
				}
				c.notice("%s", text)
			}
			if c.Mode() == ModeIdle {
				c.async(show)
			} else {
				c.goWork(func() { show(c.base) })
			}
			return true
		}
		host := c.eng.taskHost()
		if host != nil {
			host.mu.Lock()
			incomplete := host.inventoryErr
			host.mu.Unlock()
			if incomplete != nil {
				c.notice("Task inventory incomplete; unreadable parent history was skipped: %v", incomplete)
			}
			if incomplete != nil && len(host.list()) == 0 {
				return true
			}
		}
		if host == nil || len(host.list()) == 0 {
			c.notice("No delegated tasks in this session.")
			return true
		}
		// One list at the bottom. Status lines keep their places under the
		// tool lines that started the tasks.
		lines := []string{"Tasks:"}
		for _, task := range host.list() {
			if owner, active := c.broker.activeOwner(); active && owner.TaskID == task.SessionID && task.Running() {
				task.State = taskApprovalPending
			}
			lines = append(lines, "  "+taskSummary(task))
		}
		lines = append(lines, "/tasks ID shows its turns and messages; /task ID shows details.")
		c.notice("%s", strings.Join(lines, "\n"))
		c.goWork(c.taskSpend)
		return true
	}
	fields := strings.Fields(arg)
	if len(fields) == 0 || name == "/task-stop" && len(fields) != 1 {
		c.notice("Usage: /task ID [continue|done CALL_ID|discard] or /task-stop ID")
		return true
	}
	action := "inspect"
	callID := ""
	if name == "/task-stop" {
		action = "stop"
	} else if len(fields) > 1 {
		action = fields[1]
	}
	if action == "done" && len(fields) == 3 {
		callID = fields[2]
	} else if len(fields) > 2 || action == "done" {
		c.notice("Usage: /task ID done CALL_ID")
		return true
	}
	id := fields[0]
	if (c.Mode() == ModeApproval || c.Mode() == ModeCommand) && action != "inspect" && action != "stop" {
		c.notice("During a pending dialog, use /tasks, /task ID, or /task-stop ID. Answer or close it before resolution commands.")
		return true
	}
	if action == "inspect" {
		show := func(ctx context.Context) {
			snapshot, err := c.eng.inspectTask(ctx, id)
			if err != nil {
				c.notice("Task %s unavailable: %v", id, err)
				return
			}
			if owner, active := c.broker.activeOwner(); active && owner.TaskID == snapshot.Task.SessionID {
				snapshot.Prompt = &owner.Call
			}
			if viewer, ok := c.ui.(interface {
				ChooseTaskDetails(context.Context, string, string)
			}); ok && c.Mode() == ModeCommand {
				_ = c.broker.presentOnly(ctx, func() { viewer.ChooseTaskDetails(ctx, id, taskDetails(snapshot)) })
			} else {
				c.notice("%s", taskDetails(snapshot))
			}
		}
		if c.Mode() == ModeIdle {
			c.async(show)
		} else {
			c.goWork(func() { show(c.base) })
		}
		return true
	}
	if action != "stop" && action != "continue" && action != "done" && action != "discard" {
		c.notice("Unknown task action %s. Use continue, done CALL_ID, or discard.", action)
		return true
	}
	work := func(ctx context.Context) {
		if err := c.taskAction(ctx, id, action, callID); err != nil {
			c.notice("Task %s action did not complete: %v", id, err)
		}
		c.scheduleTaskNotices()
	}
	if c.Mode() == ModeIdle {
		c.async(work)
	} else {
		c.goWork(func() { work(c.base) })
	}
	return true
}

// taskCommandID binds the full target and payload by JSON meaning. Original
// Pending approval builders keep their own bound response identities.
func taskCommandID(target delegate.TaskTarget, action, call string, command dashi.Command) (string, error) {
	raw, err := json.Marshal(struct {
		ID       string
		Turn     string
		Revision int64
		Action   string
		Call     string
		Payload  dashi.Command
	}{target.ID, target.TurnID, target.LastSeq, action, call, command})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(dashi.ArgsHash(raw)))
	return "cli-task:" + hex.EncodeToString(hash[:]), nil
}
func (c *Controller) runTaskCommand(ctx context.Context, snapshot taskSnapshot, action, call string, command dashi.Command) (dashi.RunResult, error) {
	if err := c.eng.recoverTasks(ctx); err != nil {
		return dashi.RunResult{}, err
	}
	id, err := taskCommandID(snapshot.Target, action, call, command)
	if err != nil {
		return dashi.RunResult{}, err
	}
	switch cmd := command.(type) {
	case dashi.Continue:
		cmd.CommandID = id
		command = cmd
	case dashi.Cancel:
		cmd.CommandID = id
		command = cmd
	case dashi.Resolve:
		if cmd.CommandID == "" {
			cmd.CommandID = id
		}
		command = cmd
	}
	out, err := c.eng.taskHost().tasks.Run(ctx, snapshot.Target, command)
	if err != nil {
		return out, err
	}
	if fresh, loadErr := c.eng.inspectTask(ctx, snapshot.Task.ID); loadErr == nil {
		c.emit(taskStatusBlock(fresh.Task))
	}
	return out, nil
}
func (c *Controller) confirmTask(ctx context.Context, snapshot taskSnapshot, callID, text string) error {
	owner := brokerOwner{TaskID: snapshot.Task.SessionID, Call: dashi.CallRef{TurnID: snapshot.View.ID, CallID: callID}}
	ctx = context.WithValue(ctx, brokerOwnerKey{}, owner)
	answer, err := c.Ask(ctx, Question{Prompt: text, Choices: []workspace.Choice{{Label: "Cancel"}, {Label: "Confirm exact task action"}}})
	if err != nil {
		return err
	}
	if answer.Text != "Confirm exact task action" {
		return fmt.Errorf("operator did not confirm")
	}
	return nil
}

// recordedApproval is the approval request for a recorded pending call of a
// stopped task. A workspace bash call gets its command and a decide slot, so
// the command-risk check reads it as it reads a live call, in bypass mode
// too. recordedResponse records the slot's answer with the decision.
func (c *Controller) recordedApproval(ctx context.Context, turnID string, p dashi.Pending) (context.Context, ApprovalRequest) {
	r := ApprovalRequest{Tool: p.Name, Summary: p.Name, Args: p.Args, RememberScope: "This exact recorded task call only"}
	if p.Name != workspace.ToolBash || p.ProviderTool != nil {
		return ctx, r
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(p.Args, &in) != nil || in.Command == "" {
		return ctx, r
	}
	first, _, multi := strings.Cut(in.Command, "\n")
	if multi {
		first += " ..."
	}
	r.Command, r.Dir, r.Summary = in.Command, c.dir, "run: "+first
	plan := dashi.ToolPlan{Turn: dashi.TurnView{ID: turnID}, Call: tool.Call{ID: p.ID, Name: p.Name, Args: p.Args}}
	return context.WithValue(ctx, decideSlotKey{}, &decideSlot{plan: plan}), r
}

// recordedResponse is the bound decision on a recorded pending call. It
// keeps the decide answer the prompt showed, which ctx's slot holds, in the
// rules fields a live call uses, which the model never reads.
func recordedResponse(ctx context.Context, p dashi.Pending, answer Approval) dashi.Resolve {
	response := p.Deny(answer.Reason)
	if answer.Allow {
		response = p.Approve()
	}
	if r, ok := decideSlotFrom(ctx).take(); ok {
		response = response.WithRules(r.rulesVersion(), r.recordLine())
	}
	return response
}

func (c *Controller) taskAction(ctx context.Context, id, action, callID string) error {
	host := c.eng.taskHost()
	if host == nil {
		return delegate.ErrUnknownTask
	}
	snapshot, err := c.eng.inspectTask(ctx, id)
	if err != nil {
		return err
	}
	if !snapshot.Bound {
		return fmt.Errorf("restore the recorded task binding and reopen this session before controls")
	}
	if action == "stop" {
		if err := host.tasks.Stop(ctx, id); err != nil {
			return err
		}
		fresh, err := c.eng.inspectTask(ctx, id)
		if err != nil {
			return err
		}
		if fresh.View.Stop.Kind != dashi.StopCanceled || fresh.View.Stop.Inferred {
			return fmt.Errorf("cancellation acknowledgement unresolved; inspect this exact task again")
		}
		c.emit(taskStatusBlock(fresh.Task))
		return nil
	}
	if snapshot.Task.Running() {
		return delegate.ErrTaskBusy
	}
	if snapshot.View.Stop.Ended {
		return fmt.Errorf("task has ended")
	}
	switch action {
	case "done":
		var pending *dashi.Pending
		for i := range snapshot.View.Pending {
			p := &snapshot.View.Pending[i]
			if p.ID == callID && p.Kind == dashi.PendingTool && p.Tool != nil && (p.Tool.Status == dashi.ToolStarted || p.Tool.Status == dashi.ToolWaiting || p.Tool.Status == dashi.ToolResultUnknown) {
				pending = p
				break
			}
		}
		if pending == nil {
			return fmt.Errorf("call %s is not an already-started pending effect", callID)
		}
		if err := c.confirmTask(ctx, snapshot, callID, fmt.Sprintf("Verify that this exact external effect completed. It may still be running.\nTask %s · Turn %s · step %d\nCall %s · tool %s\nArguments: %s\nNo tool runs and no output is recovered by this action.", id, snapshot.View.ID, snapshot.View.LastSeq, callID, pending.Name, string(pending.Args))); err != nil {
			return err
		}
		outcome := tool.Succeeded("Operator verified this exact call completed; its output was not captured.")
		_, err = c.runTaskCommand(ctx, snapshot, action, callID, dashi.Resolve{CallID: callID, Outcome: &outcome})
		return err
	case "discard":
		if err := c.confirmTask(ctx, snapshot, "", fmt.Sprintf("Discard task %s, Turn %s, step %d? Effects whose result is unknown may already have run. This does not prove they stopped.", id, snapshot.View.ID, snapshot.View.LastSeq)); err != nil {
			return err
		}
		_, err = c.runTaskCommand(ctx, snapshot, action, "", dashi.Cancel{Reason: "Operator discarded this exact stopped task; effects whose result is unknown may have run."})
		return err
	case "continue":
		// Every original approval still waiting gets its own bound recorded response.
		for _, p := range snapshot.View.Pending {
			if p.Kind != dashi.PendingApproval {
				return fmt.Errorf("pending %s %s must be resolved explicitly before continuation", p.Kind, p.ID)
			}
			owner := brokerOwner{TaskID: snapshot.Task.SessionID, Call: dashi.CallRef{TurnID: snapshot.View.ID, CallID: p.ID}}
			askCtx, request := c.recordedApproval(context.WithValue(ctx, brokerOwnerKey{}, owner), snapshot.View.ID, p)
			answer, err := c.Approve(askCtx, request)
			if err != nil {
				return err
			}
			if _, err := c.runTaskCommand(ctx, snapshot, action, p.ID, recordedResponse(askCtx, p, answer)); err != nil {
				return err
			}
			snapshot, err = c.eng.inspectTask(ctx, id)
			if err != nil {
				return err
			}
		}
		cmd := dashi.Continue{}
		switch snapshot.View.Next() {
		case dashi.NextContinue:
		case dashi.NextRecover:
			if !snapshot.View.ReadyToContinue {
				return fmt.Errorf("recorded recovery is not ready")
			}
		case dashi.NextRaiseModelCalls:
			limit := snapshot.View.Stop.Limit
			if limit == nil || limit.Limit <= 0 || limit.Limit > int64(math.MaxInt/2) {
				return fmt.Errorf("recorded model-call cap cannot be safely doubled")
			}
			cmd.ModelCalls = int(limit.Limit) * 2
			if err := c.confirmTask(ctx, snapshot, "", fmt.Sprintf("Raise the exact task %s Turn %s model-call cap from %d to %d? This is not a dollar cap.", id, snapshot.View.ID, limit.Limit, cmd.ModelCalls)); err != nil {
				return err
			}
		case dashi.NextFix:
			if !snapshot.View.ReadyToContinue || snapshot.View.Stop.Kind != dashi.StopFailed && snapshot.View.Stop.Kind != dashi.StopRefused {
				return fmt.Errorf("resolve the recorded cause before continuation")
			}
			if err := c.confirmTask(ctx, snapshot, "", fmt.Sprintf("Task %s stopped: %s. Confirm that its external remedy is complete before continuing this exact Turn %s.", id, snapshot.View.Stop.Reason, snapshot.View.ID)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("task has no permitted continuation")
		}
		_, err = c.runTaskCommand(ctx, snapshot, action, "", cmd)
		return err
	}
	return fmt.Errorf("unsupported task action")
}

func (c *Controller) taskSpend() {
	chat := c.eng.handle()
	if chat == nil {
		return
	}
	loader, ok := chat.Runner.Recorder.(dashi.Loader)
	if !ok {
		c.notice("Task-inclusive accounting unavailable: recorder cannot load authoritative records.")
		return
	}
	var turns []dashi.Turn
	for turn, err := range chat.Turns(c.base) {
		if err != nil {
			c.notice("Task-inclusive accounting incomplete: %v", err)
			return
		}
		turns = append(turns, turn)
	}
	if host := c.eng.taskHost(); host != nil {
		if err := host.readRecorded(c.base, turns); err != nil {
			for _, task := range host.list() {
				if task.State == "record unavailable" {
					c.emit(taskStatusBlock(task))
				}
			}
			c.notice("Task-inclusive accounting incomplete; child and inclusive totals unavailable: %v", err)
			return
		}
	}
	var parent, children dashi.Spend
	for _, turn := range turns {
		view, err := turn.View()
		if err != nil {
			c.notice("Task-inclusive accounting incomplete: %v", err)
			return
		}
		report, err := delegate.Report(c.base, loader, view)
		if err != nil {
			c.notice("Task-inclusive accounting incomplete: %v", err)
			return
		}
		parent = parent.Add(report.Parent)
		children = children.Add(report.Children)
	}
	if c.eng.handle() != chat {
		return
	}
	c.notice("Session %s spend (live lower bounds while work runs):\nParent: %s\nChildren: %s\nInclusive: %s\nParent and child money/tool costs remain separate. Recorded parent caps exclude children. Explore: 24 model calls; General: 32 model calls; no child monetary cap.", chat.ID, parent.String(), children.String(), parent.Add(children).String())
}
