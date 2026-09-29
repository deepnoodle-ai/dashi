package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	Ask(ctx context.Context, q Question) (string, error)
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
	eng   *Engine
	ui    UI
	store *session.Store
	dir   string

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
	wg       sync.WaitGroup
	base     context.Context
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
		eng:     eng,
		ui:      ui,
		store:   store,
		dir:     dir,
		base:    base,
		answers: make(chan string, 1),
	}
}

// Mode returns the current input mode.
func (c *Controller) Mode() Mode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

// emit shows b on the UI. Every block goes through printable first.
func (c *Controller) emit(b Block) {
	c.ui.Emit(printable(b))
}

func (c *Controller) setMode(m Mode) {
	c.mu.Lock()
	c.mode = m
	c.mu.Unlock()
}

// Wait blocks until every background action ends.
func (c *Controller) Wait() {
	c.wg.Wait()
}

// WaitForInput blocks until every background action ends or a run waits for
// an approval answer. Then the next input line is wanted.
func (c *Controller) WaitForInput() {
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			if c.Mode() == ModeApproval {
				return
			}
		}
	}
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
func (c *Controller) Ask(ctx context.Context, q Question) (string, error) {
	c.setMode(ModeApproval)
	defer c.setMode(ModeRunning)
	q.Prompt = workspace.PrintableText(q.Prompt)
	choices := make([]workspace.Choice, len(q.Choices))
	for i, ch := range q.Choices {
		choices[i] = workspace.Choice{
			Label:       workspace.PrintableText(ch.Label),
			Description: workspace.PrintableText(ch.Description),
		}
	}
	q.Choices = choices
	return c.ui.Ask(ctx, q)
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
	switch c.mode {
	case ModeRunning:
		if c.cancel != nil {
			c.cancel()
		}
		return false
	case ModeApproval:
		// Quitting during a prompt cancels the run. Dashi records the turn
		// as interrupted, and the call had not started.
		c.quit = true
		if c.cancel != nil {
			c.cancel()
		}
		return false
	}
	return true
}

// Input handles one submitted line.
func (c *Controller) Input(line string) {
	line = strings.TrimSpace(line)
	switch c.Mode() {
	case ModeApproval:
		select {
		case c.answers <- line:
		default:
		}
		return
	case ModeCommand:
		return
	case ModeRunning:
		c.steer(line)
		return
	}
	c.mu.Lock()
	if name, _, _ := strings.Cut(line, " "); name != c.dropping {
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
	if line == "/quit" || line == "/exit" {
		c.QuitDuringRun(line)
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
		c.emit(warnBlock("The steer was not sent: %v.", err))
	}
}

// QuitDuringRun handles an exit action (/quit, /exit, or Ctrl-D) while a
// turn runs: it cancels the run, and the program quits once the run
// settles. When prompts are then still queued, it warns instead, as the
// same action at the prompt would, and repeating the action quits.
func (c *Controller) QuitDuringRun(action string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.quit, c.quitAction = true, action
	if c.cancel != nil {
		c.cancel()
	}
}

// duringTurnCommand handles a line that names a slash command while a turn
// runs. The line is not a steer. /help and /queue run now; the others wait
// for the end of the turn. It reports whether the line was a command.
func (c *Controller) duringTurnCommand(line string) bool {
	name, arg, _ := strings.Cut(line, " ")
	switch name {
	case "/help":
		c.command(line)
	case "/queue":
		c.queue(arg)
	case "/continue", "/new", "/model", "/sessions", "/done", "/discard":
		c.notice("%s runs when the turn ends. Press Ctrl-C to stop the turn.", name)
	default:
		return false
	}
	return true
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
	if !c.eng.Last(c.base).CanGoOn() {
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
	c.wg.Go(func() {
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
			c.emit(warnBlock("%s", busyText(err)))
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
	}
	return err.Error()
}

func (c *Controller) command(line string) {
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch name {
	case "/quit", "/exit":
		if c.keepQueued(name) {
			return
		}
		c.ui.Quit()
	case "/continue":
		if !c.eng.Last(c.base).LimitStopped() {
			c.emit(Block{
				Kind: KindNotice,
				Text: "/continue works after a model-call limit stop.",
				Done: true,
			})
			return
		}
		c.startRun(c.eng.ContinueLimit)
	case "/done":
		if id, _ := c.eng.Last(c.base).OpenCall(); id == "" {
			c.notice("/done marks a command done after dashi stopped while it ran.")
			return
		}
		c.startRun(c.eng.MarkDone)
	case "/discard":
		if !c.eng.Last(c.base).Open() {
			c.notice("There is no open turn to discard.")
			return
		}
		c.startRun(c.eng.Discard)
	case "/queue":
		c.prompt(arg)
	case "/new":
		if c.keepQueued(name) {
			return
		}
		c.async(c.newSession)
	case "/model":
		c.async(func(ctx context.Context) { c.model(ctx, arg) })
	case "/sessions":
		if c.keepQueued(name) {
			return
		}
		c.async(c.sessions)
	case "/help":
		c.emit(Block{Kind: KindNotice, Text: helpText, Done: true})
	default:
		c.emit(warnBlock("Unknown command %s. Type /help.", name))
	}
}

const helpText = `Commands:
  /model [name]  set the model, or pick one
  /new           start a new session
  /sessions      list sessions for this directory
  /continue      go on after a model-call limit stop
  /done          mark a command done that ran when dashi stopped
  /discard       discard the open turn
  /quit          save and exit
While a turn runs, a line is a steer. Start it with ! to cut the model call.
/queue <text> holds text as the next prompt, to run when the turn ends.`

// async runs fn in the command mode.
func (c *Controller) async(fn func(context.Context)) {
	c.setMode(ModeCommand)
	c.wg.Go(func() {
		defer c.setMode(ModeIdle)
		fn(c.base)
	})
}

// keepQueued warns, the first time, that action would drop the prompts
// queued in this session, and reports true so the caller stops. Repeating
// the action goes ahead.
func (c *Controller) keepQueued(action string) bool {
	n := c.eng.Queued()
	c.mu.Lock()
	warned := c.dropping == action
	c.dropping = ""
	if n > 0 && !warned {
		c.dropping = action
	}
	c.mu.Unlock()
	if n == 0 || warned {
		return false
	}
	c.emit(warnBlock("%d queued %s will be lost. Repeat %s to go ahead.",
		n, plural(n, "message"), action))
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

func (c *Controller) sessions(ctx context.Context) {
	list, err := c.store.List(c.dir)
	if err != nil {
		c.emit(warnBlock("Sessions cannot be listed: %v.", err))
		return
	}
	if len(list) == 0 {
		c.notice("There are no sessions for this directory.")
		return
	}
	labels := make([]string, len(list))
	for i, s := range list {
		labels[i] = fmt.Sprintf("%s  %s  %s",
			s.Updated.Local().Format("Jan 2 15:04"), s.ID,
			orDefault(s.Title, "(no prompt)"))
	}
	idx, ok := c.choose(ctx, "Sessions", labels)
	if !ok {
		return
	}
	c.openSession(list[idx].ID)
}

// OpenSession switches to the session with the given ID and shows it.
func (c *Controller) openSession(id string) {
	s, err := c.store.Load(id)
	if err != nil {
		c.emit(warnBlock("%v.", err))
		return
	}
	if err := c.eng.Open(s); err != nil {
		c.emit(warnBlock("%v.", err))
		return
	}
	if s.Model != "" {
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
