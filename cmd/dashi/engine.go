package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"github.com/deepnoodle-ai/dashi/llm"
	"github.com/deepnoodle-ai/dashi/tool"
)

// defaultModelAttempts is the model-call limit of one turn. Coding reads many
// files, and Dashi's own default of 8 stops too early.
const defaultModelAttempts = 40

// EngineConfig holds what an Engine needs.
type EngineConfig struct {
	// Dir is the workspace directory, absolute.
	Dir    string
	Client llm.Client
	Model  string
	Store  *session.Store
	Wiring wiring
	// ModelAttempts is the per-turn model-call limit; zero means 40.
	ModelAttempts int
	// Emit receives every block the engine produces. It may run on any
	// goroutine.
	Emit func(Block)

	// recorder wraps the session's recorder; tests use it to fail commits.
	recorder func(*session.Recorder) dashi.Recorder
}

// Engine runs the turns of one session. It holds the session's metadata and
// a dashi.Session handle, which owns the turn numbers, the history, the
// queue, and the rules for a new prompt while a turn is open. The steering
// inbox is the handle's. The Engine has no screen: the Controller and the
// screen sit on top of it.
type Engine struct {
	cfg EngineConfig

	mu   sync.Mutex
	sess *session.Session
	// chat drives the open session. The process lock makes it the only
	// driver, so it is Exclusive.
	chat   *dashi.Session
	inbox  *dashi.Inbox
	unlock func()
	// model is the model the next Start uses. Changing it does not touch
	// turns already started.
	model string
}

// NewEngine returns an engine with no session. Call Open before any turn.
func NewEngine(cfg EngineConfig) *Engine {
	if cfg.ModelAttempts == 0 {
		cfg.ModelAttempts = defaultModelAttempts
	}
	if cfg.Emit == nil {
		cfg.Emit = func(Block) {}
	}
	return &Engine{cfg: cfg, model: cfg.Model}
}

// Session returns the open session's metadata.
func (e *Engine) Session() *session.Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sess
}

// Saved reports whether the open session has a turn and a session file.
func (e *Engine) Saved() bool {
	s := e.Session()
	return s != nil && e.cfg.Store.Saved(s)
}

// Model returns the model the next new turn uses.
func (e *Engine) Model() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.model
}

// SetModel sets the model for the next new turn.
func (e *Engine) SetModel(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.model = name
	if e.sess != nil && e.cfg.Store.Saved(e.sess) {
		e.sess.Model = name
		_ = e.cfg.Store.Save(e.sess)
	}
}

// Open makes s the open session and locks it. It returns session.ErrBusy when
// another process holds s. Opening the session that is already open does
// nothing. The previous session, if any, is released first.
func (e *Engine) Open(s *session.Session) error {
	e.mu.Lock()
	same := e.sess != nil && e.sess.ID == s.ID
	e.mu.Unlock()
	if same {
		// This process holds the lock already. The open session is
		// current, so there is nothing to reload.
		return nil
	}
	unlock, err := e.cfg.Store.Lock(s)
	if err != nil {
		return err
	}
	rec, err := e.cfg.Store.Recorder(s)
	if err != nil {
		unlock()
		return err
	}
	var recorder dashi.Recorder = rec
	if e.cfg.recorder != nil {
		recorder = e.cfg.recorder(rec)
	}
	inbox := dashi.NewInbox()
	chat := &dashi.Session{
		ID: s.ID,
		Runner: dashi.Runner{
			Client:   e.cfg.Client,
			Tools:    e.cfg.Wiring.Tools,
			Hooks:    e.cfg.Wiring.Hooks,
			Recorder: recorder,
		},
		Steering:  inbox,
		Exclusive: true,
	}
	// A session with no turn yet is created by its first prompt.
	if _, err := chat.Open(context.Background()); errors.Is(err, dashi.ErrSessionNotFound) {
		chat.New = true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.release()
	e.sess, e.chat, e.inbox, e.unlock = s, chat, inbox, unlock
	if s.Model != "" && e.model == "" {
		e.model = s.Model
	}
	return nil
}

// Close releases the open session.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.release()
}

