package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/deepnoodle-ai/wonton/runewidth"
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
	startMCP func()
	ctrl     *Controller
	eng      *Engine
	model    func() string
	dir      string

	// send delivers an event to the loop. SetRuntime sets it; tests set it
	// to call HandleEvent directly.
	send func(tui.Event)

	// pending holds work queued from other goroutines for the loop.
	pendMu    sync.Mutex
	pending   []queued
	scheduled bool
	promptSeq atomic.Uint64

	// loop state
	tr                    Transcript
	vp                    tui.ViewportState
	input                 string
	attachments           attachmentDraft
	attachmentInput       *submittedInput
	attachmentSelectionMu sync.Mutex
	attachmentContext     context.Context
	attachmentCancel      context.CancelFunc
	attachmentUnsaved     atomic.Bool
	historyStore          *inputHistory
	pathCompletion        *workspaceCompletion
	commandMenu           commandMenu
	exitGuard             idleExitGuard
	handoff               func(func() error) (error, error)
	history               []string
	frame                 uint64
	approval              *approvalPrompt
	asking                *askPrompt
	dialogStatus          *dialogStatusInput
	picker                *pickerState
	quitting              bool
	width                 int
	height                int
	promptID              uint64
	promptActive          bool
	savedDraft            string
	expanded              map[int]bool
	runtime               *tui.Runtime
	mouseEnabled          bool
	generation            uint64
	nativeScrollback      func(bool)
	// noColor makes text printed to native scrollback plain, as NO_COLOR asks.
	noColor           bool
	clipboardOut      io.Writer
	clipboardWrite    func(context.Context, string) (clipboardWriteResult, error)
	selectionRevision uint64
}

// queued is one item of pending work: a block to show, or a reset.
type queued struct {
	block Block
	reset bool
}

type approvalPrompt struct {
	req         toolApproval
	reply       chan Approval
	vp          tui.ViewportState
	denying     bool
	reason      string
	expanded    bool
	laidOut     *approvalLayout
	layoutWidth int
	// layoutRemember is offersRemember when the layout was built.
	layoutRemember bool
}

type askPrompt struct {
	q        Question
	task     string
	reply    chan questionAnswer
	vp       tui.ViewportState
	index    int
	selected map[int]bool
	own      bool
	text     string
	err      string
}

type pickerState struct {
	startupReview detailLines
	startupVP     tui.ViewportState
	sessionCards  bool
	sessionVP     tui.ViewportState
	title         string
	options       []string
	details       []string
	index         int
	reply         chan int
}

// Events posted to the loop.
type (
	flushEvent  struct{ at time.Time }
	quitEvent   struct{ at time.Time }
	promptEvent struct {
		at       time.Time
		id       uint64
		approval *approvalPrompt
		asking   *askPrompt
		picker   *pickerState
	}
	// endPromptEvent removes a prompt that ended without an answer.
	endPromptEvent struct {
		at time.Time
		id uint64
	}
)

func (e flushEvent) Timestamp() time.Time     { return e.at }
func (e quitEvent) Timestamp() time.Time      { return e.at }
func (e promptEvent) Timestamp() time.Time    { return e.at }
func (e endPromptEvent) Timestamp() time.Time { return e.at }

func newScreen(dir string) *screen {
	s := &screen{dir: dir, mouseEnabled: true, clipboardWrite: writeClipboard}
	s.vp.Follow = true
	return s
}

// bind connects the screen to the app built around it.
func (s *screen) bind(a *app) {
	s.ctrl, s.eng, s.model = a.ctrl, a.eng, a.eng.Model
	s.startMCP = func() { a.startMCP(true) }
}

func isCtrlC(e tui.KeyEvent) bool {
	return e.Key == tui.KeyCtrlC || e.Ctrl && (e.Rune == 'c' || e.Rune == 'C')
}

// SetRuntime implements tui.RuntimeAware.
func (s *screen) SetRuntime(rt *tui.Runtime) {
	s.runtime, s.clipboardOut = rt, rt.Terminal()
	s.handoff = rt.Handoff
	s.pendMu.Lock()
	s.send = rt.SendEvent
	s.pendMu.Unlock()
}

