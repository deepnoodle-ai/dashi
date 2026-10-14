package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/catalog"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/sessionstore"
	"github.com/deepnoodle-ai/dashi/delegate"
	"github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/workspace"
)

// Mode is what the prompt is doing now. It decides what typed text and Ctrl-C
// mean.
type Mode int

const (
	// ModeIdle takes a command or a new prompt. Ctrl-C exits.
	ModeIdle Mode = iota
	// ModeRunning is a turn in progress. Typed text is a steer. Ctrl-C
	// cancels the model call or the running tool.
	ModeRunning
	// ModeApproval waits for an answer. Ctrl-C interrupts the foreground owner.
	ModeApproval
	// ModeCommand runs a slash command that may wait for the user, such as
	// a picker. Input is ignored until it ends.
	ModeCommand
)

// UI is what the Controller needs from a screen. The screen, line mode, and
// tests implement it.
type UI interface {
	// Emit shows a block. It may be called from any goroutine.
	Emit(Block)
	// Reset clears the transcript, before a new or reopened session shows.
	Reset()
	// Approve asks the user about a tool call and blocks until the answer.
	Approve(ctx context.Context, a toolApproval) (Approval, error)
	// Ask asks the user a question for the model.
	Ask(ctx context.Context, q taskQuestion) (questionAnswer, error)
	// Quit ends the program.
	Quit()
}

// Chooser is an optional part of UI: a picker. A UI without it gets a printed
// list instead.
type Chooser interface {
	// Choose shows options and returns the index picked. False means the
	// user canceled.
	Choose(ctx context.Context, title string, options []string) (int, bool)
}

// Controller connects the input line to the Engine. It knows the input modes
// and the slash commands. It never blocks its caller, so a screen can call it
// from its event loop.
type Controller struct {
	eng *Engine
	// approveAll approves every tool call without asking, for
	// --dangerously-skip-permissions.
	approveAll bool
	// decide runs the decide checks; nil when every feature is off.
	decide *decideRuntime
	// decideOn is which decide features are on, for /settings.
	decideOn       decideSettings
	ui             UI
	store          *sessionstore.Store
	dir            string
	attachmentRoot string

	mu     sync.Mutex
	mode   Mode
	cancel context.CancelFunc
	quit   bool
	// quitAction is the exit action (/quit, /exit, or Ctrl-D) that ended
	// the run, when one did. Queued prompts then stop the quit once.
	quitAction string
	// dropping is the action (a slash command, or Ctrl-C) that was just
	// warned about dropping queued prompts; repeating it goes ahead.
	dropping string
	answers  chan string
	// work counts background actions. workIdle is closed when the count
	// falls to zero; workMu guards both changes. A WaitGroup does not fit:
	// task wakes start work at any time, also while Wait waits.
	workMu           sync.Mutex
	workIdle         chan struct{}
	work             atomic.Int64
	answerPending    atomic.Bool
	base             context.Context
	statusGeneration uint64
	statusCancel     context.CancelFunc
	broker           *approvalBroker
	workerMode       Mode
	taskPump         bool
	autoTaskDelivery bool
	// knownNames caches command names for highlighting in the composer.
	knownNames commandNameCache
	// decideWarned is the end of the last pause warned about, by feature; a pause
	// that starts before it ends gets no second warning.
	decideMu     sync.Mutex
	decideWarned map[string]time.Time
	// watchWarned holds the causes of failed turn-watcher checks already
	// warned about.
	watchWarned map[string]bool
	// turnEvents gets a line when dashi waits for the person, and the end
	// of each run; nil without --turn-events.
	turnEvents *turnEventLog
}

// NewController returns a controller. base ends every run when it is done.
func NewController(
	base context.Context,
	eng *Engine,
	ui UI,
	store *sessionstore.Store,
	dir string,
) *Controller {
	c := &Controller{
		eng:            eng,
		ui:             ui,
		store:          store,
		dir:            dir,
		attachmentRoot: attachmentWorkspace(dir),
		base:           base,
		answers:        make(chan string, 1),
	}
	_, print := ui.(*printUI)
	c.autoTaskDelivery = !print
	c.broker = newApprovalBroker(c.presentTaskPrompt, nil)
	return c
}