// release unlocks the open session. A session with no turn is removed, so
// launching dashi and quitting leaves nothing behind. The caller holds e.mu.
func (e *Engine) release() {
	if e.unlock == nil {
		return
	}
	if e.sess != nil {
		e.cfg.Store.Discard(e.sess)
	}
	e.unlock()
	e.unlock = nil
}

// handle returns the open session's handle.
func (e *Engine) handle() *dashi.Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.chat
}

// LastTurn is the newest turn of the open session.
type LastTurn struct {
	// N is the turn number, counting from 1.
	N    int
	View dashi.TurnView
	// Next is the turn's next action. The process lock makes this process
	// the only driver, so a turn with no stop was left by one that exited.
	Next dashi.Next
}

// Open reports whether the turn has not ended for good. A new prompt closes
// an open turn, unless a command may still be running.
func (l *LastTurn) Open() bool {
	return l != nil && l.Next != dashi.NextNone
}

// CanGoOn reports whether Enter can run the turn on: Continue can run now,
// possibly with a raised model-call limit, or the turn failed in a way the
// user may retry. A turn with no recorded stop was left by a worker that
// exited, since the session lock rules out a live one.
func (l *LastTurn) CanGoOn() bool {
	return l != nil && (l.Next == dashi.NextContinue ||
		l.LimitStopped() ||
		l.Retryable() ||
		l.Next == dashi.NextResume && l.View.ReadyToContinue)
}

// Retryable reports whether the turn failed and Continue can run it again.
// See retryable.
func (l *LastTurn) Retryable() bool {
	return l != nil && retryable(l.View)
}

// retryable reports whether a failed turn can be retried as it is. The
// library says to resolve every failure, because it cannot tell a rejected
// request from a rate limit or an overload; a person at the terminal can,
// so Enter retries. A refusal is not a failure, so it is never retried.
func retryable(v dashi.TurnView) bool {
	return v.Next() == dashi.NextResolve && v.ReadyToContinue &&
		v.Stop.Kind == dashi.StopFailed
}

// StopText says in words why the turn stopped.
func (l *LastTurn) StopText() string {
	s := l.View.Stop
	switch {
	case l.LimitStopped():
		return "stopped at the model-call limit"
	case s.Kind == "" || s.Kind == dashi.StopInterrupted:
		return "was interrupted"
	case s.Reason != "":
		return fmt.Sprintf("stopped (%s: %s)", s.Kind, s.Reason)
	}
	return fmt.Sprintf("stopped (%s)", s.Kind)
}

// LimitStopped reports whether the turn stopped at its model-call limit.
func (l *LastTurn) LimitStopped() bool {
	return l != nil && l.Next == dashi.NextRaiseAttempts
}

// OpenCall returns a command the turn started and never finished, which
// may still be running after dashi exited: a call that is neither read-only
// nor idempotent, with no result. The session refuses a new prompt until
// the user marks it done (/done) or discards the turn (/discard).
func (l *LastTurn) OpenCall() (id, name string) {
	if l == nil {
		return "", ""
	}
	for _, p := range l.View.Pending {
		if p.Kind != dashi.PendingTool || p.Tool == nil {
			continue
		}
		a := p.Tool.Definition.Annotations
		safe := a.ReadOnly != nil && *a.ReadOnly || a.Idempotent != nil && *a.Idempotent
		if p.Tool.Status == dashi.ToolUncertain ||
			p.Tool.Status == dashi.ToolStarted && !safe {
			return p.ID, p.Tool.Call.Name
		}
	}
	return "", ""
}

// Last reads the newest turn of the session. It returns nil when the
// session has no turn, or when the record cannot be read; a warning
// explains the second case.
func (e *Engine) Last(ctx context.Context) *LastTurn {
	chat := e.handle()
	if chat == nil {
		return nil
	}
	st, err := chat.Open(ctx)
	if err != nil {
		if !errors.Is(err, dashi.ErrSessionNotFound) {
			e.warn("The last turn cannot be read: %v.", err)
		}
		return nil
	}
	if st.Turns == 0 {
		return nil
	}
	return &LastTurn{N: st.Turns, View: *st.Last, Next: st.Next}
}

