package dashi

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/deepnoodle-ai/dashi/llm"
)

// Session drives one conversation of many turns: a session handle. It sits
// on Runner.Run and uses only the engine's public contract, as http.Get
// sits on Client.Do. The session keeps no record of its own: turn n of
// session S is the turn S-n (TurnID), and the turn records are the only
// truth. A handle holds settings and a cache, so a server can build one per
// request, and many handles in many processes can drive one session.
//
// The zero value works once it has a client:
//
//	s := &dashi.Session{Runner: dashi.Runner{Client: client}}
//	out, err := s.Send(ctx, "What's the weather in Tokyo?")
//	out, err = s.Send(ctx, "And in Fahrenheit?")
//
// Each new turn gets the projected history of the turns before it. A
// Session must not be copied after first use.
type Session struct {
	// ID names the session. Empty means a new session with a generated ID
	// (NewID), readable from ID after the first Start.
	ID string
	// New creates the session: the first Start makes turn 1, and a session
	// that already has turns fails with ErrSessionExists. Without New, an ID
	// with no turns fails with ErrSessionNotFound, so a mistyped ID never
	// starts an empty session.
	New bool
	// Runner supplies services. A nil Recorder becomes one in-memory
	// recorder that the handle keeps across calls, and the first outcome
	// warns WarningMemoryOnly.
	Runner Runner
	// Agent fills the zero fields of each new turn's Start.Agent, field by
	// field. It does not change turns already started.
	Agent Agent
	// Exclusive promises that this handle is the only driver of the session
	// and that earlier drivers have exited, so a turn this handle is not
	// running has no worker. It proves nothing about effects they started:
	// an unsettled uncertain call still makes a turn busy.
	Exclusive bool

	// mu guards everything below. Run holds it while choosing the turn and
	// building the command, never while a turn runs.
	mu sync.Mutex
	// memory is the recorder used when Runner.Recorder is nil.
	memory *MemoryRecorder
	// warnedMemory is set once an outcome carried WarningMemoryOnly.
	warnedMemory bool
	// created is set once this handle started the session's first turn, or
	// found turns it may use, so New no longer applies.
	created bool
	// last is the highest turn number known to exist. Turns are never
	// deleted, so it is a lower bound for the next search.
	last int
	// turns caches loaded turns by number.
	turns map[int]*sessionTurn
	// running counts the commands this handle is running, by turn ID.
	running map[string]int
}

// CloseReason is the Cancel reason recorded when a new message closes a
// stopped turn that has not ended.
const CloseReason = "a new message moved on"

var (
	// ErrTurnBusy matches a *TurnBusyError.
	ErrTurnBusy = errors.New("dashi: session turn is busy")
	// ErrSessionNotFound reports a session ID with no turns, on a handle
	// without New, or a command other than Start on a session with no turns.
	ErrSessionNotFound = errors.New("dashi: session not found")
	// ErrSessionExists reports New on a session that already has turns.
	ErrSessionExists = errors.New("dashi: session already exists")
)

// TurnBusyError refuses a Start because the session's last turn is busy:
// live, possibly live, waiting on the host, or holding an unsettled
// uncertain call. The message names the way out. It encodes as JSON, so a
// server can return it as a 409 body.
type TurnBusyError struct {
	TurnID string `json:"turn_id"`
	// Next is the busy turn's next action, refined with liveness.
	Next Next `json:"next"`
	// Origin is who started the busy turn.
	Origin StartOrigin `json:"origin,omitempty"`
	// Duplicate means the refused Start is a retry of that turn, which has
	// not ended yet.
	Duplicate bool `json:"duplicate,omitempty"`
	// CallID names the unsettled uncertain call that makes the turn busy
	// when no worker is known to run the turn. Settle it after checking its
	// effect, or Cancel the turn. It is empty while the turn is running.
	CallID string `json:"call_id,omitempty"`
}

func (e *TurnBusyError) Error() string {
	switch {
	case e.Duplicate:
		return fmt.Sprintf(
			"dashi: turn %s already runs this command and has not ended "+
				"(next: %s); wait for it, then retry",
			e.TurnID,
			e.Next,
		)
	case e.CallID != "":
		return fmt.Sprintf(
			"dashi: call %s in turn %s may still be running; Settle{} it "+
				"after checking its effect, or Cancel{} the turn to accept "+
				"the risk",
			e.CallID,
			e.TurnID,
		)
	case e.Next == NextRunning:
		return fmt.Sprintf(
			"dashi: turn %s is running; steer it, or stop it and send again",
			e.TurnID,
		)
	case e.Next == NextResume:
		return fmt.Sprintf(
			"dashi: turn %s may still be running; if its worker is lost, "+
				"take it over with Continue{} or discard it with Cancel{}",
			e.TurnID,
		)
	case e.Next == NextAnswer:
		return fmt.Sprintf(
			"dashi: turn %s waits on the host; answer its pending work or "+
				"Cancel{} it",
			e.TurnID,
		)
	}
	return fmt.Sprintf("dashi: turn %s is busy (next: %s)", e.TurnID, e.Next)
}