// Mode returns the current input mode.
func (c *Controller) Mode() Mode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

// emit preserves source for transcript selection and sanitizes direct output.
func (c *Controller) emit(b Block) {
	emitToUI(c.ui, b)
}

func (c *Controller) setMode(m Mode) {
	c.mu.Lock()
	c.mode = m
	if m != ModeApproval {
		c.workerMode = m
	}
	if m != ModeApproval {
		c.answerPending.Store(false)
	}
	c.mu.Unlock()
}

// Wait blocks until every background action ends.
func (c *Controller) Wait() {
	c.workMu.Lock()
	idle := c.workIdle
	c.workMu.Unlock()
	if idle != nil {
		<-idle
	}
}

// WaitForInput blocks until every background action ends or a run waits for
// an approval answer. Then the next input line is wanted.
func (c *Controller) WaitForInput() {
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if c.work.Load() == 0 || (c.Mode() == ModeApproval && !c.answerPending.Load() && len(c.answers) == 0) {
			return
		}
		<-tick.C
	}
}

// goWork counts fn as background work before it starts, so Wait and
// WaitForInput see it at once. The first action opens workIdle and the last
// one to end closes it, which releases Wait.
func (c *Controller) goWork(fn func()) {
	c.workMu.Lock()
	if c.work.Add(1) == 1 {
		c.workIdle = make(chan struct{})
	}
	c.workMu.Unlock()
	go func() {
		defer func() {
			c.workMu.Lock()
			if c.work.Add(-1) == 0 {
				close(c.workIdle)
				c.workIdle = nil
			}
			c.workMu.Unlock()
		}()
		fn()
	}()
}