// Turns returns the number of turns the session has.
func (e *Engine) Turns(ctx context.Context) int {
	if last := e.Last(ctx); last != nil {
		return last.N
	}
	return 0
}

// Scrollback returns the blocks of every turn of the open session. A turn
// that cannot be read gives a warning block and the session goes on.
func (e *Engine) Scrollback(ctx context.Context) []Block {
	s, chat := e.Session(), e.handle()
	if s == nil {
		return nil
	}
	diffs := e.cfg.Store.Diffs(s)
	var out []Block
	n := 0
	for turn, err := range chat.Turns(ctx) {
		n++
		if err != nil {
			out = append(out, warnBlock("Turn %d skipped: %v.", n, err))
			continue
		}
		blocks, err := RenderTurn(n, s.Model, e.cfg.Dir, turn, diffs[turn.ID])
		if err != nil {
			out = append(out, warnBlock("Turn %d skipped: %v.", n, err))
			continue
		}
		out = append(out, blocks...)
	}
	return out
}

// Result is what a turn command returns: the outcome of the last turn it
// ran. Input queued while a turn ran, and steers that missed it, run as the
// next turns in the same command.
type Result struct {
	Outcome dashi.Outcome
}

// Prompt starts a new turn with text. Input queued earlier goes first. If
// the last turn did not end, the session closes it first, and the screen
// says so; while a command of that turn may still be running, it refuses.
func (e *Engine) Prompt(ctx context.Context, text string) (Result, error) {
	return e.run(ctx, dashi.Say(text))
}

// Resume runs the last turn on, after a quit, a Ctrl-C, a failure to retry,
// or the model-call limit.
func (e *Engine) Resume(ctx context.Context) (Result, error) {
	last := e.Last(ctx)
	if last.LimitStopped() {
		return e.ContinueLimit(ctx)
	}
	if !last.CanGoOn() {
		return Result{}, errors.New("there is no turn to continue")
	}
	return e.run(ctx, dashi.Continue{})
}

// ContinueLimit runs the last turn on after its model-call limit stop. The
// same turn goes on with a limit twice as high.
func (e *Engine) ContinueLimit(ctx context.Context) (Result, error) {
	last := e.Last(ctx)
	if !last.LimitStopped() {
		return Result{}, errors.New("the last turn did not stop at the model-call limit")
	}
	raised := int(last.View.Stop.Limit.Limit) * 2
	return e.run(ctx, dashi.Continue{ModelAttempts: raised})
}

// MarkDone records the open command of the last turn as finished, after the
// user checked it. Its output was not captured.
func (e *Engine) MarkDone(ctx context.Context) (Result, error) {
	id, _ := e.Last(ctx).OpenCall()
	if id == "" {
		return Result{}, errors.New("no command of the last turn is open")
	}
	done := tool.Succeeded("The user checked this command after dashi stopped " +
		"and marked it done. Its output was not captured.")
	return e.run(ctx, dashi.Settle{CallID: id, Outcome: &done})
}

// Discard cancels the last turn, accepting that a command it started may
// still have run. The record keeps the cancel.
func (e *Engine) Discard(ctx context.Context) (Result, error) {
	if !e.Last(ctx).Open() {
		return Result{}, errors.New("there is no open turn to discard")
	}
	return e.run(ctx, dashi.Cancel{Reason: "The user discarded the turn."})
}

// Steer sends text to the running turn. An urgent steer also cuts the model
// call in flight. With no turn running, the text waits for the next turn.
func (e *Engine) Steer(text string, urgent bool) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("the steer is empty")
	}
	q, err := e.handle().SteerItem(context.Background(), dashi.Steer{
		Item: dashi.ContextItem{
			Origin:  dashi.ContextUser,
			Message: llm.UserMessage(llm.Text(text)),
		},
		Urgent: urgent,
	})
	if err != nil {
		return err
	}
	state := SteerQueued
	if q.TurnID == "" {
		state = SteerNext
	}
	e.cfg.Emit(Block{ID: "s:" + q.ID, Kind: KindSteer, Text: text, State: state})
	return nil
}