// Unwrap lets errors.Is match ErrTurnBusy.
func (e *TurnBusyError) Unwrap() error {
	return ErrTurnBusy
}

// SessionState is what Open reports about a session.
type SessionState struct {
	// Turns is the number of the last turn; 0 for a new session.
	Turns int
	// Last is the last turn's view, or nil when Turns is 0.
	Last *TurnView
	// Next is the last turn's next action, refined with liveness: NextRunning
	// when this handle runs it or TurnLiveness reports it live. It is
	// NextNone for a new session.
	Next Next
}

// Say returns a Start holding one user text message.
func Say(text string) Start {
	return Start{Input: []llm.Message{llm.UserMessage(llm.Text(text))}}
}

// Send runs Say(text) as a new turn. It is exactly Run(ctx, Say(text)).
func (s *Session) Send(
	ctx context.Context,
	text string,
	opts ...RunOption,
) (Outcome, error) {
	return s.Run(ctx, Say(text), opts...)
}

// Run applies one command to the session. A Start opens the next turn,
// after the last-turn rules: it refuses with *TurnBusyError while the last
// turn is busy, and otherwise closes a stopped last turn and starts. Any
// other command (Continue, Settle, Deliver, Cancel) goes to the last turn.
// The session sets Start.History and Continue.History to the projected
// history of the turns before the target turn, replacing any the caller
// set, and fills a Start's agent from Session.Agent.
//
// The outcome and error mean what Runner.Run's do, and
// Outcome.SessionWarnings adds the session's own warnings.
func (s *Session) Run(
	ctx context.Context,
	c Command,
	opts ...RunOption,
) (Outcome, error) {
	switch c := c.(type) {
	case nil:
		return Outcome{}, errors.New("dashi: command is required")
	case Start:
		return s.start(ctx, c, opts)
	}
	return s.command(ctx, c, opts)
}

// Open reads the session's last turn and reports its state. It never runs
// anything. An ID with no turns gives ErrSessionNotFound, unless New is
// set; with New, a session that has turns gives ErrSessionExists. An empty
// ID is a new session.
func (s *Session) Open(ctx context.Context) (SessionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ID == "" {
		return SessionState{Next: NextNone}, nil
	}
	last, err := s.findLast(ctx)
	if err != nil {
		return SessionState{}, err
	}
	if err := s.checkExists(last); err != nil {
		return SessionState{}, err
	}
	if last == 0 {
		return SessionState{Next: NextNone}, nil
	}
	t, err := s.load(ctx, last)
	if err != nil {
		return SessionState{}, err
	}
	view := t.view
	return SessionState{
		Turns: last,
		Last:  &view,
		Next:  s.refine(ctx, t).next,
	}, nil
}

// command routes a command other than Start to the last turn.
func (s *Session) command(
	ctx context.Context,
	c Command,
	opts []RunOption,
) (Outcome, error) {
	s.mu.Lock()
	n, turn, warnings, err := s.planCommand(ctx, &c)
	r := s.runner()
	s.mu.Unlock()
	if err != nil {
		return Outcome{}, err
	}
	out, err := r.Run(ctx, turn, c, opts...)
	return s.finish(n, turn.ID, out, err, warnings)
}

func (s *Session) planCommand(
	ctx context.Context,
	c *Command,
) (int, Turn, []Warning, error) {
	if s.ID == "" {
		return 0, Turn{}, nil, ErrSessionNotFound
	}
	last, err := s.findLast(ctx)
	if err != nil {
		return 0, Turn{}, nil, err
	}
	if err := s.checkExists(last); err != nil {
		return 0, Turn{}, nil, err
	}
	if last == 0 {
		return 0, Turn{}, nil, fmt.Errorf("%w: %s", ErrSessionNotFound, s.ID)
	}
	t, err := s.load(ctx, last)
	if err != nil {
		return 0, Turn{}, nil, err
	}
	var warnings []Warning
	if cont, ok := (*c).(Continue); ok {
		cont.History, warnings = s.history(ctx, last-1)
		*c = cont
	}
	s.running[t.turn.ID]++
	return last, t.turn, warnings, nil
}

// start runs a new turn. A Start that loses a race on its turn ID reloads
// and applies the rules once more.
func (s *Session) start(
	ctx context.Context,
	c Start,
	opts []RunOption,
) (Outcome, error) {
	if c.CommandID == "" {
		c.CommandID = NewID()
	}
	for attempt := 0; ; attempt++ {
		s.mu.Lock()
		p, err := s.planStart(ctx, c, opts)
		r := s.runner()
		s.mu.Unlock()
		if err != nil {
			return Outcome{}, err
		}
		out, err := r.Run(ctx, NewTurn(p.id), p.command, opts...)
		out, err = s.finish(p.n, p.id, out, err, p.warnings)
		if attempt == 0 && lostStart(err) {
			continue
		}
		return out, err
	}
}

