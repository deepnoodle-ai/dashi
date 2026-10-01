package main

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
	dashisession "github.com/deepnoodle-ai/dashi/session"
	"github.com/deepnoodle-ai/dashi/tool"
)

// defaultModelAttempts is the model-call limit of one turn. Coding reads many
// files, and Dashi's own default of 8 stops too early.
const defaultModelAttempts = 40

// EngineConfig holds what an Engine needs.
type EngineConfig struct {
	// Dir is the workspace directory, absolute.
	Dir         string
	Client      llm.Client
	Model       string
	Store       *session.Store
	Wiring      wiring
	Preferences *cliPreferences
	Resources   *resourceManager
	MCP         *mcpRuntime
	// ModelAttempts is the per-turn model-call limit; zero means 40.
	ModelAttempts int
	// Emit receives every block the engine produces. It may run on any
	// goroutine.
	Emit    func(Block)
	Receipt func(*submittedInput, inputReceipt)
	// ParentStopped is a nonblocking notification of acknowledged parent stops.
	ParentStopped func(int)

	// recorder wraps the session's recorder; tests use it to fail commits.
	recorder func(*filerecorder.Recorder) dashi.Recorder
}

// Engine runs the turns of one session. It holds the session's metadata and
// a dashisession.Session handle, which owns the turn numbers, the history, the
// queue, and the rules for a new prompt while a turn is open. The steering
// inbox is the handle's. The Engine has no screen: the Controller and the
// screen sit on top of it.
type Engine struct {
	cfg EngineConfig

	mu   sync.Mutex
	sess *session.Session
	// chat drives the open session. The process lock makes it the only
	// driver, so it is Exclusive.
	chat               *dashisession.Session
	inbox              *dashi.Inbox
	unlock             func()
	attachmentResolver *engineAttachmentResolver
	// model is the model the next Start uses. Changing it does not touch
	// turns already started.
	model        string
	windowWarned map[string]bool
	// sessionNotices maps each once-per-session notice text to the block
	// that showed it in the open session.
	sessionNotices    map[string]string
	tasks             *taskHost
	taskWake          func()
	persistenceFailed bool

	// inputMu orders later submissions behind a not-yet-recorded prompt.
	driverMu sync.Mutex
	inputMu  sync.Mutex
	pending  *pendingPrompt
	later    []pendingLine
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
	return e.openOwned(s, nil)
}

func (e *Engine) openOwned(s *session.Session, ownedUnlock func()) error {
	e.mu.Lock()
	same := e.sess != nil && e.sess.ID == s.ID
	e.mu.Unlock()
	if same {
		// This process holds the lock already. The open session is
		// current, so there is nothing to reload.
		return nil
	}
	unlock := ownedUnlock
	var err error
	if unlock == nil {
		unlock, err = e.cfg.Store.Lock(s)
	}
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
	resolver := &engineAttachmentResolver{store: e.cfg.Store, session: s}
	chat := &dashisession.Session{
		ID: s.ID,
		Runner: dashi.Runner{
			Client:          e.cfg.Client,
			Tools:           e.cfg.Wiring.Tools,
			Hooks:           e.cfg.Wiring.Hooks,
			Recorder:        recorder,
			ContentResolver: resolver,
		},
		Steering:  inbox,
		Exclusive: true,
	}
	if stopped := e.cfg.ParentStopped; stopped != nil {
		chat.Runner.Observer = func(ev dashi.Event) {
			if ev.Step != nil && ev.Step.Kind == dashi.StepStopped && strings.HasPrefix(ev.TurnID, s.ID+"-") {
				// Child IDs are hashes. Only exact numbered parent identities qualify.
				n := turnNumber(ev.TurnID)
				if n > 0 && ev.TurnID == dashisession.TurnID(s.ID, n) {
					stopped(n)
				}
			}
		}
	}
	// A session with no turn yet is created by its first prompt.
	_, openErr := chat.Open(context.Background())
	if errors.Is(openErr, dashisession.ErrNotFound) {
		chat.New = true
	}
	if err := e.closeTasks(); err != nil {
		unlock()
		return err
	}
	host := e.newTaskHost(chat, resolver)
	e.mu.Lock()
	e.release()
	e.sess, e.chat, e.inbox, e.unlock = s, chat, inbox, unlock
	e.sessionNotices = nil
	e.attachmentResolver = resolver
	e.tasks = host
	if s.Model != "" && e.model == "" {
		e.model = s.Model
	}
	e.mu.Unlock()
	e.inputMu.Lock()
	e.pending, e.later = nil, nil
	e.inputMu.Unlock()
	e.configureChat()
	return nil
}