// Queue holds text as the next prompt. It runs when the running turn ends,
// or goes ahead of the next prompt if that turn stops first.
func (e *Engine) Queue(text string) error {
	q, err := e.handle().Enqueue(context.Background(), text)
	if err != nil {
		return err
	}
	e.cfg.Emit(Block{ID: "s:" + q.ID, Kind: KindSteer, Text: text, State: SteerNext})
	return nil
}

// agent is the agent fields the CLI sets on every new turn. The session
// carries the rest forward. Continue and Cancel run with the agent the turn
// recorded. The caller holds e.mu.
func (e *Engine) agent() dashi.Agent {
	return dashi.Agent{
		ID:           "dashi-cli",
		Model:        e.model,
		Instructions: systemPrompt(e.cfg.Dir, e.cfg.Wiring.Instructions),
		Limits:       dashi.Limits{ModelAttempts: e.cfg.ModelAttempts},
	}
}

// run streams one command, and the turns it leads to, as blocks.
func (e *Engine) run(ctx context.Context, cmd dashi.Command) (Result, error) {
	e.mu.Lock()
	chat := e.chat
	chat.Agent = e.agent()
	e.mu.Unlock()
	renderers := map[string]*Renderer{}
	renderer := func(id string) *Renderer {
		r := renderers[id]
		if r == nil {
			r = NewRenderer(turnNumber(id), e.Model(), e.cfg.Dir)
			renderers[id] = r
		}
		return r
	}
	var (
		res    Result
		have   bool
		runErr error
	)
	for ev, err := range chat.Stream(ctx, cmd) {
		if err != nil {
			runErr = err
		}
		if ev.Outcome != nil {
			res.Outcome, have = *ev.Outcome, true
			e.settled(ctx, renderer(ev.TurnID), *ev.Outcome)
			continue
		}
		if err != nil || ev.TurnID == "" {
			continue
		}
		r := renderer(ev.TurnID)
		if ev.Step != nil && ev.Step.Kind == dashi.StepStarted {
			if err := e.record(ev.Step); err != nil {
				e.warn("The session file was not saved: %v.", err)
			}
		}
		for _, b := range r.Event(ev) {
			e.cfg.Emit(b)
		}
		if ev.Step != nil && ev.Step.Kind == dashi.StepToolSettled &&
			r.CallName(ev.Step.Tool.Call.ID) == "bash" {
			if b, ok := gitDiffBlock(ctx, e.cfg.Dir, ev.Step.Seq); ok {
				b.ID = diffID(ev.TurnID, ev.Step.Seq)
				e.saveDiff(ev.TurnID, ev.Step.Seq, b.Lines)
				e.cfg.Emit(b)
			}
		}
	}
	if !have {
		return Result{}, runErr
	}
	return res, runErr
}

// settled shows the end of one turn: its footer, how to go on, the session's
// warnings, and the input that waits for a later turn.
func (e *Engine) settled(ctx context.Context, r *Renderer, out dashi.Outcome) {
	for _, w := range out.SessionWarnings {
		if w.Code == dashi.WarningTurnUnreadable {
			e.warn("%s.", w.Message)
		}
	}
	if out.Stop.Kind == "" {
		return
	}
	r.Model = turnModel(out.Turn, r.Model)
	e.cfg.Emit(r.Footer(out.TurnView))
	if retryable(out.TurnView) {
		e.cfg.Emit(Block{
			Kind: KindNotice,
			Text: "Press Enter to retry, or type a new prompt.",
			Done: true,
		})
	}
	switch out.Next() {
	case dashi.NextNone:
		return
	case dashi.NextRaiseAttempts:
		// The turn can go on, so the steers that missed it stay for its
		// continuation.
		if n := e.queuedSteers(out.Turn.ID); n > 0 {
			e.notice("%d queued %s will reach the turn when it continues.",
				n, plural(n, "steer"))
		}
	default:
		// Steers that missed the turn wait for its continuation, or go
		// ahead of the next prompt.
		if n := e.queuedSteers(out.Turn.ID); n > 0 {
			e.notice("%d queued %s kept for the next turn.", n, plural(n, "steer"))
		}
	}
	if st, err := e.handle().Open(ctx); err == nil && st.Queued > 0 {
		e.notice("%d queued %s will go with the next prompt.",
			st.Queued, plural(st.Queued, "message"))
	}
}