// CancelUnanswered cancels the run when it waits for an approval answer that
// no input can supply. It is for the end of piped input. An answer that is
// already queued, or is being read, keeps the run alive.
func (c *Controller) CancelUnanswered() {
	for range 3 {
		if c.Mode() != ModeApproval || len(c.answers) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c.Mode() == ModeApproval {
		c.mu.Lock()
		c.cancelExitDialogLocked()
		c.mu.Unlock()
	}
}

// approveUI asks the UI about r and marks the approval mode while the user
// decides. task is the short ID of the delegated task that asks, or empty.
func (c *Controller) approveUI(
	ctx context.Context,
	r ApprovalRequest,
	task string,
) (Approval, error) {
	slot := c.decide.checkedCommand(ctx, r)
	if c.approveAll {
		// approveBypass lets only a call the check holds into the queue.
		result, held := slot.take()
		if !held || !result.holds() {
			return Approval{Allow: true}, nil
		}
		return c.askHeld(ctx, r, task, result)
	}
	if slot != nil && !c.canAnswer() {
		// Every approval is refused here; a check would spend for nothing.
		slot = nil
	}
	c.setMode(ModeApproval)
	defer c.endDialog()
	c.waiting(notificationPermissionPrompt, r)
	// The request text is the model's. The UI gets the printable form only.
	a := toolApproval{ApprovalRequest: r.Printable(), Task: task}
	if slot != nil {
		// The prompt shows at once; the check's answer joins it.
		a.Check = c.decide.startCheck(ctx, slot.plan)
	}
	answer, err := c.ui.Approve(ctx, a)
	if slot != nil {
		result := a.Check.finish()
		slot.set(result)
		if result.Flagged {
			answer.Remember = false
		}
	}
	return answer, err
}

// waiting writes the turn event that says a tool call waits for approval.
func (c *Controller) waiting(kind string, r ApprovalRequest) {
	message := "Dashi needs your permission to use " + r.Printable().Tool
	if w := c.turnEvents.failure(c.turnEvents.waiting(kind, message)); w != "" {
		c.emit(warnBlock("%s", w))
	}
}

// canAnswer reports whether a person can answer a prompt. The run command
// has no one to ask.
func (c *Controller) canAnswer() bool {
	_, print := c.ui.(*printUI)
	return !print
}

// approveBypass decides a call in bypass mode. Every call runs, except a
// shell command the command-risk check holds: decide flags it, or cannot
// check it for a reason the command itself can cause. The check runs here,
// before the approval queue, so checks from parallel tasks run at once and
// other calls never wait behind one. Only a held call enters the queue to
// ask; with no one to ask, it is refused. Model advice never approves:
// bypass mode, which the person chose, does.
func (c *Controller) approveBypass(ctx context.Context, r ApprovalRequest) (Approval, error) {
	slot := c.decide.checkedCommand(ctx, r)
	if slot == nil {
		return Approval{Allow: true}, nil
	}
	owner, _ := promptOwner(ctx)
	row := owner.TaskID == "" && c.canAnswer()
	if row {
		c.emit(c.bypassRow(slot.plan, StateChecking))
	}
	result := c.decide.check(ctx, slot.plan)
	if err := ctx.Err(); err != nil {
		return Approval{}, err
	}
	slot.set(result)
	if !result.holds() {
		if result.Paused {
			c.warnPause(decideCommandRisk)
		}
		return Approval{Allow: true}, nil
	}
	if !c.canAnswer() {
		what := "flagged this command (severe " + percent(result.Severe) + ")"
		if !result.Flagged {
			what = "could not check this command (" + result.Cause + ")"
		}
		return Approval{Reason: "The decide command-risk check " + what + ", and no one can " +
			"approve it in a non-interactive run. Do not retry it. Tell the user what you " +
			"wanted to run and why, so they can run it themselves."}, nil
	}
	if row {
		// The prompt shows the answer; the row says only that it waits.
		c.emit(c.bypassRow(slot.plan, StateWaitingForYou))
	}
	return c.broker.approve(ctx, r)
}

// askHeld asks about a shell command the check held in bypass mode, with
// allow once and deny only.
func (c *Controller) askHeld(ctx context.Context, r ApprovalRequest, task string, result decideResult) (Approval, error) {
	c.setMode(ModeApproval)
	defer c.endDialog()
	c.waiting(notificationPermissionPrompt, r)
	answer, err := c.ui.Approve(ctx, toolApproval{ApprovalRequest: r.Printable(), Task: task, Check: doneCheck(result), Hold: true})
	answer.Remember = false
	return answer, err
}

// bypassRow is the tool row of a bash call in bypass mode before it
// starts: StateChecking while decide checks it, or StateWaitingForYou while
// it waits for the person's answer. The renderer's row replaces it.
func (c *Controller) bypassRow(plan dashi.ToolPlan, state string) Block {
	return Block{
		ID:    "t:" + plan.Call.ID,
		Kind:  KindTool,
		Name:  plan.Call.Name,
		Arg:   mainArg(plan.Call.Args, c.dir),
		State: state,
	}
}

// warnWatchFailed says once per cause, where there is no status area,
// that the turn watcher could not check, such as after a revoked key.
// /settings counts every failure.
func (c *Controller) warnWatchFailed(cause string) {
	if c.isScreen() {
		return
	}
	c.decideMu.Lock()
	seen := c.watchWarned[cause]
	if !seen {
		if c.watchWarned == nil {
			c.watchWarned = map[string]bool{}
		}
		c.watchWarned[cause] = true
	}
	c.decideMu.Unlock()
	if !seen {
		c.emit(warnBlock("decide: turn_watcher could not check (%s), so the agent got no reminder. "+
			"Later failures of this kind are not shown.", cause))
	}
}

// isScreen reports whether the UI has a status area.
func (c *Controller) isScreen() bool {
	_, ok := c.ui.(*screen)
	return ok
}

// pausedWhat says what goes unchecked while a feature is paused.
var pausedWhat = map[string]string{
	decideCommandRisk:     "shell commands run unchecked",
	decidePromptInjection: "tool results reach the agent unchecked",
}

// warnPause says once per pause of feature, where there is no status area,
// what goes unchecked and until when.
func (c *Controller) warnPause(feature string) {
	if c.isScreen() {
		return
	}
	until, cause, ok := c.decide.pausedUntil(feature)
	if !ok {
		return
	}
	c.decideMu.Lock()
	seen := c.decideWarned[feature].After(time.Now())
	if !seen {
		if c.decideWarned == nil {
			c.decideWarned = map[string]time.Time{}
		}
		c.decideWarned[feature] = until
	}
	c.decideMu.Unlock()
	if !seen {
		c.emit(warnBlock("decide: %s paused (%s); %s until %s.",
			feature, cause, pausedWhat[feature], until.Format(time.TimeOnly)))
	}
}

// askUI asks the UI a question for the model. task is the short ID of the
// delegated task that asks, or empty.
func (c *Controller) askUI(ctx context.Context, q Question, task string) (workspace.Answer, error) {
	c.setMode(ModeApproval)
	defer c.endDialog()
	if w := c.turnEvents.failure(c.turnEvents.waiting(notificationPermissionPrompt, "Dashi has a question for you")); w != "" {
		c.emit(warnBlock("%s", w))
	}
	original := q.Choices
	q.Prompt = workspace.PrintableText(q.Prompt)
	choices := make([]workspace.Choice, len(q.Choices))
	for i, ch := range q.Choices {
		choices[i] = workspace.Choice{
			Label:       workspace.PrintableText(ch.Label),
			Description: workspace.PrintableText(ch.Description),
		}
	}
	q.Choices = choices
	a, err := c.ui.Ask(ctx, taskQuestion{Question: q, Task: task})
	if err != nil {
		return workspace.Answer{}, err
	}
	if len(a.choices) == 0 {
		return workspace.Answer{Text: a.text}, nil
	}
	labels := make([]string, len(a.choices))
	for i, index := range a.choices {
		if index < 0 || index >= len(original) {
			return workspace.Answer{}, fmt.Errorf("answer choice is out of range")
		}
		labels[i] = original[index].Label
	}
	if !q.Multiple {
		if len(labels) != 1 {
			return workspace.Answer{}, fmt.Errorf("question accepts one choice")
		}
		return workspace.Answer{Text: labels[0]}, nil
	}
	return workspace.Answer{Selections: labels}, nil
}

// Answers is the channel that line mode reads an approval answer from.
func (c *Controller) Answers() <-chan string {
	return c.answers
}

// Open shows the scrollback of the open session and says what state its last
// turn is in.
func (c *Controller) Open() {
	ctx := c.base
	for _, b := range c.eng.Scrollback(ctx) {
		c.emit(b)
	}
	last := c.eng.Last(ctx)
	id, name := last.OpenCall()
	switch {
	case id != "":
		c.notice("Turn %d stopped while %s was running (call %s), and it may "+
			"still be running. Type /done once it has finished, or /discard "+
			"to discard the turn.", last.N, name, id)
	case last.LimitStopped():
		// The limit stop block is already in the scrollback.
	case last.Retryable():
		c.emit(Block{
			Kind: KindNotice,
			Text: fmt.Sprintf(
				"Turn %d %s. Press Enter to retry it, "+
					"or type a new prompt to cancel it.", last.N, last.StopText()),
			Done: true,
		})
	case last.CanGoOn():
		c.emit(Block{
			Kind: KindNotice,
			Text: fmt.Sprintf(
				"Turn %d %s. Press Enter to continue it, "+
					"or type a new prompt to cancel it.", last.N, last.StopText()),
			Done: true,
		})
	case last.Open():
		c.emit(Block{
			Kind: KindNotice,
			Text: fmt.Sprintf(
				"Turn %d %s and cannot continue. "+
					"Type a new prompt to cancel it.", last.N, last.StopText()),
			Done: true,
		})
	}
	// On a session switch the runtime is already initialized. Recovery is
	// background work because a recorded child binding can require a dialog.
	if c.eng.cfg.MCP == nil || c.eng.cfg.MCP.Ready() {
		c.goWork(func() {
			if err := c.eng.recoverTasks(ctx); err != nil {
				c.notice("Task recovery incomplete: %v", err)
			}
			c.scheduleTaskNotices()
		})
	}

}

// Interrupt handles Ctrl-C. It returns true when the program should exit.
// When prompts are queued, the first Ctrl-C at the prompt only warns that
// quitting drops them.
func (c *Controller) Interrupt() bool {
	if c.Mode() == ModeIdle {
		if host := c.eng.taskHost(); host != nil {
			running := 0
			for _, task := range host.tasks.List() {
				if task.Running() {
					running++
				}
			}
			if running > 0 {
				c.notice("No foreground work to interrupt; %d child tasks continue. Use /tasks and /task-stop ID, or /quit to exit.", running)
				return false
			}
		}
		if c.keepQueued("Ctrl-C") {
			return false
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.statusCancel != nil {
		c.statusCancel()
		c.statusGeneration++
	}
	switch c.mode {
	case ModeRunning, ModeCommand:
		if c.cancel != nil {
			c.cancel()
		}
		if host := c.eng.taskHost(); host != nil {
			running := 0
			for _, v := range host.tasks.List() {
				if v.Running() {
					running++
				}
			}
			if running > 0 {
				c.notice("Foreground interrupted; %d child tasks continue. Use /tasks and /task-stop ID to stop a child.", running)
			}
		}
		return false
	case ModeApproval:
		if c.cancel != nil {
			c.cancel()
		}
		owner, active := c.broker.activeOwner()
		if active && owner.TaskID != "" {
			c.notice("Foreground interrupted; child approval stays pending. Use /tasks, /task %s, or /task-stop %s to control that child.", delegate.TaskID(owner.TaskID), delegate.TaskID(owner.TaskID))
		} else {
			c.broker.cancelActive()
		}
		return false
	}
	return true
}

// CancelWork interrupts a turn or dialog without requesting process exit.
func (c *Controller) CancelWork() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.statusCancel != nil {
		c.statusCancel()
		c.statusGeneration++
	}
	if c.cancel != nil {
		c.cancel()
	}
	c.broker.cancelActive()
}

// Input handles one submitted line.
func (c *Controller) Input(line string) {
	raw := line
	line = strings.TrimSpace(line)
	if c.taskInput(line) {
		return
	}
	if name, arg := splitCommand(line); name == "/status" {
		c.inspectSession(arg)
		return
	}
	if c.mcpInput(line) {
		return
	}
	switch c.Mode() {
	case ModeApproval:
		pending := c.answerPending.Swap(true)
		select {
		case c.answers <- line:
		default:
			c.answerPending.Store(pending)
		}
		return
	case ModeCommand:
		return
	case ModeRunning:
		c.steer(line)
		return
	}
	// Screen input is accepted by screen.submit on the Wonton event loop.
	// Only the synchronous line reader accepts attachments through Controller.
	if u, ok := c.ui.(interface{ attachmentLineInput(string) bool }); ok && !strings.HasPrefix(line, "/") && u.attachmentLineInput(raw) {
		return
	}
	name, _ := splitCommand(line)
	if strings.HasPrefix(name, "/") {
		for _, entry := range c.commands() {
			if strings.EqualFold(entry.name, name) || slices.Contains(entry.aliases, name) {
				name = entry.name
				break
			}
		}
	}
	c.mu.Lock()
	if name != c.dropping {
		c.dropping = ""
	}
	c.mu.Unlock()
	switch {
	case line == "":
		c.resume()
	case strings.HasPrefix(line, "/"):
		c.command(line)
	default:
		c.prompt(line)
	}
}

func (c *Controller) steer(line string) {
	if line == "" {
		return
	}
	if len(attachmentPaths(line)) > 0 {
		c.notice("Attachments need a fresh idle submission; no files were read or sent.")
		return
	}
	if c.duringTurnCommand(line) {
		return
	}
	urgent := false
	if text, ok := strings.CutPrefix(line, "!"); ok {
		line, urgent = strings.TrimSpace(text), true
	}
	if err := c.eng.Steer(line, urgent); err != nil {
		c.emit(warnBlock("The input %q was not sent: %v.", line, err))
		return
	}
	if w := c.turnEvents.failure(c.turnEvents.steered(line)); w != "" {
		c.emit(warnBlock("%s", w))
	}
}

// QuitDuringRun handles an exit action (/quit, /exit, or Ctrl-D) while a
// turn runs: it cancels the run, and the program quits once the run
// finishes. When prompts are then still queued, it warns instead, as the
// same action at the prompt would, and repeating the action quits.
func (c *Controller) QuitDuringRun(action string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.statusCancel != nil {
		c.statusCancel()
		c.statusGeneration++
	}
	c.quit, c.quitAction = true, action
	c.cancelExitDialogLocked()
}

// queue holds text as the next prompt.
func (c *Controller) queue(text string) {
	if strings.TrimSpace(text) == "" {
		c.notice("/queue needs the text of the next prompt.")
		return
	}
	if err := c.eng.Queue(strings.TrimSpace(text)); err != nil {
		c.emit(warnBlock("The prompt was not queued: %v.", err))
	}
}

func (c *Controller) prompt(text string) {
	if host := c.eng.taskHost(); host != nil {
		host.mu.Lock()
		paused := host.deliveryErr != nil
		host.mu.Unlock()
		if paused {
			c.notice("Couldn't confirm that a task notice was saved. Press Enter to check it, or reopen the session before sending a new prompt.")
			return
		}
	}
	c.startRun(func(ctx context.Context) (Result, error) {
		return c.eng.Prompt(ctx, text)
	})
}

func (c *Controller) resume() {
	if host := c.eng.taskHost(); host != nil {
		host.mu.Lock()
		paused := host.deliveryErr != nil
		host.mu.Unlock()
		if paused && !c.eng.HasPending() {
			c.startRun(func(ctx context.Context) (Result, error) {
				if c.eng.Last(ctx).CanGoOn() {
					res, err := c.eng.Resume(ctx)
					if err != nil || res.Outcome.Next() != dashi.NextNone {
						return res, err
					}
				}
				return c.eng.DriveTaskNotices(ctx)
			})
			return
		}
	}
	if !c.eng.HasPending() && !c.eng.Last(c.base).CanGoOn() {
		return
	}
	c.startRun(c.eng.Resume)
}

// startRun runs fn in the background in the running mode. Steers that
// missed the turn, and queued prompts, run as the next turns within fn.
func (c *Controller) startRun(fn func(context.Context) (Result, error)) bool {
	ctx, cancel := context.WithCancel(c.base)
	c.mu.Lock()
	if c.workerMode != ModeIdle || c.mode == ModeApproval {
		c.mu.Unlock()
		cancel()
		c.notice("Parent work is already owned by another driver; submit again when idle.")
		return false
	}
	c.workerMode, c.cancel = ModeRunning, cancel
	if c.mode != ModeApproval {
		c.mode = ModeRunning
	}
	c.mu.Unlock()
	c.goWork(func() {
		_, err := fn(ctx)
		cancel()
		c.mu.Lock()
		// The turn's end is written before the mode allows a new prompt, so
		// a reader that sends one after it never steers this run.
		eventErr := c.turnEvents.idle(err)
		c.workerMode, c.cancel = ModeIdle, nil
		if c.mode != ModeApproval {
			c.mode = ModeIdle
		}
		quit, action := c.quit, c.quitAction
		c.quit, c.quitAction = false, ""
		if action != "" {
			// The warning below is the first for this action.
			c.dropping = ""
		}
		c.mu.Unlock()
		if w := c.turnEvents.failure(eventErr); w != "" {
			c.emit(warnBlock("%s", w))
		}
		c.scheduleTaskNotices()
		if err != nil && !errors.Is(err, context.Canceled) {
			var attachmentErr *attachmentFailure
			if errors.As(err, &attachmentErr) {
				if ui, ok := c.ui.(interface{ AttachmentWarning(string) }); ok {
					ui.AttachmentWarning(busyText(err))
				} else {
					c.emit(warnBlock("%s", busyText(err)))
				}
			} else {
				c.emit(warnBlock("%s", busyText(err)))
			}
		}
		if quit && action != "" && c.keepQueued(action) {
			return
		}
		if quit {
			c.ui.Quit()
		}
	})
	return true
}

// busyText says what to do when the session refuses a prompt, or returns
// the error's own text.
func busyText(err error) string {
	var busy *session.TurnBusyError
	if !errors.As(err, &busy) {
		return err.Error()
	}
	n := turnNumber(busy.TurnID)
	switch {
	case busy.CallID != "":
		return fmt.Sprintf("The last command (call %s in turn %d) may still "+
			"be running. Type /done once it has finished, or /discard to "+
			"discard the turn.", busy.CallID, n)
	case busy.Next == dashi.NextResolvePending:
		return fmt.Sprintf("Turn %d waits for an answer. Press Enter to "+
			"continue it, or type /discard to discard it.", n)
	case busy.Next == dashi.NextContinue || busy.Next == dashi.NextRecover:
		return fmt.Sprintf("Turn %d is still open. Press Enter to continue it, "+
			"or type /discard to discard it before compacting.", n)
	case busy.Next == dashi.NextRaiseModelCalls:
		return fmt.Sprintf("Turn %d reached its model-call limit. Type /continue "+
			"to raise the limit, or /discard to discard it before compacting.", n)
	}
	return err.Error()
}

// async runs fn in the command mode.
func (c *Controller) async(fn func(context.Context)) {
	ctx, cancel := context.WithCancel(c.base)
	c.mu.Lock()
	if c.workerMode != ModeIdle || c.mode == ModeApproval {
		c.mu.Unlock()
		cancel()
		c.notice("Wait for the current parent work or dialog before this command.")
		return
	}
	c.workerMode, c.cancel = ModeCommand, cancel
	if c.mode != ModeApproval {
		c.mode = ModeCommand
	}
	c.mu.Unlock()
	c.goWork(func() {
		defer func() {
			cancel()
			c.mu.Lock()
			c.workerMode, c.cancel = ModeIdle, nil
			if c.mode != ModeApproval {
				c.mode = ModeIdle
			}
			quit, action := c.quit, c.quitAction
			c.quit, c.quitAction = false, ""
			c.mu.Unlock()
			if quit && (action == "" || !c.keepQueued(action)) {
				c.ui.Quit()
			}
			c.scheduleTaskNotices()
		}()
		fn(ctx)
	})
}

// keepQueued warns, the first time, that action would drop the prompts
// queued in this session, and reports true so the caller stops. Repeating
// the action goes ahead.
func (c *Controller) keepQueued(action string) bool {
	n := c.eng.Queued()
	attachmentLoss := false
	if u, ok := c.ui.(attachmentUI); ok {
		attachmentLoss = u.AttachmentUnsaved()
	}
	c.mu.Lock()
	warned := c.dropping == action
	c.dropping = ""
	if (n > 0 || attachmentLoss) && !warned {
		c.dropping = action
	}
	c.mu.Unlock()
	if n == 0 && !attachmentLoss || warned {
		return false
	}
	if attachmentLoss {
		c.notice("The unsent attachment draft will be lost. Repeat %s to accept draft loss.", action)
	}
	if n > 0 {
		c.emit(warnBlock("%d queued %s will be lost. Repeat %s to go ahead.",
			n, plural(n, "message"), action))
	}
	if c.eng.HasPending() {
		c.notice("The pending prompt and later queued input are held only in memory and cannot be recovered after exit.")
	}
	return true
}

func (c *Controller) notice(format string, args ...any) {
	c.emit(Block{Kind: KindNotice, Text: fmt.Sprintf(format, args...), Done: true})
}

func (c *Controller) newSession(ctx context.Context) {
	s, err := c.store.Create(c.dir, c.eng.Model())
	if err != nil {
		c.emit(warnBlock("A new session was not created: %v.", err))
		return
	}
	if err := c.eng.Open(s); err != nil {
		c.emit(warnBlock("%v.", err))
		return
	}
	c.ui.Reset()
	c.notice("New session %s.", s.ID)
}

func (c *Controller) model(ctx context.Context, name string) {
	if p := c.eng.cfg.Preferences; p != nil {
		c.destinationModel(ctx, name, p)
		return
	}
	models := modelNames()
	if name == "" {
		idx, ok := c.choose(ctx, "Model", models)
		if !ok {
			return
		}
		name = models[idx]
	}
	if !knownModel(name) {
		c.emit(warnBlock(
			"Model %q is not in the catalog. Keeping %s.", name, c.eng.Model(),
		))
		return
	}
	c.eng.SetModel(name)
	c.notice("Model set to %s for the next turn.", name)
}

// OpenSession switches to the session with the given ID and shows it.
func (c *Controller) openSession(id string) {
	s, err := c.store.LoadScoped(c.base, c.dir, id)
	if err != nil {
		c.emit(warnBlock("%v.", err))
		return
	}
	var restored *launchPreferences
	if p := c.eng.cfg.Preferences; p != nil {
		saved, err := reopenedPreferences(s.NextTurn, s.Model)
		if err != nil {
			c.emit(warnBlock("Session preferences cannot be read: %v.", err))
			return
		}
		candidate, err := p.selectedSessionPreferences(saved)
		if err != nil {
			c.emit(warnBlock("Session preferences cannot be selected: %v.", err))
			return
		}
		restored = &candidate
	}
	if err := c.eng.Open(s); err != nil {
		c.emit(warnBlock("%v.", err))
		return
	}
	if restored != nil {
		p := c.eng.cfg.Preferences
		p.mu.Lock()
		p.launch = *restored
		p.mu.Unlock()
		// Opening a session changes the displayed next-turn model without writing
		// user settings or rewriting the selected session's saved defaults.
		c.eng.mu.Lock()
		c.eng.model = restored.Selection.Model
		c.eng.mu.Unlock()
	}
	if s.Model != "" && c.eng.cfg.Preferences == nil {
		c.eng.SetModel(s.Model)
	}
	c.ui.Reset()
	c.Open()
}

// choose asks the UI to pick from options. A UI without a picker gets a
// printed list, and the command ends without a choice.
func (c *Controller) choose(
	ctx context.Context,
	title string,
	options []string,
) (int, bool) {
	if ch, ok := c.ui.(Chooser); ok {
		return ch.Choose(ctx, title, options)
	}
	c.notice("%s:\n  %s", title, strings.Join(options, "\n  "))
	return 0, false
}

func modelNames() []string {
	var names []string
	for _, e := range catalog.Entries("openai") {
		names = append(names, e.Model)
	}
	return names
}

func knownModel(name string) bool {
	_, ok := catalog.Find("openai", name)
	return ok
}

// Source-preserving screens sanitize at rendering and native-output boundaries.
// Other UIs receive display text, including test recorders and line/print mode.
func emitToUI(ui UI, b Block) {
	if _, retains := ui.(interface{ retainSourceBlocks() }); !retains {
		b = printable(b)
	}
	ui.Emit(b)
}

// Cancel the execution owner before its dialog. A local dialog cancellation
// must not race a live hook into recording an ordinary hook failure. Exiting
// from a child approval closes the child host too: child contexts intentionally
// do not inherit foreground interruption, so canceling only the parent hangs.
// Caller holds c.mu; the child shutdown waits in counted background work.
func (c *Controller) cancelExitDialogLocked() {
	if c.cancel != nil {
		c.cancel()
	}
	owner, active := c.broker.activeOwner()
	if active && owner.TaskID != "" && c.eng != nil {
		host := c.eng.taskHost()
		if host != nil {
			c.goWork(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := host.tasks.Close(ctx); err != nil {
					c.notice("Task shutdown unresolved; records and ownership are retained: %v", err)
					c.broker.cancelActive()
				}
			})
			return
		}
	}
	c.broker.cancelActive()
}
