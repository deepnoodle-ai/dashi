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
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
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
	// ModeApproval waits for an answer to an approval prompt. Ctrl-C quits.
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
	Approve(ctx context.Context, r ApprovalRequest) (Approval, error)
	// Ask asks the user a question for the model.
	Ask(ctx context.Context, q Question) (questionAnswer, error)
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
	eng            *Engine
	ui             UI
	store          *session.Store
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
	dropping         string
	answers          chan string
	wg               sync.WaitGroup
	work             atomic.Int64
	answerPending    atomic.Bool
	base             context.Context
	statusGeneration uint64
	statusCancel     context.CancelFunc
}

// NewController returns a controller. base ends every run when it is done.
func NewController(
	base context.Context,
	eng *Engine,
	ui UI,
	store *session.Store,
	dir string,
) *Controller {
	return &Controller{
		eng:            eng,
		ui:             ui,
		store:          store,
		dir:            dir,
		attachmentRoot: attachmentWorkspace(dir),
		base:           base,
		answers:        make(chan string, 1),
	}
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
		c.answerPending.Store(false)
	}
	c.mu.Unlock()
}

// Wait blocks until every background action ends.
func (c *Controller) Wait() {
	c.wg.Wait()
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

// goWork counts work before scheduling it. WaitForInput can stop at a dialog
// without leaving a WaitGroup waiter behind when the next command begins.
func (c *Controller) goWork(fn func()) {
	c.work.Add(1)
	c.wg.Go(func() { defer c.work.Add(-1); fn() })
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
		c.Interrupt()
	}
}

// Approve is the ApprovalFunc the wiring uses. It marks the approval mode
// while the user decides.
func (c *Controller) Approve(
	ctx context.Context,
	r ApprovalRequest,
) (Approval, error) {
	c.setMode(ModeApproval)
	defer c.setMode(ModeRunning)
	// The request text is the model's. The UI gets the printable form only.
	return c.ui.Approve(ctx, r.Printable())
}

// Ask is the AskFunc the wiring uses.
func (c *Controller) Ask(ctx context.Context, q Question) (workspace.Answer, error) {
	previousMode := c.Mode()
	c.setMode(ModeApproval)
	defer c.setMode(previousMode)
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
	a, err := c.ui.Ask(ctx, q)
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
}

// Interrupt handles Ctrl-C. It returns true when the program should exit.
// When prompts are queued, the first Ctrl-C at the prompt only warns that
// quitting drops them.
func (c *Controller) Interrupt() bool {
	if c.Mode() == ModeIdle && c.keepQueued("Ctrl-C") {
		return false
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
		return false
	case ModeApproval:
		// Quitting during a prompt cancels the run. Dashi records the turn
		// as interrupted, and the call had not started. As with
		// QuitDuringRun, queued prompts get a warning once the run settles.
		c.quit, c.quitAction = true, "Ctrl-C"
		if c.cancel != nil {
			c.cancel()
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
}

// Input handles one submitted line.
func (c *Controller) Input(line string) {
	raw := line
	line = strings.TrimSpace(line)
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
	// Screen admission belongs to screen.submit on the Wonton event loop.
	// Only the synchronous line reader admits attachments through Controller.
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
	}
}

// QuitDuringRun handles an exit action (/quit, /exit, or Ctrl-D) while a
// turn runs: it cancels the run, and the program quits once the run
// settles. When prompts are then still queued, it warns instead, as the
// same action at the prompt would, and repeating the action quits.
func (c *Controller) QuitDuringRun(action string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.statusCancel != nil {
		c.statusCancel()
		c.statusGeneration++
	}
	c.quit, c.quitAction = true, action
	if c.cancel != nil {
		c.cancel()
	}
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
	c.startRun(func(ctx context.Context) (Result, error) {
		return c.eng.Prompt(ctx, text)
	})
}

func (c *Controller) resume() {
	if !c.eng.HasPending() && !c.eng.Last(c.base).CanGoOn() {
		return
	}
	c.startRun(c.eng.Resume)
}

// startRun runs fn in the background in the running mode. Steers that
// missed the turn, and queued prompts, run as the next turns within fn.
func (c *Controller) startRun(fn func(context.Context) (Result, error)) {
	ctx, cancel := context.WithCancel(c.base)
	c.mu.Lock()
	c.mode, c.cancel = ModeRunning, cancel
	c.mu.Unlock()
	c.goWork(func() {
		_, err := fn(ctx)
		cancel()
		c.mu.Lock()
		c.mode, c.cancel = ModeIdle, nil
		quit, action := c.quit, c.quitAction
		c.quit, c.quitAction = false, ""
		if action != "" {
			// The warning below is the first for this action.
			c.dropping = ""
		}
		c.mu.Unlock()
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
}

// busyText says what to do when the session refuses a prompt, or returns
// the error's own text.
func busyText(err error) string {
	var busy *dashi.TurnBusyError
	if !errors.As(err, &busy) {
		return err.Error()
	}
	n := turnNumber(busy.TurnID)
	switch {
	case busy.CallID != "":
		return fmt.Sprintf("The last command (call %s in turn %d) may still "+
			"be running. Type /done once it has finished, or /discard to "+
			"discard the turn.", busy.CallID, n)
	case busy.Next == dashi.NextAnswer:
		return fmt.Sprintf("Turn %d waits for an answer. Press Enter to "+
			"continue it, or type /discard to discard it.", n)
	case busy.Next == dashi.NextContinue || busy.Next == dashi.NextResume:
		return fmt.Sprintf("Turn %d is still open. Press Enter to continue it, "+
			"or type /discard to discard it before compacting.", n)
	case busy.Next == dashi.NextRaiseAttempts:
		return fmt.Sprintf("Turn %d reached its model-call limit. Type /continue "+
			"to raise the limit, or /discard to discard it before compacting.", n)
	}
	return err.Error()
}

// async runs fn in the command mode.
func (c *Controller) async(fn func(context.Context)) {
	ctx, cancel := context.WithCancel(c.base)
	c.mu.Lock()
	c.mode, c.cancel = ModeCommand, cancel
	c.mu.Unlock()
	c.goWork(func() {
		defer func() {
			cancel()
			c.mu.Lock()
			c.mode, c.cancel = ModeIdle, nil
			quit, action := c.quit, c.quitAction
			c.quit, c.quitAction = false, ""
			c.mu.Unlock()
			if quit && (action == "" || !c.keepQueued(action)) {
				c.ui.Quit()
			}
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