// Init implements tui.Initializable. It shows what was queued before the
// runtime started, such as the scrollback of a reopened session.
func (s *screen) Init() error {
	s.drain()
	if s.startMCP != nil {
		s.startMCP()
	}
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
func (*screen) retainSourceBlocks() {}

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
func (s *screen) Approve(ctx context.Context, r toolApproval) (Approval, error) {
	id := s.promptSeq.Add(1)
	p := &approvalPrompt{req: r, reply: make(chan Approval, 1)}
	s.post(promptEvent{at: time.Now(), id: id, approval: p})
	select {
	case a := <-p.reply:
		return a, nil
	case <-ctx.Done():
		s.post(endPromptEvent{at: time.Now(), id: id})
		return Approval{}, ctx.Err()
	}
}

// Ask implements UI.
func (s *screen) Ask(ctx context.Context, q taskQuestion) (questionAnswer, error) {
	id := s.promptSeq.Add(1)
	p := &askPrompt{q: q.Question, task: q.Task, reply: make(chan questionAnswer, 1), selected: map[int]bool{}, own: len(q.Choices) == 0}
	s.post(promptEvent{at: time.Now(), id: id, asking: p})
	select {
	case a := <-p.reply:
		return a, nil
	case <-ctx.Done():
		s.post(endPromptEvent{at: time.Now(), id: id})
		return questionAnswer{}, ctx.Err()
	}
}

// Choose implements Chooser.
func (s *screen) Choose(ctx context.Context, title string, options []string) (int, bool) {
	return s.ChooseDetailed(ctx, title, options, nil, 0)
}

// ChooseDetailed keeps model facts readable separately from the option list.
func (s *screen) ChooseDetailed(ctx context.Context, title string, options, details []string, selected int) (int, bool) {
	return s.choosePicker(ctx, title, options, details, selected, false)
}

func (s *screen) ChooseSessions(ctx context.Context, options, details []string) (int, bool) {
	return s.choosePicker(ctx, "Workspace sessions", options, details, 0, true)
}

func (s *screen) choosePicker(ctx context.Context, title string, options, details []string, selected int, sessions bool) (int, bool) {
	id := s.promptSeq.Add(1)
	p := &pickerState{title: title, options: options, details: details, sessionCards: sessions, index: max(0, min(selected, len(options)-1)), reply: make(chan int, 1)}
	s.post(promptEvent{at: time.Now(), id: id, picker: p})
	select {
	case i := <-p.reply:
		return i, i >= 0
	case <-ctx.Done():
		s.post(endPromptEvent{at: time.Now(), id: id})
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
	b := s.liveBlock(i)
	if b.Kind == KindFooter {
		return footerView(b, s.width)
	}
	return readingView(b.Kind, blockView(b))
}

// liveBlock is block i as the live screen draws it. A tool line shows its
// whole output when the user expanded it, offers the expansion when there
// is more to see and room on its result line, and spins while it runs.
func (s *screen) liveBlock(i int) Block {
	b := s.tr.Blocks[i]
	if b.Kind != KindTool {
		return b
	}
	more := toolHasMore(b)
	b.Expanded = more && s.expanded[i]
	if more && b.Summary != "" {
		room := min(readingWidth, s.width-2) - 2 - runewidth.StringWidth(toolResultMarker)
		b.ExpandHint = s.width <= 0 || runewidth.StringWidth(b.Summary+toolExpandHint) <= room
	}
	if toolSpins(b.State) {
		b.Spinner = s.spinnerFrame()
	}
	return b
}

// spinTools redraws the tool lines that show the spinner. Only the lines
// after the last turn footer can still run.
func (s *screen) spinTools() {
	for i := len(s.tr.Blocks) - 1; i >= 0; i-- {
		b := s.tr.Blocks[i]
		if b.Kind == KindFooter {
			return
		}
		if b.Kind == KindTool && toolSpins(b.State) {
			s.vp.Invalidate(i)
		}
	}
}

// Source implements tui.ViewportSourceItems: it is the text a selection
// copies. Glyphs, labels, rules, and states are decorations. Every
// non-empty source must match text its view marks with SourceOffset;
// otherwise Wonton cannot map the item, treats it as decoration, and a
// selection skips its text.
func (s *screen) Source(i int) (string, bool) {
	if i < 0 || i >= len(s.tr.Blocks) {
		return "", true
	}
	b := s.liveBlock(i)
	switch b.Kind {
	case KindTool:
		head := toolHeadSource(b)
		if body := toolBody(b); body != "" {
			return head + "\n" + body, true
		}
		return head, true
	case KindDiff:
		return strings.Join(b.Lines, "\n"), true
	case KindFooter:
		return footerSource(b, s.width), true
	case KindPending:
		if b.Done {
			return "", true
		}
	}
	return b.Text, true
}

func (s *screen) drain() {
	s.pendMu.Lock()
	items := s.pending
	s.pending = nil
	s.scheduled = false
	s.pendMu.Unlock()
	for _, q := range items {
		if q.reset {
			s.endDialogStatus()
			s.tr = Transcript{}
			s.attachments.clear()
			s.attachmentInput = nil
			s.attachmentUnsaved.Store(false)
			s.input = ""
			s.generation++
			s.expanded = nil
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
	case attachmentEvent:
		s.attachmentEvent(e)
	case tui.KeyEvent:
		return s.key(e)
	case tui.ResizeEvent:
		s.width, s.height = e.Width, e.Height
	case tui.TickEvent:
		before := s.spinnerFrame()
		s.frame = e.Frame
		if s.spinnerFrame() != before {
			s.spinTools()
		}
		if s.vp.DragAutoScroll() {
			s.selectionRevision++
		}
	case tui.MouseEvent:
		s.exitGuard.reset()
		s.handleTranscriptMouse(e)
	case focusEvent:
		if e.prompt != s.promptID {
			return nil
		}
		if e.id == dialogStatusInputID && s.dialogStatus == nil {
			return nil
		}
		return []tui.Cmd{tui.Focus(e.id)}
	case inputActionEvent:
		if e.prompt == 0 && s.promptActive || e.prompt != 0 && (!s.promptActive || e.prompt != s.promptID) {
			return nil
		}
		return s.key(e.key)
	case dialogStatusKeyEvent:
		if s.dialogStatusActive(e.input) {
			return s.dialogStatusKey(e.key)
		}
	case transcriptCommandEvent:
		s.transcriptCommand(e.name, e.arg)
	case clipboardResultEvent:
		s.clipboardResult(e)
	case flushEvent:
		s.drain()
	case quitEvent:
		s.drain()
		s.cancelAttachmentSelections()
		s.quitting = true
		return []tui.Cmd{tui.Quit()}
	case promptEvent:
		s.drain()
		if e.id != 0 && e.id <= s.promptID {
			return nil
		}
		if !s.promptActive {
			s.savedDraft = s.input
		}
		s.promptActive, s.promptID, s.input = true, e.id, ""
		s.endDialogStatus()
		s.approval, s.asking, s.picker = e.approval, e.asking, e.picker
		return []tui.Cmd{tui.Focus(s.focusID())}
	case endPromptEvent:
		if e.id != s.promptID {
			return nil
		}
		s.clearPrompts()
		return []tui.Cmd{tui.Focus(inputID)}
	}
	return nil
}

const inputID = "main-input"

func (s *screen) clearPrompts() {
	s.endDialogStatus()
	s.approval, s.asking, s.picker = nil, nil, nil
	if s.promptActive {
		s.input, s.savedDraft, s.promptActive = s.savedDraft, "", false
	}
}

func (s *screen) key(e tui.KeyEvent) []tui.Cmd {
	if !isCtrlC(e) {
		s.exitGuard.reset()
	}
	if isCtrlC(e) {
		if s.mainComposerActive() && s.copySelection() {
			s.exitGuard.reset()
			return nil
		}
		return s.interrupt()
	}
	if s.dialogStatus != nil {
		return s.dialogStatusKey(e)
	}
	if (s.approval != nil && !s.approval.denying || s.asking != nil && !s.asking.own) &&
		(e.Rune == '/' || strings.HasPrefix(strings.TrimSpace(e.Paste), "/")) {
		value := "/"
		if e.Paste != "" {
			value = e.Paste
		}
		s.beginDialogStatus(value)
		return []tui.Cmd{tui.Focus(dialogStatusInputID)}
	}
	switch {
	case s.approval != nil:
		return s.approvalKey(e)
	case s.picker != nil:
		return s.pickerKey(e)
	case s.asking != nil:
		return s.questionKey(e)
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
	case tui.KeyEscape:
		if s.ctrl.Mode() == ModeRunning || s.ctrl.Mode() == ModeApproval {
			s.ctrl.CancelWork()
		} else if s.vp.HasSelection() {
			s.vp.ClearSelection()
		} else {
			s.vp.ScrollToBottom()
		}
	case tui.KeyF2:
		if s.mainComposerActive() {
			s.toggleMouse()
		}
	case tui.KeyCtrlG:
		return s.editDraft()
	case tui.KeyCtrlL:
		if s.runtime != nil {
			s.runtime.Terminal().Invalidate()
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
		if s.picker.startupReview != nil {
			s.ctrl.Interrupt()
		}
		s.picker.reply <- -1
		s.clearPrompts()
		return []tui.Cmd{tui.Focus(inputID)}
	}
	if !s.promptActive && s.ctrl.Mode() == ModeIdle && s.protectIdleExit() {
		return nil
	}
	s.exitGuard.reset()
	if s.ctrl.Interrupt() {
		s.cancelAttachmentSelections()
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
		s.cancelAttachmentSelections()
		s.quitting = true
		return []tui.Cmd{tui.Quit()}
	case ModeRunning:
		s.ctrl.QuitDuringRun("Ctrl-D")
	case ModeApproval:
		s.ctrl.QuitDuringRun("Ctrl-D")
	}
	return nil
}

func (s *screen) pickerKey(e tui.KeyEvent) []tui.Cmd {
	p := s.picker
	if p.sessionCards {
		switch e.Key {
		case tui.KeyPageUp, tui.KeyPageDown, tui.KeyHome, tui.KeyEnd:
			scrollDialog(&p.sessionVP, e)
			return nil
		}
		p.sessionVP.ScrollToTop()
	}
	if p.startupReview != nil {
		switch e.Key {
		case tui.KeyPageUp:
			p.startupVP.PageUp()
			return nil
		case tui.KeyPageDown:
			p.startupVP.PageDown()
			return nil
		case tui.KeyHome:
			p.startupVP.ScrollToTop()
			return nil
		case tui.KeyEnd:
			p.startupVP.ScrollToBottom()
			return nil
		}
	}
	switch e.Key {
	case tui.KeyArrowUp:
		p.index = max(p.index-1, 0)
	case tui.KeyArrowDown:
		p.index = min(p.index+1, len(p.options)-1)
	case tui.KeyHome:
		p.index = 0
	case tui.KeyEnd:
		p.index = max(0, len(p.options)-1)
	case tui.KeyPageUp:
		p.index = max(0, p.index-5)
	case tui.KeyPageDown:
		p.index = min(len(p.options)-1, p.index+5)
	case tui.KeyEnter:
		s.clearPrompts()
		p.reply <- p.index
		return []tui.Cmd{tui.Focus(inputID)}
	case tui.KeyEscape:
		s.clearPrompts()
		p.reply <- -1
		return []tui.Cmd{tui.Focus(inputID)}
	}
	return nil
}

func (s *screen) submit(value string) {
	raw := value
	value = strings.TrimSpace(value)
	if s.promptActive && (s.asking != nil || s.approval != nil) && s.inspectDialogSubmission(value) {
		return
	}
	if s.asking == nil && !s.eng.cfg.MCP.Ready() {
		name, _ := splitCommand(value)
		if name != "/status" && name != "/mcp" && name != "/quit" && name != "/exit" && name != "/q" {
			s.ctrl.mcpInput(value)
			return
		}
	}
	if s.asking == nil && !strings.HasPrefix(value, "/") && s.AttachmentInput(raw) {
		return
	}
	if s.asking == nil && strings.HasPrefix(value, "/") {
		name, arg := splitCommand(value)
		if name == "/attach" || name == "/detach" || name == "/attachments" {
			s.attachmentCommand(name, arg)
			s.input = s.attachments.text
			return
		}
		if len(s.attachments.entries) > 0 {
			s.ctrl.Input(value)
			s.input = s.attachments.text
			return
		}
	}
	s.input = ""
	if s.asking != nil {
		p := s.asking
		a, err := parseQuestionAnswer(p.q, value)
		if err != nil {
			s.Emit(warnBlock("%v", err))
			return
		}
		s.clearPrompts()
		p.reply <- a
		return
	}
	s.recordSubmittedInput(raw)
	if !strings.HasPrefix(value, "/") {
		s.attachments.clear()
		s.attachmentInput = nil
		s.attachmentUnsaved.Store(false)
	}
	s.vp.ScrollToBottom()
	s.ctrl.Input(value)
}

// A retained field callback must not submit into a replacement dialog.
func (s *screen) inputSubmit(id uint64) func(string) {
	return func(value string) {
		if !s.promptActive && s.promptID == id {
			// Enter in the open command menu runs the selected command.
			if name := s.commandMenu.submit; name != "" {
				value = name
			}
			s.commandMenu.submit = ""
			s.submit(value)
		}
	}
}

// dumpTranscript writes the plain text of the whole transcript. The alternate
// screen leaves no scrollback, so the program prints this on exit.
func (s *screen) dumpTranscript(w io.Writer) {
	for _, b := range s.tr.Blocks {
		fmt.Fprintln(w, FormatBlock(printable(fullBlock(b))))
	}
}

func (s *screen) ChooseTaskDetails(ctx context.Context, id, text string) {
	s.choosePicker(ctx, "Task "+id, []string{"Close details"}, []string{text}, 0, true)
}
