package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/wonton/tui"
)

// screen is the full-screen UI. It draws the transcript in a scrollable
// viewport, with the prompt and status line below. It implements UI and
// Chooser for the Controller.
//
// Only the Wonton event loop touches the fields marked "loop". Other
// goroutines call Emit, Approve, and the rest, which queue an event and, when
// they must wait for the user, block on a reply channel.
type screen struct {
	ctrl  *Controller
	eng   *Engine
	model func() string
	dir   string

	// send delivers an event to the loop. SetRuntime sets it; tests set it
	// to call HandleEvent directly.
	send func(tui.Event)

	// pending holds work queued from other goroutines for the loop.
	pendMu    sync.Mutex
	pending   []queued
	scheduled bool

	// loop state
	tr       Transcript
	vp       tui.ViewportState
	input    string
	history  []string
	frame    uint64
	approval *approvalPrompt
	asking   *askPrompt
	picker   *pickerState
	quitting bool
	width    int
	height   int
}

// queued is one item of pending work: a block to show, or a reset.
type queued struct {
	block Block
	reset bool
}

type approvalPrompt struct {
	req   ApprovalRequest
	reply chan Approval
}

type askPrompt struct {
	q     Question
	reply chan string
}

type pickerState struct {
	title   string
	options []string
	index   int
	reply   chan int
}

// Events posted to the loop.
type (
	flushEvent  struct{ at time.Time }
	quitEvent   struct{ at time.Time }
	promptEvent struct {
		at       time.Time
		approval *approvalPrompt
		asking   *askPrompt
		picker   *pickerState
	}
	// endPromptEvent removes a prompt that ended without an answer.
	endPromptEvent struct{ at time.Time }
)

func (e flushEvent) Timestamp() time.Time     { return e.at }
func (e quitEvent) Timestamp() time.Time      { return e.at }
func (e promptEvent) Timestamp() time.Time    { return e.at }
func (e endPromptEvent) Timestamp() time.Time { return e.at }

func newScreen(dir string) *screen {
	s := &screen{dir: dir}
	s.vp.Follow = true
	return s
}

// bind connects the screen to the app built around it.
func (s *screen) bind(a *app) {
	s.ctrl, s.eng, s.model = a.ctrl, a.eng, a.eng.Model
}

func isCtrlC(e tui.KeyEvent) bool {
	return e.Key == tui.KeyCtrlC || e.Ctrl && (e.Rune == 'c' || e.Rune == 'C')
}

// SetRuntime implements tui.RuntimeAware.
func (s *screen) SetRuntime(rt *tui.Runtime) {
	s.pendMu.Lock()
	s.send = rt.SendEvent
	s.pendMu.Unlock()
}

// Init implements tui.Initializable. It shows what was queued before the
// runtime started, such as the scrollback of a reopened session.
func (s *screen) Init() error {
	s.drain()
	return nil
}

// --- UI ---

func (s *screen) enqueue(q queued) {
	s.pendMu.Lock()
	s.pending = append(s.pending, q)
	send := s.send != nil && !s.scheduled
	if send {
		s.scheduled = true
	}
	deliver := s.send
	s.pendMu.Unlock()
	if send {
		deliver(flushEvent{at: time.Now()})
	}
}

// Emit implements UI.
func (s *screen) Emit(b Block) {
	s.enqueue(queued{block: b})
}

// Reset implements UI.
func (s *screen) Reset() {
	s.enqueue(queued{reset: true})
}

// Quit implements UI.
func (s *screen) Quit() {
	s.post(quitEvent{at: time.Now()})
}

func (s *screen) post(e tui.Event) {
	s.pendMu.Lock()
	deliver := s.send
	s.pendMu.Unlock()
	if deliver != nil {
		deliver(e)
	}
}

// Approve implements UI. It blocks until the user answers or ctx ends.
func (s *screen) Approve(ctx context.Context, r ApprovalRequest) (Approval, error) {
	p := &approvalPrompt{req: r, reply: make(chan Approval, 1)}
	s.post(promptEvent{at: time.Now(), approval: p})
	select {
	case a := <-p.reply:
		return a, nil
	case <-ctx.Done():
		s.post(endPromptEvent{at: time.Now()})
		return Approval{}, ctx.Err()
	}
}

// Ask implements UI.
func (s *screen) Ask(ctx context.Context, q Question) (string, error) {
	p := &askPrompt{q: q, reply: make(chan string, 1)}
	s.post(promptEvent{at: time.Now(), asking: p})
	select {
	case a := <-p.reply:
		return a, nil
	case <-ctx.Done():
		s.post(endPromptEvent{at: time.Now()})
		return "", ctx.Err()
	}
}

