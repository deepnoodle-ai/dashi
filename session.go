package dashi

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// Each new turn gets the projected history of the turns before it, and its
// agent carries forward from the last user turn field by field. A Session
// must not be copied after first use.
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
	// Agent overrides fields of the carried agent for new turns: a nonzero
	// field here wins over the last user turn's, and Start.Agent wins over
	// both. NoInheritedSettings preserves omitted Settings at this layer.
	// It does not change turns already started.
	Agent Agent
	// Steering reaches the turns this handle runs. Nil means the handle's
	// own *Inbox, which Steer and SteerItem send to. A host with its own
	// durable source sets it and sends through that source; Steer then
	// works only when the source is an *Inbox.
	Steering Steering
	// Compactor, when set, runs a compaction turn before a new turn when
	// the projected history has grown past its threshold. Package
	// compaction implements it. Every handle honors compaction turns
	// already recorded, with or without one.
	Compactor Compactor
	// Exclusive promises that this handle is the only driver of the session
	// and that earlier drivers have exited, so a turn this handle is not
	// running has no worker. It proves nothing about effects they started:
	// an unsettled uncertain call still makes a turn busy. A last turn that
	// cannot be read is skipped with a warning rather than blocking the
	// session.
	Exclusive bool

	// mu guards everything below. Run holds it while choosing the turn and
	// building the command, never while a turn runs.
	mu sync.Mutex
	// inbox is the handle's own steering source when Steering is nil.
	inbox *Inbox
	// queue holds input for the next turn, in order (see Enqueue).
	queue []*queuedItem
	// driving is the turn a Start, Continue, or queue turn of this handle
	// is running, which Steer reaches; empty when none runs.
	driving string
	// compacting is the compaction turn this handle is running, which
	// Steer refuses; empty when none runs.
	compacting string
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
	// Duplicate means the refused Start is a retry of that turn, which may
	// still be running.
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
	case e.Next == NextRunning && e.Origin == StartCompaction:
		return fmt.Sprintf(
			"dashi: turn %s is compacting the history; enqueue the input, "+
				"or wait and send again",
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
	// Queued counts the items waiting in this handle's queue, including
	// those reserved for a turn that is starting.
	Queued int
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

// Answer records each answer with Settle, then runs one Continue, so a
// host answers a waiting turn in one call; the record keeps a step per
// answer. It stops at the first Settle error. A Settle without a command
// ID gets "settle:" and its CallID, as Pending's builders derive theirs
// from the call, and the Continue's command ID derives from the answers',
// so a retried Answer is recognized. Each run gets opts, as Run's would:
// WithOwner for a leasing host, WithObserver for a live view of the
// Continue.
func (s *Session) Answer(
	ctx context.Context,
	answers []Settle,
	opts ...RunOption,
) (Outcome, error) {
	ids := make([]string, 0, len(answers))
	for _, answer := range answers {
		if answer.CommandID == "" && answer.CallID != "" {
			// Call IDs are unique within a turn, and command IDs are
			// scoped to their turn.
			answer.CommandID = "settle:" + answer.CallID
		}
		answer = normalizeCommand(answer).(Settle)
		if out, err := s.Run(ctx, answer, opts...); err != nil {
			return out, err
		}
		ids = append(ids, answer.CommandID)
	}
	next := Continue{}
	if len(ids) > 0 {
		next.CommandID = "answer:" + strings.Join(ids, ",")
	}
	return s.Run(ctx, next, opts...)
}

// Run applies one command to the session. A Start opens the next turn,
// after the idempotency check and the last-turn rules: it returns the
// earlier result of a retried command ID, refuses with *TurnBusyError while
// the last turn is busy, and otherwise closes a stopped last turn and
// starts. A last turn that cannot be read is an error, except under
// Exclusive: then the Start leaves it out of the history with a
// WarningTurnUnreadable and starts the turn after it. Queued user input (see Enqueue) goes ahead of the Start's Input.
// Any other command (Continue, Settle, Deliver, Cancel) goes to the last
// turn. The session sets Start.History and Continue.History to the
// projected history of the turns before the target turn, replacing any the
// caller set, and gives a Start the carried agent. A Start's command ID
// must not begin with "queue:" or "compact:", which mark the session's own
// turns, and a Start cannot claim origin StartCompaction or set
// Compaction. Every turn gets the handle's steering source (see Steering).
//
// When a Start or Continue ends its turn with NextNone and input is
// queued, the same call runs queue turns until the queue is empty or a
// turn stops any other way; the result is the last turn's, with the
// session warnings of the earlier ones. With a Compactor, a compaction
// turn may run first, before a Start, under the Start's Budget, or before
// a queue turn: the next turn's outcome carries its session warnings, and
// WarningCompactionFailed when it fails.
//
// The outcome and error mean what Runner.Run's do, and
// Outcome.SessionWarnings adds the session's own warnings.
func (s *Session) Run(
	ctx context.Context,
	c Command,
	opts ...RunOption,
) (Outcome, error) {
	if err := checkCommand(c); err != nil {
		return Outcome{}, err
	}
	return (&call{s: s, opts: opts}).run(ctx, c)
}

// checkCommand refuses a nil command, a Start with a reserved key, and a
// Start that claims to be a compaction turn: the session's projection
// trusts compaction turns, so only its Compactor may start them.
func checkCommand(c Command) error {
	switch c := c.(type) {
	case nil:
		return errors.New("dashi: command is required")
	case Start:
		if internalKey(c.CommandID) {
			return fmt.Errorf(
				"dashi: command ID %q uses a prefix reserved for the session's queue and compaction turns",
				c.CommandID,
			)
		}
		if c.Origin == StartCompaction || c.Compaction != nil {
			return errors.New(
				"dashi: compaction turns are started by the session's Compactor",
			)
		}
	}
	return nil
}

// Open reads the session's last turn and reports its state. It never runs
// anything. An ID with no turns gives ErrSessionNotFound, unless New is
// set; with New, a session that has turns gives ErrSessionExists. An empty
// ID is a new session.
func (s *Session) Open(ctx context.Context) (SessionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queued := len(s.queue)
	if s.ID == "" {
		return SessionState{Next: NextNone, Queued: queued}, nil
	}
	// A reservation that cannot be settled holds only the queue back.
	_ = s.settleReserved(ctx)
	last, err := s.findLast(ctx)
	if err != nil {
		return SessionState{}, err
	}
	if err := s.checkExists(last); err != nil {
		return SessionState{}, err
	}
	queued = len(s.queue)
	if last == 0 {
		return SessionState{Next: NextNone, Queued: queued}, nil
	}
	t, err := s.load(ctx, last)
	if err != nil {
		return SessionState{}, err
	}
	view := t.view
	return SessionState{
		Turns:  last,
		Last:   &view,
		Next:   s.refine(ctx, t).next,
		Queued: queued,
	}, nil
}

// commandPlan is a command routed to the last turn.
type commandPlan struct {
	n        int
	turn     Turn
	warnings []Warning
	runner   Runner
	drives   bool
}

func (s *Session) planCommand(
	ctx context.Context,
	c *Command,
) (commandPlan, error) {
	if s.ID == "" {
		return commandPlan{}, ErrSessionNotFound
	}
	// A reservation that cannot be settled holds only the queue back.
	_ = s.settleReserved(ctx)
	last, err := s.findLast(ctx)
	if err != nil {
		return commandPlan{}, err
	}
	if err := s.checkExists(last); err != nil {
		return commandPlan{}, err
	}
	if last == 0 {
		return commandPlan{}, fmt.Errorf("%w: %s", ErrSessionNotFound, s.ID)
	}
	t, err := s.load(ctx, last)
	if err != nil {
		return commandPlan{}, err
	}
	p := commandPlan{n: last, turn: t.turn, runner: s.runner()}
	if cont, ok := (*c).(Continue); ok {
		cont.History, p.warnings = s.historyFor(ctx, last, t)
		*c = cont
		p.drives = true
		if t.view.Origin == StartCompaction {
			s.compacting = t.turn.ID
		} else {
			s.driving = t.turn.ID
		}
	}
	s.running[t.turn.ID]++
	return p, nil
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

// takenKey reports whether a keyed Start found its command ID on the turn
// it chose before its turn_started step committed. The idempotency check,
// run again, tells a true retry, whose history or agent may differ, from a
// conflict, so the Start never runs as a new turn.
func takenKey(err error) bool {
	if !errors.Is(err, ErrCommandConflict) {
		return false
	}
	var ce *CommitError
	return !errors.As(err, &ce) || ce.Step == nil ||
		ce.Step.Kind == StepStarted
}

// startPlan is a chosen new turn, or the answer that replaces running one.
type startPlan struct {
	n        int
	id       string
	command  Start
	warnings []Warning
	runner   Runner
	// reserved holds the queued items the Start carries.
	reserved *reservation
	// closing holds the events of the step that closed the last turn, for
	// Stream.
	closing []Event
	// compact marks a compaction turn, which runs before the planned turn.
	compact bool
	// done is the result when no turn runs: a retried command's outcome.
	done *Outcome
}

func (s *Session) planStart(
	ctx context.Context,
	c Start,
	fresh bool,
	opts []RunOption,
	stream bool,
	compacted bool,
) (startPlan, error) {
	if s.ID == "" {
		s.ID = NewID()
		s.created = true
	}
	// A reservation that cannot be settled holds only the queue back; its
	// items are not free, so this Start does not take them.
	_ = s.settleReserved(ctx)
	last, err := s.findLast(ctx)
	if err != nil {
		return startPlan{}, err
	}
	if !fresh {
		out, err := s.duplicate(ctx, c, last)
		if out != nil || err != nil {
			return startPlan{done: out}, err
		}
	}
	if err := s.checkExists(last); err != nil {
		return startPlan{}, err
	}
	// This handle now makes the session, so New no longer applies to it,
	// even while its first turn runs.
	s.created = true
	var p startPlan
	upTo := last // the turns the history holds
	closed := false
	if last > 0 {
		t, skip, err := s.readLast(ctx, last)
		if err != nil {
			return p, err
		}
		if skip != nil {
			p.warnings = append(p.warnings, *skip)
			upTo = last - 1
		} else {
			var events []Event
			closed, events, err = s.closeLast(ctx, last, t, opts, stream)
			p.closing = events
			if err != nil {
				return p, err
			}
			if closed {
				p.warnings = append(p.warnings, closedWarning(t))
			}
		}
	}
	history, unreadable := s.history(ctx, upTo)
	// A turn this Start closed did not complete as its caller meant, even
	// when closing completed it, so no compaction follows it.
	if upTo == last && !compacted && !closed {
		cp, warnings := s.planCompaction(ctx, last, history, c.Budget)
		if cp != nil {
			return *cp, nil
		}
		p.warnings = append(p.warnings, warnings...)
	}
	p.warnings = append(p.warnings, unreadable...)
	c.History = history
	c.Agent = s.agentFor(ctx, c.Agent, last)
	// Queued user input goes first, since the user typed it first. Other
	// items run as a queue turn afterwards.
	if items := s.free(fromUser); len(items) > 0 {
		c.Input = append(itemMessages(items), c.Input...)
		p.reserved = s.reserve(items, c.CommandID, last+1)
	}
	p.n, p.id, p.command, p.runner = last+1, TurnID(s.ID, last+1), c, s.runner()
	s.running[p.id]++
	s.driving = p.id
	return p, nil
}

// closedWarning is the warning for a last turn a new turn closed: a
// WarningCompactionFailed for a compaction turn, else a WarningTurnClosed.
func closedWarning(t *sessionTurn) Warning {
	if t.view.Origin == StartCompaction {
		return compactionFailed(fmt.Sprintf(
			"compaction turn %s stopped %s and was closed",
			t.turn.ID,
			t.view.Stop.Kind,
		))
	}
	return Warning{
		Code:    WarningTurnClosed,
		Message: fmt.Sprintf("turn %s was closed: %s", t.turn.ID, CloseReason),
	}
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

// duplicate applies the idempotency check. It scans back from the last
// turn through the newest turn a caller's Start began; queue and
// compaction turns never end the window, since they run after the caller's
// turn. A match with the same input returns the turn's current outcome,
// as the first call returned it, unless the turn may still be running
// (NextRunning or NextResume): then waiting helps, and it returns a
// duplicate busy error. A match with other input conflicts.
func (s *Session) duplicate(
	ctx context.Context,
	c Start,
	last int,
) (*Outcome, error) {
	for n := last; n >= 1; n-- {
		t, err := s.load(ctx, n)
		if err != nil {
			// The window cannot be checked, and a missed key would run
			// the command again.
			return nil, err
		}
		start := t.turn.Steps[0]
		if start.CommandID == c.CommandID {
			if !sameInputTail(start.Started.Input, c.Input) {
				return nil, fmt.Errorf(
					"%w: command %s started turn %s with other input",
					ErrCommandConflict,
					c.CommandID,
					t.turn.ID,
				)
			}
			r := s.refine(ctx, t)
			if r.next == NextRunning || r.next == NextResume {
				return nil, &TurnBusyError{
					TurnID:    t.turn.ID,
					Next:      r.next,
					Origin:    t.view.Origin,
					Duplicate: true,
				}
			}
			return &Outcome{Turn: cloneTurn(t.turn), TurnView: t.view}, nil
		}
		if !internalKey(start.CommandID) {
			return nil, nil
		}
	}
	return nil, nil
}

// readLast loads the last turn before a new turn starts. Without Exclusive,
// a last turn that cannot be read is an error: its worker may still run it.
// Under Exclusive it has no worker, so the session skips it: t is nil, and
// skip is the turn_unreadable warning for the new turn's outcome. The
// caller holds mu.
func (s *Session) readLast(
	ctx context.Context,
	last int,
) (t *sessionTurn, skip *Warning, err error) {
	t, err = s.load(ctx, last)
	if err == nil || !s.Exclusive || ctx.Err() != nil {
		return t, nil, err
	}
	return nil, &Warning{
		Code: WarningTurnUnreadable,
		Message: fmt.Sprintf(
			"turn %s was left out of the history, and the next turn starts after it: %v",
			TurnID(s.ID, last),
			err,
		),
	}, nil
}

// closeLast applies the last-turn rules to a new Start: it returns an error
// when the turn is busy, closes a stopped turn that has not ended, and
// reports whether it closed one. Steers the closed turn never recorded move
// to the queue. With stream, it returns the closing run's events.
func (s *Session) closeLast(
	ctx context.Context,
	last int,
	t *sessionTurn,
	opts []RunOption,
	stream bool,
) (bool, []Event, error) {
	r := s.refine(ctx, t)
	if busy := r.busy(t, s.Exclusive); busy != nil {
		return false, nil, busy
	}
	if r.next == NextNone {
		return false, nil, nil
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
		cont.History, _ = s.historyFor(ctx, last, t)
		closing = cont
	}
	var out Outcome
	var events []Event
	var err error
	if stream {
		// The consumer gets these events after the lock is released.
		out, events, err = collectRun(ctx, s.runner(), t.turn, closing, opts)
	} else {
		out, err = s.runner().Run(ctx, t.turn, closing, opts...)
	}
	s.store(last, out.Turn)
	if err != nil {
		return false, events, err
	}
	if out.Next() != NextNone {
		return false, events, &TurnBusyError{
			TurnID: id,
			Next:   out.Next(),
			Origin: out.Origin,
		}
	}
	s.moveLeftovers(out.Turn)
	return true, events, nil
}

// finish records a command's result in the cache, settles the queued items
// it carried (started: the run reported its turn_started step), moves
// missed steers to the queue when its turn ended, and adds the session's
// warnings to its outcome. Clearing the running turn and
// moving missed steers happen under one lock, so a steer is either in the
// inbox when they move or goes to the queue itself.
func (s *Session) finish(
	ctx context.Context,
	n int,
	id string,
	out Outcome,
	err error,
	warnings []Warning,
	reserved *reservation,
	started bool,
) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[id]--; s.running[id] <= 0 {
		delete(s.running, id)
	}
	if s.driving == id && s.running[id] == 0 {
		s.driving = ""
	}
	if s.compacting == id && s.running[id] == 0 {
		s.compacting = ""
	}
	if reserved != nil {
		err = s.settle(ctx, reserved, started, err)
	}
	if out.Turn.ID == id && len(out.Turn.Steps) > 0 {
		s.store(n, out.Turn)
		s.created = true
	}
	if err == nil && out.Turn.ID == id && out.Next() == NextNone &&
		out.Origin != StartCompaction {
		s.moveLeftovers(out.Turn)
	}
	out.SessionWarnings = append(warnings, out.SessionWarnings...)
	out.SessionWarnings = append(out.SessionWarnings, s.queueBlocked()...)
	return s.withMemoryWarningLocked(out), err
}

// runner returns the Runner with the recorder the session uses and the
// session ID as its fallback prompt cache key. The caller holds mu.
func (s *Session) runner() Runner {
	r := s.Runner
	r.Recorder = s.recorder()
	r.cacheKey = s.ID
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

func (s *Session) withMemoryWarning(out Outcome) Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withMemoryWarningLocked(out)
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

// sameInputTail reports whether recorded ends with input by JSON meaning.
// Queued user items the session put ahead of a caller's input come first.
func sameInputTail(recorded, input []llm.Message) bool {
	if len(input) > len(recorded) {
		return false
	}
	return sameJSONValue(recorded[len(recorded)-len(input):], input)
}

// internalKey reports whether a Start's command ID marks one of the
// session's own turns, a queue or compaction turn. Run refuses these keys
// from callers, so the idempotency window can trust them.
func internalKey(id string) bool {
	return strings.HasPrefix(id, "queue:") || strings.HasPrefix(id, "compact:")
}