// queuedSteers counts the steers that no run has recorded.
func (e *Engine) queuedSteers(turnID string) int {
	e.mu.Lock()
	inbox := e.inbox
	e.mu.Unlock()
	steers, err := inbox.Steers(context.Background(), turnID)
	if err != nil {
		return 0
	}
	return len(steers)
}

// turnNumber reads n from a turn ID "<session>-<n>".
func turnNumber(id string) int {
	i := strings.LastIndexByte(id, '-')
	n, _ := strconv.Atoi(id[i+1:])
	return n
}

// turnModel returns the model the turn recorded when it started, or
// fallback for a turn with no start step yet.
func turnModel(turn dashi.Turn, fallback string) string {
	for _, s := range turn.Steps {
		if s.Started != nil && s.Started.Agent.Model != "" {
			return s.Started.Agent.Model
		}
	}
	return fallback
}

// record saves the session file when a turn starts: the first prompt
// becomes the title, and the time orders the session list.
func (e *Engine) record(s *dashi.Step) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sess.Title == "" && s.Started != nil {
		e.sess.Title = truncate(
			strings.Join(strings.Fields(joinMessages(s.Started.Input)), " "),
			60,
		)
	}
	e.sess.Model = e.model
	return e.cfg.Store.Save(e.sess)
}

func (e *Engine) warn(format string, args ...any) {
	e.cfg.Emit(warnBlock(format, args...))
}

func (e *Engine) notice(format string, args ...any) {
	e.cfg.Emit(Block{Kind: KindNotice, Text: fmt.Sprintf(format, args...), Done: true})
}

func warnBlock(format string, args ...any) Block {
	return Block{Kind: KindWarning, Text: fmt.Sprintf(format, args...), Done: true}
}

// maxDiffLines caps the diff shown after a shell call.
const maxDiffLines = 40

// gitDiffBlock returns the uncommitted changes of dir, capped at 40 lines. It
// reports false when dir is not a Git repository or nothing changed.
func gitDiffBlock(ctx context.Context, dir string, seq int64) (Block, bool) {
	stat, err := gitOutput(ctx, dir, "diff", "--stat")
	if err != nil || strings.TrimSpace(stat) == "" {
		return Block{}, false
	}
	diff, _ := gitOutput(ctx, dir, "diff")
	lines := strings.Split(strings.TrimRight(stat, "\n"), "\n")
	lines = append(lines, "")
	lines = append(lines, strings.Split(strings.TrimRight(diff, "\n"), "\n")...)
	if len(lines) > maxDiffLines {
		hidden := len(lines) - maxDiffLines
		lines = append(lines[:maxDiffLines],
			fmt.Sprintf("... %d more lines", hidden))
	}
	return diffBlock("", seq, lines), true
}

func diffID(turnID string, seq int64) string {
	return fmt.Sprintf("diff:%s:%d", turnID, seq)
}

func diffBlock(turnID string, seq int64, lines []string) Block {
	return Block{
		ID:    diffID(turnID, seq),
		Kind:  KindDiff,
		Lines: lines,
		Done:  true,
	}
}

// saveDiff keeps a shown diff, so a reopened session shows it again.
func (e *Engine) saveDiff(turnID string, seq int64, lines []string) {
	s := e.Session()
	err := e.cfg.Store.AppendDiff(s, session.Diff{
		Turn: turnID, Seq: seq, Lines: lines,
	})
	if err != nil {
		e.warn("The diff was not saved: %v.", err)
	}
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return string(out), err
}

// systemPrompt tells the model what it is and where it works.
func systemPrompt(dir, extra string) string {
	p := "You are Dashi, a coding agent that works in a terminal. " +
		"The user's repository is at " + dir + ". " +
		"Use the tools to read, search, and change files and to run " +
		"commands. Read a file before you edit it. Keep changes small and " +
		"run the tests after a change. Show your work in short steps, and " +
		"finish with a brief summary of what you did. The user may send " +
		"new messages while you work. Read them and follow the latest one."
	if extra != "" {
		p += "\n\n" + extra
	}
	return p
}
