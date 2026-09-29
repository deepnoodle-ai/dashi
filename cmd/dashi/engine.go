package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/cmd/dashi/session"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
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
}

// Engine runs the turns of one session. It owns the session record, the
// steering inbox, and the model history. It has no screen: the Controller and
// the screen sit on top of it.
type Engine struct {
	cfg   EngineConfig
	Inbox *dashi.Inbox

	mu     sync.Mutex
	sess   *session.Session
	rec    *filerecorder.Recorder
	unlock func()
	model  string
}

// NewEngine returns an engine with no session. Call Open before any turn.
func NewEngine(cfg EngineConfig) *Engine {
	if cfg.ModelAttempts == 0 {
		cfg.ModelAttempts = defaultModelAttempts
	}
	if cfg.Emit == nil {
		cfg.Emit = func(Block) {}
	}
	return &Engine{cfg: cfg, Inbox: dashi.NewInbox(), model: cfg.Model}
}

// Session returns the open session.
func (e *Engine) Session() *session.Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sess
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
	if e.sess != nil {
		e.sess.Model = name
		_ = e.cfg.Store.Save(e.sess)
	}
}

// Open makes s the open session and locks it. It returns session.ErrBusy when
// another process holds s. The previous session, if any, is released first.
func (e *Engine) Open(s *session.Session) error {
	unlock, err := e.cfg.Store.Lock(s)
	if err != nil {
		return err
	}
	rec, err := e.cfg.Store.Recorder(s)
	if err != nil {
		unlock()
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.unlock != nil {
		e.unlock()
	}
	e.sess, e.rec, e.unlock = s, rec, unlock
	if s.Model != "" && e.model == "" {
		e.model = s.Model
	}
	return nil
}

// Close releases the open session.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.unlock != nil {
		e.unlock()
		e.unlock = nil
	}
}

// LastTurn is the newest turn of the open session.
type LastTurn struct {
	// N is the turn number, counting from 1.
	N    int
	Turn dashi.Turn
	View dashi.TurnView
}

// Unfinished reports whether the turn can still run: it did not complete,
// and was not canceled or ended for good.
func (l *LastTurn) Unfinished() bool {
	return l != nil && !l.View.Stop.Terminal
}

// LimitStopped reports whether the turn stopped at its model-call limit.
func (l *LastTurn) LimitStopped() bool {
	if l == nil {
		return false
	}
	s := l.View.Stop
	return s.Kind == dashi.StopLimited && s.Limit != nil &&
		s.Limit.Kind == dashi.LimitModelAttempts && !s.Terminal
}

// Last loads the newest turn of the session. It returns nil when the
// session has no turn, or when the record cannot be read; a warning explains
// the second case.
func (e *Engine) Last(ctx context.Context) *LastTurn {
	e.mu.Lock()
	s, rec := e.sess, e.rec
	e.mu.Unlock()
	if s == nil || len(s.Turns) == 0 {
		return nil
	}
	n := len(s.Turns)
	turn, err := rec.Load(ctx, s.Turns[n-1])
	if err != nil {
		e.warn("Turn %d cannot be read: %v.", n, err)
		return nil
	}
	view, err := dashi.Replay(turn)
	if err != nil {
		e.warn("Turn %d cannot be replayed: %v.", n, err)
		return nil
	}
	return &LastTurn{N: n, Turn: turn, View: view}
}

// Scrollback returns the blocks of every turn of the open session. A turn
// that cannot be read gives a warning block and the session goes on.
func (e *Engine) Scrollback(ctx context.Context) []Block {
	e.mu.Lock()
	s, rec := e.sess, e.rec
	e.mu.Unlock()
	if s == nil {
		return nil
	}
	var out []Block
	for i, id := range s.Turns {
		turn, err := rec.Load(ctx, id)
		if err != nil {
			out = append(out, warnBlock("Turn %d skipped: %v.", i+1, err))
			continue
		}
		blocks, err := RenderTurn(i+1, s.Model, turn)
		if err != nil {
			out = append(out, warnBlock("Turn %d skipped: %v.", i+1, err))
			continue
		}
		out = append(out, blocks...)
	}
	return out
}

// Result is what a turn command returns.
type Result struct {
	Outcome dashi.Outcome
	// Leftovers holds steers that missed their turn. They are the input of
	// the next turn.
	Leftovers []string
}

// Prompt starts a new turn with text. If the last turn did not end, it is
// canceled first, and the screen says so.
func (e *Engine) Prompt(ctx context.Context, text string) (Result, error) {
	if last := e.Last(ctx); last.Unfinished() {
		if err := e.cancel(ctx, last); err != nil {
			return Result{}, err
		}
	}
	s := e.Session()
	hist, err := e.history(ctx, len(s.Turns))
	if err != nil {
		return Result{}, err
	}
	id := s.NextTurnID()
	agent := e.agent()
	return e.run(ctx, len(s.Turns)+1, dashi.NewTurn(id), dashi.Start{
		Input:   []llm.Message{llm.UserMessage(llm.Text(text))},
		History: hist,
	}, agent)
}

// Resume runs an unfinished turn on, after a quit or a Ctrl-C.
func (e *Engine) Resume(ctx context.Context) (Result, error) {
	last := e.Last(ctx)
	if !last.Unfinished() {
		return Result{}, errors.New("there is no unfinished turn")
	}
	hist, err := e.history(ctx, last.N-1)
	if err != nil {
		return Result{}, err
	}
	return e.run(ctx, last.N, last.Turn, dashi.Continue{History: hist}, dashi.Agent{})
}