// Choose implements Chooser.
func (s *screen) Choose(ctx context.Context, title string, options []string) (int, bool) {
	p := &pickerState{title: title, options: options, reply: make(chan int, 1)}
	s.post(promptEvent{at: time.Now(), picker: p})
	select {
	case i := <-p.reply:
		return i, i >= 0
	case <-ctx.Done():
		s.post(endPromptEvent{at: time.Now()})
		return 0, false
	}
}

// --- tui.ViewportItems ---

// Len implements tui.ViewportItems.
func (s *screen) Len() int { return len(s.tr.Blocks) }

// Item implements tui.ViewportItems.
func (s *screen) Item(i int) tui.View {
	if i < 0 || i >= len(s.tr.Blocks) {
		return nil
	}
	return blockView(s.tr.Blocks[i])
}

func (s *screen) drain() {
	s.pendMu.Lock()
	items := s.pending
	s.pending = nil
	s.scheduled = false
	s.pendMu.Unlock()
	for _, q := range items {
		if q.reset {
			s.tr = Transcript{}
			s.vp.InvalidateAll()
			s.vp.Follow = true
			continue
		}
		i := s.tr.Apply(q.block)
		s.vp.Invalidate(i)
	}
}

// --- events ---

// HandleEvent implements tui.EventHandler.
func (s *screen) HandleEvent(event tui.Event) []tui.Cmd {
	switch e := event.(type) {
	case tui.KeyEvent:
		return s.key(e)
	case tui.ResizeEvent:
		s.width, s.height = e.Width, e.Height
	case tui.TickEvent:
		s.frame = e.Frame
	case flushEvent:
		s.drain()
	case quitEvent:
		s.drain()
		s.quitting = true
		return []tui.Cmd{tui.Quit()}
	case promptEvent:
		s.drain()
		s.approval, s.asking, s.picker = e.approval, e.asking, e.picker
	case endPromptEvent:
		s.clearPrompts()
		return []tui.Cmd{tui.Focus(inputID)}
	}
	return nil
}

const inputID = "main-input"

func (s *screen) clearPrompts() {
	s.approval, s.asking, s.picker = nil, nil, nil
}

func (s *screen) key(e tui.KeyEvent) []tui.Cmd {
	if isCtrlC(e) {
		return s.interrupt()
	}
	switch {
	case s.approval != nil:
		return s.approvalKey(e)
	case s.picker != nil:
		return s.pickerKey(e)
	}
	switch e.Key {
	case tui.KeyPageUp:
		s.vp.PageUp()
	case tui.KeyPageDown:
		s.vp.PageDown()
	case tui.KeyHome:
		if e.Ctrl || s.input == "" {
			s.vp.ScrollToTop()
		}
	case tui.KeyEnd:
		if e.Ctrl || s.input == "" {
			s.vp.ScrollToBottom()
		}
	}
	isD := e.Key == tui.KeyCtrlD || e.Ctrl && (e.Rune == 'd' || e.Rune == 'D')
	if isD && s.input == "" {
		return s.requestQuit()
	}
	return nil
}

// interrupt handles Ctrl-C by mode.
func (s *screen) interrupt() []tui.Cmd {
	if s.picker != nil {
		s.picker.reply <- -1
		s.picker = nil
		return []tui.Cmd{tui.Focus(inputID)}
	}
	if s.ctrl.Interrupt() {
		s.quitting = true
		return []tui.Cmd{tui.Quit()}
	}
	return nil
}

// requestQuit handles Ctrl-D and /quit typed while a turn runs.
func (s *screen) requestQuit() []tui.Cmd {
	switch s.ctrl.Mode() {
	case ModeIdle:
		if s.ctrl.keepQueued("Ctrl-D") {
			return nil
		}
		s.quitting = true
		return []tui.Cmd{tui.Quit()}
	case ModeRunning:
		s.ctrl.QuitDuringRun("Ctrl-D")
	case ModeApproval:
		s.ctrl.Interrupt()
	}
	return nil
}

func (s *screen) approvalKey(e tui.KeyEvent) []tui.Cmd {
	var a Approval
	switch {
	case e.Rune == 'y' || e.Rune == 'Y':
		a = Approval{Allow: true}
	case e.Rune == 'a' || e.Rune == 'A':
		a = Approval{Allow: true, Remember: true}
	case e.Rune == 'n' || e.Rune == 'N' || e.Key == tui.KeyEscape:
		a = Approval{Reason: declinedReason}
	default:
		return nil
	}
	p := s.approval
	s.approval = nil
	p.reply <- a
	return []tui.Cmd{tui.Focus(inputID)}
}