// Close releases the open session.
func (e *Engine) Close() {
	if err := e.closeTasks(); err != nil {
		e.warn("%v", err)
		return
	}
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
	if e.attachmentResolver != nil {
		e.attachmentResolver.close()
		e.attachmentResolver = nil
	}
	if e.sess != nil {
		e.cfg.Store.Discard(e.sess)
	}
	e.unlock()
	e.unlock = nil
}

// handle returns the open session's handle.
func (e *Engine) handle() *dashisession.Session {
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
		if !errors.Is(err, dashisession.ErrNotFound) {
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
		for _, b := range blocks {
			if !e.repeatsSessionNotice(b) {
				out = append(out, b)
			}
		}
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
	if err := e.cfg.Preferences.selectionReady(); err != nil {
		return Result{}, err
	}
	if !e.cfg.MCP.Ready() {
		return Result{}, errors.New("MCP startup has not completed; no work was started")
	}
	e.inputMu.Lock()
	if e.pending != nil {
		e.holdLineLocked(text)
	} else {
		if err := e.flushLaterLocked(); err != nil {
			e.holdLineLocked(text)
			e.inputMu.Unlock()
			return Result{}, err
		}
		e.mu.Lock()
		start := dashi.Say(text)
		start.CommandID = dashi.NewID()
		start.Agent = e.agent()
		e.mu.Unlock()
		// A first admission may skip damaged history under Exclusive. A
		// supplied key cannot bypass the library's idempotency scan. In
		// this case retain the generated key from the first recording
		// acknowledgment (or CommitError) before any retry.
		if _, err := e.handle().Open(ctx); errors.Is(err, filerecorder.ErrCorrupt) {
			start.CommandID = ""
		}
		e.pending = &pendingPrompt{start: start}
	}
	e.inputMu.Unlock()
	return e.runPending(ctx)
}

// Resume runs the last turn on, after a quit, a Ctrl-C, a failure to retry,
// or the model-call limit.
func (e *Engine) Resume(ctx context.Context) (Result, error) {
	if err := e.cfg.Preferences.selectionReady(); err != nil {
		return Result{}, err
	}
	if !e.cfg.MCP.Ready() {
		return Result{}, errors.New("MCP startup has not completed; no work was started")
	}
	if e.HasPending() {
		return e.resumePending(ctx)
	}
	last := e.Last(ctx)
	if last.LimitStopped() {
		return e.ContinueLimit(ctx)
	}
	if !last.CanGoOn() {
		return Result{}, errors.New("there is no turn to continue")
	}
	return e.continueLast(ctx, dashi.Continue{}, last)
}

// ContinueLimit runs the last turn on after its model-call limit stop. The
// same turn goes on with a limit twice as high.
func (e *Engine) ContinueLimit(ctx context.Context) (Result, error) {
	last := e.Last(ctx)
	if !last.LimitStopped() {
		return Result{}, errors.New("the last turn did not stop at the model-call limit")
	}
	if e.HasPending() {
		return e.resumePending(ctx)
	}
	raised := int(last.View.Stop.Limit.Limit) * 2
	return e.continueLast(ctx, dashi.Continue{ModelAttempts: raised}, last)
}

// Continuing a summary authorizes only that summary. Stop at its Outcome
// before Session dispatches any queued user turn, including after reopening.
func (e *Engine) continueLast(ctx context.Context, cmd dashi.Continue, last *LastTurn) (Result, error) {
	if last != nil && last.View.Origin == dashi.StartCompaction {
		e.driverMu.Lock()
		defer e.driverMu.Unlock()
		chat := e.configureChat()
		if err := e.bindCommand(ctx, cmd); err != nil {
			return Result{}, err
		}
		return e.stream(ctx, chat.Stream(ctx, cmd), true)
	}
	return e.run(ctx, cmd)
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
	if err := e.cfg.Preferences.selectionReady(); err != nil {
		return err
	}
	if !e.cfg.MCP.Ready() {
		return errors.New("MCP startup has not completed; no input was queued")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("the steer is empty")
	}
	e.inputMu.Lock()
	if e.pending != nil {
		e.holdLineLocked(text)
		e.inputMu.Unlock()
		if urgent {
			e.notice("This input cannot interrupt compaction. It is queued; press Ctrl-C to interrupt.")
		}
		return nil
	}
	e.inputMu.Unlock()
	q, err := e.handle().SteerItem(context.Background(), dashi.Steer{
		Item: dashi.ContextItem{
			Origin:  dashi.ContextUser,
			Message: llm.UserMessage(llm.Text(text)),
		},
		Urgent: urgent,
	})
	if err != nil {
		var busy *dashisession.TurnBusyError
		if errors.As(err, &busy) && busy.Origin == dashi.StartCompaction {
			if urgent {
				e.notice("This input cannot interrupt compaction. It is queued; press Ctrl-C to interrupt.")
			}
			return e.Queue(text)
		}
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
	if err := e.cfg.Preferences.selectionReady(); err != nil {
		return err
	}
	if !e.cfg.MCP.Ready() {
		return errors.New("MCP startup has not completed; no input was queued")
	}
	e.inputMu.Lock()
	defer e.inputMu.Unlock()
	if e.pending != nil {
		e.holdLineLocked(text)
		return nil
	}
	if err := e.flushLaterLocked(); err != nil {
		return err
	}
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
	if p := e.cfg.Preferences; p != nil {
		agent := p.agent()
		guidance := ""
		if p.guidanceSet {
			guidance = p.guidance
		}
		agent.Instructions = composedSystemPrompt(e.cfg.Dir, e.cfg.Wiring.Instructions, guidance, p.guidanceSet)
		agent.Limits = dashi.Limits{ModelAttempts: e.cfg.ModelAttempts}
		return agent
	}
	return dashi.Agent{
		ID:               "dashi-cli",
		Model:            e.model,
		Instructions:     systemPrompt(e.cfg.Dir, e.cfg.Wiring.Instructions),
		Limits:           dashi.Limits{ModelAttempts: e.cfg.ModelAttempts},
		ActiveCompaction: &dashi.ActiveCompactionPolicy{},
	}
}

// run streams one command, and the turns it leads to, as blocks.
func (e *Engine) run(ctx context.Context, cmd dashi.Command) (Result, error) {
	e.driverMu.Lock()
	defer e.driverMu.Unlock()
	if err := e.recoverTasks(ctx); err != nil {
		return Result{}, err
	}
	if !e.cfg.MCP.Ready() {
		return Result{}, errors.New("MCP startup has not completed; no work was started")
	}
	chat := e.configureChat()
	if err := e.bindCommand(ctx, cmd); err != nil {
		return Result{}, err
	}
	if start, ok := cmd.(dashi.Start); ok {
		if e.cfg.Preferences != nil {
			chat.Compactor = e.routeCompactor(start.Agent)
		}
		return e.stream(ctx, chat.Stream(ctx, cmd), false, &start.Agent)
	}
	return e.stream(ctx, chat.Stream(ctx, cmd), false)
}

func (e *Engine) stream(
	ctx context.Context,
	events iter.Seq2[dashi.Event, error],
	stopCompaction bool,
	admission ...*dashi.Agent,
) (Result, error) {
	return e.streamBounded(ctx, events, stopCompaction, false, admission...)
}

func (e *Engine) streamBounded(ctx context.Context, events iter.Seq2[dashi.Event, error], stopCompaction, stopOutcome bool, admission ...*dashi.Agent) (Result, error) {
	renderers := map[string]*Renderer{}
	renderer := func(id string) *Renderer {
		r := renderers[id]
		if r == nil {
			r = NewRenderer(turnNumber(id), e.Model(), e.cfg.Dir)
			if loader, ok := e.handle().Runner.Recorder.(dashi.Loader); ok {
				if turn, err := loader.Load(ctx, id); err == nil {
					if view, err := dashi.Replay(turn); err == nil {
						r.Origin, r.summary = view.Origin, view.Answer
						r.showReasoning = view.Agent.Settings.ReasoningSummary != nil && *view.Agent.Settings.ReasoningSummary
					}
				}
			}
			renderers[id] = r
		}
		return r
	}
	var (
		res    Result
		have   bool
		runErr error
	)
	for ev, err := range events {
		if err != nil {
			runErr = err
			e.rememberPromptError(err)
		}
		if ev.Outcome != nil {
			if len(ev.Outcome.Turn.Steps) > 0 {
				if err := e.acknowledgePrompt(&ev.Outcome.Turn.Steps[0]); err != nil {
					e.warn("%v.", err)
				}
			}
			res.Outcome, have = *ev.Outcome, true
			if e.cfg.ParentStopped != nil && ev.Outcome.Origin != dashi.StartCompaction {
				e.cfg.ParentStopped(turnNumber(ev.TurnID))
			}
			e.settled(renderer(ev.TurnID), *ev.Outcome)
			if stopOutcome {
				break
			}
			if p := e.cfg.Preferences; p != nil && ev.Outcome.Next() == dashi.NextNone {
				var next *dashi.Agent
				if ev.Outcome.Origin == dashi.StartCompaction && len(admission) > 0 {
					next = admission[0]
				}
				if ev.Outcome.Origin != dashi.StartCompaction && e.Queued() > 0 {
					agent := p.agent()
					next = &agent
				}
				if next != nil {
					if err := p.bind(ctx, *next); err != nil {
						runErr = err
						break
					}
				}
			}
			if stopCompaction && ev.Outcome.Origin == dashi.StartCompaction {
				break
			}
			continue
		}
		if err != nil || ev.TurnID == "" {
			continue
		}
		r := renderer(ev.TurnID)
		if ev.Step != nil && ev.Step.Kind == dashi.StepStarted {
			if p := e.cfg.Preferences; p != nil {
				if err := p.bind(ctx, ev.Step.Started.Agent); err != nil {
					runErr = err
					break
				}
			}
			if ev.Step.Started.Origin == dashi.StartCompaction {
				e.inputMu.Lock()
				if e.pending != nil {
					e.pending.summary = true
				}
				e.inputMu.Unlock()
				e.showPending()
			}
			if err := e.acknowledgePrompt(ev.Step); err != nil {
				e.warn("%v.", err)
			}
			if err := e.record(ev.Step); err != nil {
				e.warn("The session file was not saved: %v.", err)
			}
		}
		for _, b := range r.Event(ev) {
			if !e.repeatsSessionNotice(b) {
				e.cfg.Emit(b)
			}
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
	e.showPending()
	// Queued prompts that no turn took wait for the next prompt: after a
	// turn that stopped without ending, a /discard or /done, or a refusal.
	if n := e.Queued(); n > 0 {
		e.notice("%d queued %s will go with the next prompt.",
			n, plural(n, "message"))
	}
	if !have {
		return Result{}, runErr
	}
	return res, runErr
}

// Queued counts the prompts and missed steers waiting in the open session's
// queue. They live in this process only, so quitting or leaving the
// session drops them.
func (e *Engine) Queued() int {
	e.inputMu.Lock()
	n := len(e.later)
	if e.pending != nil {
		n++
	}
	e.inputMu.Unlock()
	chat := e.handle()
	if chat == nil {
		return n
	}
	st, err := chat.Open(context.Background())
	if err != nil {
		return n
	}
	return n + st.Queued
}

// settled shows the end of one turn: its footer, how to go on, the session's
// warnings, and the steers that wait for a later turn.
func (e *Engine) settled(r *Renderer, out dashi.Outcome) {
	for _, w := range out.SessionWarnings {
		if w.Code == dashi.WarningTurnUnreadable ||
			w.Code == dashisession.WarningCompactionFailed ||
			w.Code == dashi.WarningCompactionMismatch {
			e.warn("%s.", w.Message)
		}
	}
	if out.Stop.Kind == "" {
		return
	}
	if out.Origin == dashi.StartCompaction {
		e.cfg.Emit(r.compactionOutcome(out.TurnView))
	}
	for _, b := range r.activeContextStopped(out.Stop) {
		e.cfg.Emit(b)
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
	if e.sess.Title == "" && s.Started != nil && s.Started.Origin != dashi.StartCompaction {
		e.sess.Title = truncate(
			strings.Join(strings.Fields(joinMessages(s.Started.Input)), " "),
			60,
		)
	}
	e.sess.Model = e.model
	if e.cfg.Preferences != nil {
		e.sess.NextTurn = e.cfg.Preferences.metadata()
	}
	err := e.cfg.Store.Save(e.sess)
	if err != nil {
		e.persistenceFailed = true
	}
	return err
}

// repeatsSessionNotice reports whether b repeats a notice already shown in
// the open session. Active-Turn protection limits rarely change between
// Turns, so only the first Turn shows each one. An update to the block that
// showed it is not a repeat.
func (e *Engine) repeatsSessionNotice(b Block) bool {
	if b.Kind != KindCompaction || b.State != "unavailable" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if id, ok := e.sessionNotices[b.Text]; ok {
		return id != b.ID
	}
	if e.sessionNotices == nil {
		e.sessionNotices = map[string]string{}
	}
	e.sessionNotices[b.Text] = b.ID
	return false
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

func composedSystemPrompt(dir, extra, guidance string, custom bool) string {
	if !custom {
		return systemPrompt(dir, extra)
	}
	text := guidance + "\n\nWorkspace: " + dir
	if extra != "" {
		text += "\n\n" + extra
	}
	return text
}