// ContinueLimit runs the last turn on after its model-call limit stop. The
// same turn goes on with a limit twice as high.
func (e *Engine) ContinueLimit(ctx context.Context) (Result, error) {
	last := e.Last(ctx)
	if !last.LimitStopped() {
		return Result{}, errors.New("the last turn did not stop at the model-call limit")
	}
	hist, err := e.history(ctx, last.N-1)
	if err != nil {
		return Result{}, err
	}
	raised := int(last.View.Stop.Limit.Limit) * 2
	return e.run(ctx, last.N, last.Turn, dashi.Continue{
		History:       hist,
		ModelAttempts: raised,
	}, dashi.Agent{})
}

// Steer sends text to the running turn. An urgent steer also cuts the model
// call in flight.
func (e *Engine) Steer(text string, urgent bool) error {
	send := e.Inbox.SendText
	if urgent {
		send = e.Inbox.SendUrgent
	}
	id, err := send(text)
	if err != nil {
		return err
	}
	e.cfg.Emit(Block{
		ID:    "s:" + id,
		Kind:  KindSteer,
		Text:  text,
		State: SteerQueued,
	})
	return nil
}

func (e *Engine) agent() dashi.Agent {
	return dashi.Agent{
		ID:           "dashi-cli",
		Model:        e.Model(),
		Instructions: systemPrompt(e.cfg.Dir, e.cfg.Wiring.Instructions),
		Limits:       dashi.Limits{ModelAttempts: e.cfg.ModelAttempts},
	}
}

func (e *Engine) runner(agent dashi.Agent) dashi.Runner {
	e.mu.Lock()
	rec := e.rec
	e.mu.Unlock()
	return dashi.Runner{
		Agent:    agent,
		Client:   e.cfg.Client,
		Tools:    e.cfg.Wiring.Tools,
		Hooks:    e.cfg.Wiring.Hooks,
		Recorder: rec,
	}
}

// history builds the messages of the first n turns and shows a warning for
// each turn it had to skip.
func (e *Engine) history(ctx context.Context, n int) ([]llm.Message, error) {
	e.mu.Lock()
	s, rec := e.sess, e.rec
	e.mu.Unlock()
	h, err := e.cfg.Store.HistoryBefore(ctx, rec, s, n)
	if err != nil {
		return nil, err
	}
	for _, note := range h.Notices {
		e.cfg.Emit(Block{Kind: KindWarning, Text: note, Done: true})
	}
	return h.Messages, nil
}

func (e *Engine) cancel(ctx context.Context, last *LastTurn) error {
	_, err := e.runner(dashi.Agent{}).Run(ctx, last.Turn, dashi.Cancel{
		Reason: "The user started a new prompt.",
	})
	if err != nil {
		return fmt.Errorf("cancel turn %d: %w", last.N, err)
	}
	e.cfg.Emit(Block{
		Kind: KindStop,
		Text: fmt.Sprintf("Turn %d canceled.", last.N),
		Done: true,
	})
	return nil
}

// run streams one command and turns its events into blocks.
func (e *Engine) run(
	ctx context.Context,
	n int,
	turn dashi.Turn,
	cmd dashi.Command,
	agent dashi.Agent,
) (Result, error) {
	r := NewRenderer(n, e.Model())
	var (
		out      dashi.Outcome
		have     bool
		runErr   error
		recorded bool
	)
	for ev, err := range e.runner(agent).Stream(
		ctx, turn, cmd, dashi.WithSteering(e.Inbox),
	) {
		if ev.Outcome != nil {
			out, have = *ev.Outcome, true
		}
		if err != nil {
			runErr = err
			continue
		}
		if ev.Step != nil && ev.Step.Kind == dashi.StepStarted && !recorded {
			recorded = true
			if err := e.record(turn.ID, ev.Step); err != nil {
				e.warn("The session file was not saved: %v.", err)
			}
		}
		for _, b := range r.Event(ev) {
			e.cfg.Emit(b)
		}
		if ev.Step != nil && ev.Step.Kind == dashi.StepToolSettled &&
			r.CallName(ev.Step.Tool.Call.ID) == "bash" {
			if b, ok := gitDiffBlock(ctx, e.cfg.Dir, ev.Step.Seq); ok {
				e.cfg.Emit(b)
			}
		}
	}
	if !have {
		return Result{}, runErr
	}
	if out.Stop.Kind != "" {
		e.cfg.Emit(r.Footer(out.TurnView))
	}
	res := Result{Outcome: out}
	if left, err := e.Inbox.Leftovers(out.Turn); err == nil {
		for _, s := range left {
			res.Leftovers = append(res.Leftovers, s.Item.Message.Text())
		}
	}
	return res, runErr
}

// record adds a new turn to the session as soon as its first step is saved,
// so a crash later leaves the turn in the list.
func (e *Engine) record(id string, s *dashi.Step) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range e.sess.Turns {
		if t == id {
			return nil
		}
	}
	e.sess.Turns = append(e.sess.Turns, id)
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
	return Block{
		ID:    fmt.Sprintf("diff:%d", seq),
		Kind:  KindDiff,
		Lines: lines,
		Done:  true,
	}, true
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