func (s *screen) pickerKey(e tui.KeyEvent) []tui.Cmd {
	p := s.picker
	switch e.Key {
	case tui.KeyArrowUp:
		p.index = max(p.index-1, 0)
	case tui.KeyArrowDown:
		p.index = min(p.index+1, len(p.options)-1)
	case tui.KeyEnter:
		s.picker = nil
		p.reply <- p.index
		return []tui.Cmd{tui.Focus(inputID)}
	case tui.KeyEscape:
		s.picker = nil
		p.reply <- -1
		return []tui.Cmd{tui.Focus(inputID)}
	}
	return nil
}

func (s *screen) submit(value string) {
	s.input = ""
	value = strings.TrimSpace(value)
	if s.asking != nil {
		p := s.asking
		s.asking = nil
		p.reply <- value
		return
	}
	if value != "" && (len(s.history) == 0 || s.history[len(s.history)-1] != value) {
		s.history = append(s.history, value)
	}
	s.vp.ScrollToBottom()
	s.ctrl.Input(value)
}

// --- views ---

// View implements tui.Application.
func (s *screen) View() tui.View {
	return tui.Stack(
		tui.PaddingLTRB(1, 0, 1, 0, tui.Viewport(&s.vp, s).Gap(1)),
		s.footer(),
	).Gap(0)
}

func (s *screen) footer() tui.View {
	views := make([]tui.View, 0, 10)
	switch {
	case s.approval != nil:
		views = append(views, approvalView(s.approval.req))
	case s.picker != nil:
		views = append(views, pickerView(s.picker))
	default:
		mode := s.ctrl.Mode()
		if mode == ModeRunning {
			views = append(views, tui.Text(""), s.workingView(), tui.Text(""))
		}
		if s.asking != nil {
			views = append(views, askView(s.asking.q))
		}
		views = append(views, tui.Divider(), s.inputView(mode), tui.Divider())
	}
	views = append(views, s.statusView())
	return tui.Stack(views...).Gap(0)
}