// lostStart reports whether a Start lost a race for its turn: another
// worker's Start took the turn ID before this one's turn_started step
// committed, so the next turn is free. Only the commit of turn_started
// reports that. A conflict on a later step means the turn started and
// another worker then took it over, and a command conflict means the
// command ID is taken; either way the Start must not run again.
func lostStart(err error) bool {
	var ce *CommitError
	return errors.As(err, &ce) && ce.Step != nil &&
		ce.Step.Kind == StepStarted &&
		errors.Is(ce.Err, ErrRevisionConflict)
}

// startPlan is a chosen new turn.
type startPlan struct {
	n        int
	id       string
	command  Start
	warnings []Warning
}

func (s *Session) planStart(
	ctx context.Context,
	c Start,
	opts []RunOption,
) (startPlan, error) {
	if s.ID == "" {
		s.ID = NewID()
		s.created = true
	}
	last, err := s.findLast(ctx)
	if err != nil {
		return startPlan{}, err
	}
	if err := s.checkExists(last); err != nil {
		return startPlan{}, err
	}
	// This handle now makes the session, so New no longer applies to it,
	// even while its first turn runs.
	s.created = true
	var warnings []Warning
	if last > 0 {
		closed, err := s.closeLast(ctx, last, opts)
		if err != nil {
			return startPlan{}, err
		}
		if closed {
			warnings = append(warnings, Warning{
				Code: WarningTurnClosed,
				Message: fmt.Sprintf(
					"turn %s was closed: %s",
					TurnID(s.ID, last),
					CloseReason,
				),
			})
		}
	}
	history, unreadable := s.history(ctx, last)
	warnings = append(warnings, unreadable...)
	c.History = history
	c.Agent = mergeAgent(c.Agent, s.Agent)
	id := TurnID(s.ID, last+1)
	s.running[id]++
	return startPlan{
		n:        last + 1,
		id:       id,
		command:  c,
		warnings: warnings,
	}, nil
}

// checkExists applies New: without it a session needs turns, and with it
// a session must have none, until this handle made or found the session.
func (s *Session) checkExists(last int) error {
	switch {
	case s.created:
		return nil
	case last == 0 && !s.New:
		return fmt.Errorf("%w: %s", ErrSessionNotFound, s.ID)
	case last > 0 && s.New:
		return fmt.Errorf("%w: %s", ErrSessionExists, s.ID)
	}
	s.created = last > 0
	return nil
}

// closeLast applies the last-turn rules to a new Start: it returns an error
// when the turn is busy, closes a stopped turn that has not ended, and
// reports whether it closed one.
func (s *Session) closeLast(
	ctx context.Context,
	last int,
	opts []RunOption,
) (bool, error) {
	t, err := s.load(ctx, last)
	if err != nil {
		return false, err
	}
	r := s.refine(ctx, t)
	if busy := r.busy(t, s.Exclusive); busy != nil {
		return false, busy
	}
	if r.next == NextNone {
		return false, nil
	}
	id := t.turn.ID
	var closing Command = Cancel{CommandID: "close:" + id, Reason: CloseReason}
	if t.view.Output != nil {
		// The engine refuses Cancel once structured output is recorded, so
		// Continue completes the turn without a model call, raising a spent
		// model-attempt limit so that it can.
		cont := Continue{CommandID: "close:" + id}
		if l := t.view.Stop.Limit; l != nil && l.Kind == LimitModelAttempts {
			cont.ModelAttempts = int(l.Limit) + 1
		}
		cont.History, _ = s.history(ctx, last-1)
		closing = cont
	}
	out, err := s.runner().Run(ctx, t.turn, closing, opts...)
	s.store(last, out.Turn)
	if err != nil {
		return false, err
	}
	if out.Next() != NextNone {
		return false, &TurnBusyError{
			TurnID: id,
			Next:   out.Next(),
			Origin: out.Origin,
		}
	}
	return true, nil
}

// finish records a command's result in the cache and adds the session's
// warnings to its outcome.
func (s *Session) finish(
	n int,
	id string,
	out Outcome,
	err error,
	warnings []Warning,
) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[id]--; s.running[id] <= 0 {
		delete(s.running, id)
	}
	if out.Turn.ID == id && len(out.Turn.Steps) > 0 {
		s.store(n, out.Turn)
		s.created = true
	}
	out.SessionWarnings = append(warnings, out.SessionWarnings...)
	return s.withMemoryWarningLocked(out), err
}

// runner returns the Runner with the recorder the session uses. The
// caller holds mu.
func (s *Session) runner() Runner {
	r := s.Runner
	r.Recorder = s.recorder()
	return r
}

// recorder returns Runner.Recorder, or the handle's in-memory recorder.
// The caller holds mu.
func (s *Session) recorder() Recorder {
	if s.Runner.Recorder != nil {
		return s.Runner.Recorder
	}
	if s.memory == nil {
		s.memory = NewMemoryRecorder()
	}
	return s.memory
}

func (s *Session) withMemoryWarningLocked(out Outcome) Outcome {
	if s.Runner.Recorder == nil && !s.warnedMemory {
		s.warnedMemory = true
		out.SessionWarnings = append(out.SessionWarnings, Warning{
			Code: WarningMemoryOnly,
			Message: "the session is recorded in memory only and ends " +
				"with this handle; set Runner.Recorder to keep it",
		})
	}
	return out
}
