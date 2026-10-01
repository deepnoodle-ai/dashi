package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/deepnoodle-ai/dashi/workspace"
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
	clipboardOut          io.Writer
	clipboardWrite        func(context.Context, string) (string, error)
}

// queued is one item of pending work: a block to show, or a reset.
type queued struct {
	block Block
	reset bool
}

type approvalPrompt struct {
	req     ApprovalRequest
	reply   chan Approval
	vp      tui.ViewportState
	denying bool
	reason  string
}

type askPrompt struct {
	q        Question
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
func (s *screen) Approve(ctx context.Context, r ApprovalRequest) (Approval, error) {
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
func (s *screen) Ask(ctx context.Context, q Question) (questionAnswer, error) {
	id := s.promptSeq.Add(1)
	p := &askPrompt{q: q, reply: make(chan questionAnswer, 1), selected: map[int]bool{}, own: len(q.Choices) == 0}
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
	b := s.tr.Blocks[i]
	if b.Kind == KindTool && b.Detail != "" {
		if s.expanded[i] {
			b.Expanded = true
		} else {
			return tui.Stack(blockView(b), tui.PaddingLTRB(6, 0, 0, 0,
				tui.Text("%d output lines · click to expand", strings.Count(b.Detail, "\n")+1).Style(hintStyle))).Gap(0)
		}
	}
	return blockView(b)
}

// Source implements tui.ViewportSourceItems. Labels and controls are decorations.
func (s *screen) Source(i int) (string, bool) {
	if i < 0 || i >= len(s.tr.Blocks) {
		return "", true
	}
	b := s.tr.Blocks[i]
	switch b.Kind {
	case KindTool:
		if s.expanded[i] && b.Detail != "" {
			return b.Detail, true
		}
		return strings.Join(b.Lines, "\n"), true
	case KindDiff:
		return strings.Join(b.Lines, "\n"), true
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
		s.frame = e.Frame
		s.vp.DragAutoScroll()
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
			s.submit(value)
		}
	}
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
	if !s.vp.AtBottom && s.vp.LinesBelow > 0 {
		views = append(views, tui.Text(" %d lines below · Ctrl+End returns to live output", s.vp.LinesBelow).Style(hintStyle))
	}
	switch {
	case s.dialogStatus != nil:
		views = append(views, s.dialogStatusView())
	case s.approval != nil:
		views = append(views, s.approvalView())
	case s.picker != nil:
		views = append(views, pickerView(s.picker))
	case s.asking != nil:
		views = append(views, s.questionView())
	default:
		mode := s.ctrl.Mode()
		if mode == ModeRunning {
			views = append(views, tui.Text(""), s.workingView(), tui.Text(""))
		}
		for _, row := range attachmentPreviewRows(&s.attachments, s.ctrl.attachmentRoot) {
			views = append(views, tui.Text(" %s", row).Style(mutedStyle))
		}
		views = append(views,
			tui.Text(" %s", s.inputHint(mode)).Wrap().Style(hintStyle),
			tui.Divider(), s.inputView(mode), tui.Divider())
	}
	views = append(views, s.statusView())
	return tui.Stack(views...).Gap(0)
}

func (s *screen) inputView(mode Mode) tui.View {
	maxHeight := 8
	if len(s.attachments.entries) > 0 {
		// Inline output uses eight rows. Keep composer and status visible on
		// an 80×24 terminal, including the running-work indicator.
		maxHeight = 6
	}
	// Wonton v0.2.1 retains a field's initial placeholder. Keep it neutral;
	// the ordinary text view above it updates the current input meaning.
	return tui.InputField(&s.input).
		ID(inputID).
		Prompt("❯").
		PromptStyle(tui.NewStyle().WithForeground(tui.ColorCyan).WithBold()).
		Placeholder("Type a message...").
		History(s.history).
		Multiline(true).
		MaxHeight(maxHeight).
		OnKey(s.inputKey).
		OnPaste(s.inputPaste(s.promptID)).
		OnChange(func(value string) {
			s.exitGuard.reset()
			if s.promptActive {
				return
			}
			if s.attachments.frozenBy != "" {
				s.input = s.attachments.text
				return
			}
			if !strings.HasPrefix(value, "/") {
				s.attachments.setText(value)
			}
			s.updateAttachmentUnsaved()
		}).
		OnComplete(s.completeDraft).
		OnSubmit(s.inputSubmit(s.promptID))
}

func (s *screen) inputHint(mode Mode) string {
	switch {
	case s.asking != nil:
		return "Type your answer."
	case mode == ModeCommand && !s.eng.cfg.MCP.Ready():
		return "Starting MCP servers. Submit input after startup finishes; Ctrl-C cancels startup."
	case mode == ModeRunning:
		for i := len(s.tr.Blocks) - 1; i >= 0; i-- {
			b := s.tr.Blocks[i]
			if b.Kind == KindUser {
				break
			}
			if b.Kind == KindCompaction {
				if b.State == StateRunning {
					return "Compacting: new text is queued. Ctrl-C interrupts."
				}
				break
			}
		}
		return "Type to steer. Start with ! to cut the model call."
	case s.eng.HasPending():
		return "Press Enter to continue and deliver the pending prompt."
	}
	return "Ask anything. /help lists commands."
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (s *screen) workingView() tui.View {
	frame := spinnerFrames[int(s.frame/3)%len(spinnerFrames)]
	return tui.Group(
		tui.Text(" %s", frame).Fg(tui.ColorCyan),
		tui.Text(" Working. Ctrl-C interrupts.").Style(hintStyle),
	)
}

func (s *screen) statusView() tui.View {
	id := ""
	if sess := s.eng.Session(); sess != nil {
		id = sess.ID
	}
	return tui.Text(" %s · %s · %s", s.model(), shortDir(s.dir), id).Style(mutedStyle)
}

// shortDir shows the last two parts of a path.
func shortDir(dir string) string {
	parent, base := filepath.Split(filepath.Clean(dir))
	return filepath.Join(filepath.Base(parent), base)
}

func pickerView(p *pickerState) tui.View {
	if p.startupReview != nil {
		return mcpStartupView(p)
	}
	if p.sessionCards {
		return sessionPickerView(p)
	}
	views := []tui.View{tui.Divider(), tui.Text(" %s", p.title).Bold()}
	// Keep room for the selected model's facts on an 80×24 terminal.
	window := 10
	if len(p.details) > 0 {
		window = 5
	}
	start := max(0, min(p.index-window/2, len(p.options)-window))
	end := min(len(p.options), start+window)
	for i := start; i < end; i++ {
		if i == p.index {
			views = append(views, tui.Text(" ❯ %s", p.options[i]).Fg(tui.ColorCyan).Bold())
		} else {
			views = append(views, tui.Text("   %s", p.options[i]))
		}
	}
	if p.index >= 0 && p.index < len(p.details) {
		views = append(views, tui.Divider(), tui.Text("%s\n%s", p.options[p.index], p.details[p.index]).Wrap())
	}
	views = append(views, tui.Text(" Up and Down move. Enter picks. Esc cancels.").Style(hintStyle))
	return tui.Stack(views...).Gap(0)
}

// blockView draws one transcript block.
func blockView(b Block) tui.View {
	switch b.Kind {
	case KindUser:
		return tui.Group(
			tui.Text("❯ ").Fg(tui.ColorCyan).Bold(),
			tui.Text("%s", b.Text).SourceOffset(0).Wrap().Bold().Flex(1),
		)
	case KindAssistant:
		text := b.Text
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return tui.ZStack(
			tui.PaddingLTRB(2, 0, 0, 0, tui.Markdown(text, nil).SourceOffset(0)),
			tui.Text("⏺").Fg(tui.ColorCyan),
		).Align(tui.AlignLeft)
	case KindReasoning:
		if strings.TrimSpace(b.Text) == "" {
			return nil
		}
		return tui.Stack(tui.Text("  Reasoning").Fg(tui.ColorCyan), tui.PaddingLTRB(2, 0, 0, 0, tui.Markdown(b.Text, nil).SourceOffset(0))).Gap(0)
	case KindTool:
		return toolView(b)
	case KindDiff:
		views := make([]tui.View, 0, len(b.Lines))
		offset := 0
		for _, l := range b.Lines {
			views = append(views, tui.PaddingLTRB(2, 0, 0, 0, diffSourceLineView(l, offset)))
			offset += len(l) + 1
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
		return sourceLabel("  ↳ "+label+": ", b.Text, tui.ColorYellow)
	case KindCompaction:
		color := tui.ColorCyan
		if b.State == StateFailed {
			color = tui.ColorRed
		} else if b.State == "interrupted" {
			color = tui.ColorYellow
		}
		return sourceLabel("  ", b.Text, color)
	case KindPending:
		if b.Done {
			return nil
		}
		return sourceLabel("  ↳ pending prompt: ", b.Text, tui.ColorYellow)
	case KindStop:
		return sourceLabel("■ ", b.Text, tui.ColorYellow)
	case KindFooter:
		return tui.Text("%s", b.Text).SourceOffset(0).Style(mutedStyle)
	case KindWarning:
		return sourceLabel("! ", b.Text, tui.ColorYellow)
	}
	return tui.Text("%s", b.Text).SourceOffset(0).Wrap().Style(mutedStyle)
}

func toolView(b Block) tui.View {
	var icon *tui.TextView
	var state *tui.TextView
	switch b.State {
	case StateOK:
		icon = tui.Text("  ✓").Fg(tui.ColorGreen)
		state = tui.Text("ok  %.1fs", b.Elapsed.Seconds()).Style(mutedStyle)
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
		state = tui.Text("running").Style(mutedStyle)
	}
	head := tui.Group(
		icon,
		tui.Text(" %s", workspace.PrintableText(b.Name)).Bold(),
		tui.Text("  %s", fitArg(workspace.PrintableText(b.Arg), 70)).Style(mutedStyle),
		tui.Spacer(),
		state,
	)
	if b.Expanded && b.Detail != "" {
		return tui.Stack(head, tui.PaddingLTRB(6, 0, 0, 0,
			tui.Text("%s", b.Detail).SourceOffset(0).Wrap().Style(mutedStyle))).Gap(0)
	}
	if len(b.Lines) == 0 {
		return head
	}
	views := []tui.View{head}
	offset := 0
	for _, l := range b.Lines {
		views = append(views, tui.PaddingLTRB(6, 0, 0, 0,
			tui.Text("%s", l).SourceOffset(offset).Wrap().Style(mutedStyle)))
		offset += len(l) + 1
	}
	return tui.Stack(views...).Gap(0)
}

// Secondary text uses 256-color gray 245 (#8a8a8a) without dim. Wonton's
// Hint and Muted add dim to bright black, which nearly disappears on dark
// themes. Italic marks only key and command help.
var (
	mutedStyle = tui.NewStyle().WithForeground(tui.Color(245))
	hintStyle  = mutedStyle.WithItalic()
)

func sourceLabel(label, source string, color tui.Color) tui.View {
	return tui.Group(tui.Text("%s", label).Fg(color),
		tui.Text("%s", source).SourceOffset(0).Wrap().Fg(color).Flex(1))
}

func diffLineView(indent, line string) tui.View {
	return tui.PaddingLTRB(len(indent), 0, 0, 0, diffSourceLineView(line, 0))
}

func diffSourceLineView(line string, offset int) tui.View {
	t := tui.Text("%s", line).SourceOffset(offset).Wrap()
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
	return t.Style(mutedStyle)
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