func (s *screen) inputView(mode Mode) tui.View {
	placeholder := "Ask anything. /help lists commands."
	switch {
	case s.asking != nil:
		placeholder = "Type your answer."
	case mode == ModeRunning:
		placeholder = "Type to steer. Start with ! to cut the model call."
	}
	return tui.InputField(&s.input).
		ID(inputID).
		Prompt("❯").
		PromptStyle(tui.NewStyle().WithForeground(tui.ColorCyan).WithBold()).
		Placeholder(placeholder).
		History(s.history).
		Multiline(true).
		MaxHeight(8).
		OnSubmit(s.submit)
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (s *screen) workingView() tui.View {
	frame := spinnerFrames[int(s.frame/3)%len(spinnerFrames)]
	return tui.Group(
		tui.Text(" %s", frame).Fg(tui.ColorCyan),
		tui.Text(" Working. Ctrl-C interrupts.").Hint(),
	)
}

func (s *screen) statusView() tui.View {
	id := ""
	if sess := s.eng.Session(); sess != nil {
		id = sess.ID
	}
	return tui.Text(" %s · %s · %s", s.model(), shortDir(s.dir), id).Hint()
}

// shortDir shows the last two parts of a path.
func shortDir(dir string) string {
	parent, base := filepath.Split(filepath.Clean(dir))
	return filepath.Join(filepath.Base(parent), base)
}

func askView(q Question) tui.View {
	views := []tui.View{tui.Text(" %s", q.Prompt).Bold()}
	for i, c := range q.Choices {
		views = append(views, tui.Text("   %d. %s", i+1, c.Label))
	}
	return tui.Stack(views...).Gap(0)
}

func approvalView(r ApprovalRequest) tui.View {
	views := []tui.View{
		tui.Divider(),
		tui.Text(" %s", r.Summary).Bold().Fg(tui.ColorYellow),
	}
	if r.Command != "" {
		views = append(views, tui.Text("   $ %s", r.Command).Wrap())
	}
	if r.Preview != "" {
		lines := strings.Split(strings.TrimRight(r.Preview, "\n"), "\n")
		if len(lines) > 30 {
			hidden := len(lines) - 30
			lines = append(lines[:30], fmt.Sprintf("... %d more lines", hidden))
		}
		for _, l := range lines {
			views = append(views, diffLineView("   ", l))
		}
	}
	views = append(views,
		tui.Text(""),
		tui.Group(
			tui.Text(" y").Bold().Fg(tui.ColorGreen),
			tui.Text(" allow once   ").Hint(),
			tui.Text("a").Bold().Fg(tui.ColorGreen),
			tui.Text(" always   ").Hint(),
			tui.Text("n").Bold().Fg(tui.ColorRed),
			tui.Text(" deny").Hint(),
		),
	)
	return tui.Stack(views...).Gap(0)
}

func pickerView(p *pickerState) tui.View {
	views := []tui.View{tui.Divider(), tui.Text(" %s", p.title).Bold()}
	// Show a window of ten options around the cursor.
	start := max(0, min(p.index-5, len(p.options)-10))
	end := min(len(p.options), start+10)
	for i := start; i < end; i++ {
		if i == p.index {
			views = append(views, tui.Text(" ❯ %s", p.options[i]).Fg(tui.ColorCyan).Bold())
		} else {
			views = append(views, tui.Text("   %s", p.options[i]))
		}
	}
	views = append(views, tui.Text(" Up and Down move. Enter picks. Esc cancels.").Hint())
	return tui.Stack(views...).Gap(0)
}

// blockView draws one transcript block.
func blockView(b Block) tui.View {
	switch b.Kind {
	case KindUser:
		return tui.Group(
			tui.Text("❯ ").Fg(tui.ColorCyan).Bold(),
			tui.Text("%s", b.Text).Wrap().Bold().Flex(1),
		)
	case KindAssistant:
		text := strings.TrimSpace(b.Text)
		if text == "" {
			return nil
		}
		return tui.ZStack(
			tui.PaddingLTRB(2, 0, 0, 0, tui.Markdown(text, nil)),
			tui.Text("⏺").Fg(tui.ColorCyan),
		).Align(tui.AlignLeft)
	case KindTool:
		return toolView(b)
	case KindDiff:
		views := make([]tui.View, 0, len(b.Lines))
		for _, l := range b.Lines {
			views = append(views, diffLineView("  ", l))
		}
		return tui.Stack(views...).Gap(0)
	case KindSteer:
		label := "steer queued"
		switch b.State {
		case SteerDelivered:
			label = "steer delivered"
		case SteerNext:
			label = "queued for the next turn"
		}
		return tui.Text("  ↳ %s: %s", label, b.Text).Wrap().Fg(tui.ColorYellow)
	case KindStop:
		return tui.Text("■ %s", b.Text).Wrap().Fg(tui.ColorYellow)
	case KindFooter:
		return tui.Text("%s", b.Text).Hint()
	case KindWarning:
		return tui.Text("! %s", b.Text).Wrap().Warning()
	}
	return tui.Text("%s", b.Text).Wrap().Hint()
}

func toolView(b Block) tui.View {
	var icon *tui.TextView
	var state *tui.TextView
	switch b.State {
	case StateOK:
		icon = tui.Text("  ✓").Fg(tui.ColorGreen)
		state = tui.Text("ok  %.1fs", b.Elapsed.Seconds()).Hint()
	case StateFailed:
		icon = tui.Text("  ✗").Fg(tui.ColorRed)
		state = tui.Text("failed  %.1fs", b.Elapsed.Seconds()).Fg(tui.ColorRed)
	case StateDenied:
		icon = tui.Text("  ✗").Fg(tui.ColorYellow)
		state = tui.Text("denied").Fg(tui.ColorYellow)
	case StateWaiting:
		icon = tui.Text("  …").Fg(tui.ColorYellow)
		state = tui.Text("waiting").Fg(tui.ColorYellow)
	default:
		icon = tui.Text("  ⠋").Fg(tui.ColorCyan)
		state = tui.Text("running").Hint()
	}
	head := tui.Group(
		icon,
		tui.Text(" %s", b.Name).Bold(),
		tui.Text("  %s", fitArg(b.Arg, 70)).Hint(),
		tui.Spacer(),
		state,
	)
	if len(b.Lines) == 0 {
		return head
	}
	views := []tui.View{head}
	for _, l := range b.Lines {
		views = append(views, tui.Text("      %s", l).Hint())
	}
	return tui.Stack(views...).Gap(0)
}

func diffLineView(indent, line string) tui.View {
	t := tui.Text("%s%s", indent, line)
	switch {
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		return t.Bold()
	case strings.HasPrefix(line, "+"):
		return t.Fg(tui.ColorGreen)
	case strings.HasPrefix(line, "-"):
		return t.Fg(tui.ColorRed)
	case strings.HasPrefix(line, "@@"):
		return t.Fg(tui.ColorCyan)
	}
	return t.Hint()
}

// dumpTranscript writes the plain text of the whole transcript. The alternate
// screen leaves no scrollback, so the program prints this on exit.
func (s *screen) dumpTranscript(w io.Writer) {
	for _, b := range s.tr.Blocks {
		fmt.Fprintln(w, FormatBlock(b))
	}
}
