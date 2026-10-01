package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/internal/canonjson"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Session drives one conversation of many turns: a session handle. It sits
// on dashi.Runner and uses only the engine's public contract, as http.Get
// sits on Client.Do. The session keeps no record of its own: turn n of
// session S is the turn S-n (TurnID), and the turn records are the only
// truth. A handle holds settings and a cache, so a server can build one per
// request, and many handles in many processes can drive one session.
//
// The zero value works once it has a client:
//
//	s := &session.Session{Runner: dashi.Runner{Client: client}}
//	out, err := s.Send(ctx, "What's the weather in Tokyo?")
//	out, err = s.Send(ctx, "And in Fahrenheit?")
//
// Each new turn gets the projected history of the turns before it, and its
// agent carries forward from the last user turn field by field. A Session
// must not be copied after first use.
type Session struct {
	// ID names the session. Empty means a new session with a generated ID
	// (dashi.NewID), readable from ID after the first Start.
	ID string
	// New creates the session: the first Start makes turn 1, and a session
	// that already has turns fails with ErrExists. Without New, an ID with
	// no turns fails with ErrNotFound, so a mistyped ID never starts an
	// empty session.
	New bool
	// Runner supplies services. A nil Recorder becomes one in-memory
	// recorder that the handle keeps across calls, and the first outcome
	// warns WarningMemoryOnly. The session sets Runner.CacheKey to its ID.
	Runner dashi.Runner
	// Agent overrides fields of the carried agent for new turns: a nonzero
	// field here wins over the last user turn's, and Start.Agent wins over
	// both. NoInheritedSettings preserves omitted Settings at this layer.
	// It does not change turns already started.
	Agent dashi.Agent
	// Steering reaches the turns this handle runs. Nil means the handle's
	// own *dashi.Inbox, which Steer and SteerItem send to. A host with its
	// own durable source sets it and sends through that source; Steer then
	// works only when the source is a *dashi.Inbox.
	Steering dashi.Steering
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
	inbox *dashi.Inbox
	// queue holds input for the next turn, in order (see Enqueue).
	queue []*queuedItem
	// driving is the turn a Start, Continue, or queue turn of this handle
	// is running, which Steer reaches; empty when none runs.
	driving string
	// compacting is the compaction turn this handle is running, which
	// Steer refuses; empty when none runs.
	compacting string
	// memory is the recorder used when Runner.Recorder is nil.
	memory *dashi.MemoryRecorder
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

// A session's own warnings. No turn records them; they arrive on
// dashi.RunResult.SessionWarnings, beside the dashi.WarningTurnUnreadable and
// dashi.WarningCompactionMismatch of the session's projected history.
const (
	// WarningTurnClosed means a new Start closed the session's last turn,
	// which had stopped without ending, with Cancel and CloseReason.
	WarningTurnClosed dashi.WarningCode = "turn_closed"
	// WarningMemoryOnly means the session is recorded only in the handle's
	// in-memory recorder and ends with the handle.
	WarningMemoryOnly dashi.WarningCode = "memory_only"
	// WarningQueueBlocked means queued items wait because the session
	// cannot read the turn that may have taken some of them. New commands
	// still run; RunQueue returns the error until the turn can be read.
	WarningQueueBlocked dashi.WarningCode = "queue_blocked"
	// WarningCompactionFailed means a compaction turn did not complete with
	// an answer, or could not start. The session closed it, kept the
	// previous projected history, and ran the next turn.
	WarningCompactionFailed dashi.WarningCode = "compaction_failed"
)

var (
	// ErrTurnBusy matches a *TurnBusyError.
	ErrTurnBusy = errors.New("session: turn is busy")
	// ErrNotFound reports a session ID with no turns, on a handle without
	// New, or a command other than Start on a session with no turns.
	ErrNotFound = errors.New("session: not found")
	// ErrExists reports New on a session that already has turns.
	ErrExists = errors.New("session: already exists")
)

// TurnBusyError refuses a Start because the session's last turn is busy:
// live, possibly live, waiting on the host, or holding an unsettled
// uncertain call. The message names the way out. It encodes as JSON, so a
// server can return it as a 409 body.
type TurnBusyError struct {
	TurnID string `json:"turn_id"`
	// Next is the busy turn's next action, refined with liveness.
	Next dashi.Next `json:"next"`
	// Origin is who started the busy turn.
	Origin dashi.StartOrigin `json:"origin,omitempty"`
	// Duplicate means the refused Start is a retry of that turn, which may
	// still be running.
	Duplicate bool `json:"duplicate,omitempty"`
	// CallID names the unsettled uncertain call that makes the turn busy
	// when no worker is known to run the turn. Resolve it after checking its
	// effect, or Cancel the turn. It is empty while the turn is running.
	CallID string `json:"call_id,omitempty"`
}

func (e *TurnBusyError) Error() string {
	switch {
	case e.Duplicate:
		return fmt.Sprintf(
			"session: turn %s already runs this command and has not ended "+
				"(next: %s); wait for it, then retry",
			e.TurnID,
			e.Next,
		)
	case e.CallID != "":
		return fmt.Sprintf(
			"session: call %s in turn %s may still be running; Resolve{} it "+
				"after checking its effect, or Cancel{} the turn to accept "+
				"the risk",
			e.CallID,
			e.TurnID,
		)
	case e.Next == dashi.NextRunning && e.Origin == dashi.StartCompaction:
		return fmt.Sprintf(
			"session: turn %s is compacting the history; enqueue the input, "+
				"or wait and send again",
			e.TurnID,
		)
	case e.Next == dashi.NextRunning:
		return fmt.Sprintf(
			"session: turn %s is running; steer it, or stop it and send again",
			e.TurnID,
		)
	case e.Next == dashi.NextRecover:
		return fmt.Sprintf(
			"session: turn %s may still be running; if its worker is lost, "+
				"take it over with Continue{} or discard it with Cancel{}",
			e.TurnID,
		)
	case e.Next == dashi.NextResolvePending:
		return fmt.Sprintf(
			"session: turn %s waits on the host; answer its pending work or "+
				"Cancel{} it",
			e.TurnID,
		)
	}
	return fmt.Sprintf("session: turn %s is busy (next: %s)", e.TurnID, e.Next)
}

// Unwrap lets errors.Is match ErrTurnBusy.
func (e *TurnBusyError) Unwrap() error {
	return ErrTurnBusy
}

// State is what Open reports about a session.
type State struct {
	// Turns is the number of the last turn; 0 for a new session.
	Turns int
	// Last is the last turn's view, or nil when Turns is 0.
	Last *dashi.TurnView
	// Next is the last turn's next action, refined with liveness: NextRunning
	// when this handle runs it or WorkerChecker reports it live. It is
	// NextNone for a new session.
	Next dashi.Next
	// Queued counts the items waiting in this handle's queue, including
	// those reserved for a turn that is starting.
	Queued int
}

// Send runs dashi.Say(text) as a new turn. It is exactly
// Run(ctx, dashi.Say(text)).
func (s *Session) Send(
	ctx context.Context,
	text string,
	opts ...dashi.RunOption,
) (dashi.RunResult, error) {
	return s.Run(ctx, dashi.Say(text), opts...)
}

// Resolve records each answer with Resolve, then runs one Continue, so a
// host answers a waiting turn in one call; the record keeps a step per
// answer. It stops at the first Resolve error. A Resolve without a command
// ID gets "resolve:" and its CallID, as Pending's builders derive theirs
// from the call, and the Continue's command ID derives from the answers',
// so a retried Resolve is recognized. Each run gets opts, as Run's would:
// WithOwner for a leasing host, WithObserver for a live view of the
// Continue.
func (s *Session) Resolve(
	ctx context.Context,
	answers []dashi.Resolve,
	opts ...dashi.RunOption,
) (dashi.RunResult, error) {
	ids := make([]string, 0, len(answers))
	for _, answer := range answers {
		if answer.CommandID == "" && answer.CallID != "" {
			// Call IDs are unique within a turn, and command IDs are
			// scoped to their turn.
			answer.CommandID = "resolve:" + answer.CallID
		}
		if answer.CommandID == "" {
			// The Continue's command ID derives from this one.
			answer.CommandID = dashi.NewID()
		}
		if out, err := s.Run(ctx, answer, opts...); err != nil {
			return out, err
		}
		ids = append(ids, answer.CommandID)
	}
	next := dashi.Continue{}
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
// dashi.WarningTurnUnreadable and starts the turn after it. Queued user
// input (see Enqueue) goes ahead of the Start's Input. Any other command
// (Continue, Resolve, AddMessages, Cancel) goes to the last turn. The session sets Start.History and Continue.History to the
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
// The outcome and error mean what dashi.Runner.Run's do, and
// RunResult.SessionWarnings adds the session's own warnings.
func (s *Session) Run(
	ctx context.Context,
	c dashi.Command,
	opts ...dashi.RunOption,
) (dashi.RunResult, error) {
	if err := checkCommand(c); err != nil {
		return dashi.RunResult{}, err
	}
	return (&call{s: s, opts: opts}).run(ctx, c)
}

// checkCommand refuses a nil command, a Start with a reserved key, and a
// Start that claims to be a compaction turn: the session's projection
// trusts compaction turns, so only its Compactor may start them.
func checkCommand(c dashi.Command) error {
	switch c := c.(type) {
	case nil:
		return errors.New("session: command is required")
	case dashi.Start:
		if internalKey(c.CommandID) {
			return fmt.Errorf(
				"session: command ID %q uses a prefix reserved for the session's queue and compaction turns",
				c.CommandID,
			)
		}
		if c.Origin == dashi.StartCompaction || c.Compaction != nil {
			return errors.New(
				"session: compaction turns are started by the session's Compactor",
			)
		}
	}
	return nil
}

// Open reads the session's last turn and reports its state. It never runs
// anything. An ID with no turns gives ErrNotFound, unless New is
// set; with New, a session that has turns gives ErrExists. An empty
// ID is a new session.
func (s *Session) Open(ctx context.Context) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queued := len(s.queue)
	if s.ID == "" {
		return State{Next: dashi.NextNone, Queued: queued}, nil
	}
	// A reservation that cannot be settled holds only the queue back.
	_ = s.settleReserved(ctx)
	last, err := s.findLast(ctx)
	if err != nil {
		return State{}, err
	}
	if err := s.checkExists(last); err != nil {
		return State{}, err
	}
	queued = len(s.queue)
	if last == 0 {
		return State{Next: dashi.NextNone, Queued: queued}, nil
	}
	t, err := s.load(ctx, last)
	if err != nil {
		return State{}, err
	}
	view := t.view
	return State{
		Turns:  last,
		Last:   &view,
		Next:   s.refine(ctx, t).next,
		Queued: queued,
	}, nil
}

// commandPlan is a command routed to the last turn.
type commandPlan struct {
	n        int
	turn     dashi.Turn
	warnings []dashi.Warning
	runner   dashi.Runner
	drives   bool
}

func (s *Session) planCommand(
	ctx context.Context,
	c *dashi.Command,
) (commandPlan, error) {
	if s.ID == "" {
		return commandPlan{}, ErrNotFound
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
		return commandPlan{}, fmt.Errorf("%w: %s", ErrNotFound, s.ID)
	}
	t, err := s.load(ctx, last)
	if err != nil {
		return commandPlan{}, err
	}
	p := commandPlan{n: last, turn: t.turn, runner: s.runner()}
	if cont, ok := (*c).(dashi.Continue); ok {
		cont.History, p.warnings = s.historyFor(ctx, last, t)
		*c = cont
		p.drives = true
		if t.view.Origin == dashi.StartCompaction {
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
	var ce *dashi.CommitError
	return errors.As(err, &ce) && ce.Step != nil &&
		ce.Step.Kind == dashi.StepTurnStarted &&
		errors.Is(ce.Err, dashi.ErrStaleTurn)
}

// takenKey reports whether a keyed Start found its command ID on the turn
// it chose before its turn_started step committed. The idempotency check,
// run again, tells a true retry, whose history or agent may differ, from a
// conflict, so the Start never runs as a new turn.
func takenKey(err error) bool {
	if !errors.Is(err, dashi.ErrCommandIDReused) {
		return false
	}
	var ce *dashi.CommitError
	return !errors.As(err, &ce) || ce.Step == nil ||
		ce.Step.Kind == dashi.StepTurnStarted
}

// startPlan is a chosen new turn, or the answer that replaces running one.
type startPlan struct {
	n        int
	id       string
	command  dashi.Start
	warnings []dashi.Warning
	runner   dashi.Runner
	// reserved holds the queued items the Start carries.
	reserved *reservation
	// closing holds the events of the step that closed the last turn, for
	// Stream.
	closing []dashi.Event
	// compact marks a compaction turn, which runs before the planned turn.
	compact bool
	// done is the result when no turn runs: a retried command's outcome.
	done *dashi.RunResult
}

func (s *Session) planStart(
	ctx context.Context,
	c dashi.Start,
	fresh bool,
	opts []dashi.RunOption,
	stream bool,
	compacted bool,
) (startPlan, error) {
	if s.ID == "" {
		s.ID = dashi.NewID()
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
			var events []dashi.Event
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
func closedWarning(t *sessionTurn) dashi.Warning {
	if t.view.Origin == dashi.StartCompaction {
		return compactionFailed(fmt.Sprintf(
			"compaction turn %s stopped %s and was closed",
			t.turn.ID,
			t.view.Stop.Kind,
		))
	}
	return dashi.Warning{
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
		return fmt.Errorf("%w: %s", ErrNotFound, s.ID)
	case last > 0 && s.New:
		return fmt.Errorf("%w: %s", ErrExists, s.ID)
	}
	s.created = last > 0
	return nil
}

// duplicate applies the idempotency check. It scans back from the last
// turn through the newest turn a caller's Start began; queue and
// compaction turns never end the window, since they run after the caller's
// turn. A match with the same input returns the turn's current outcome,
// as the first call returned it, unless the turn may still be running
// (NextRunning or NextRecover): then waiting helps, and it returns a
// duplicate busy error. A match with other input conflicts.
func (s *Session) duplicate(
	ctx context.Context,
	c dashi.Start,
	last int,
) (*dashi.RunResult, error) {
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
					dashi.ErrCommandIDReused,
					c.CommandID,
					t.turn.ID,
				)
			}
			r := s.refine(ctx, t)
			if r.next == dashi.NextRunning || r.next == dashi.NextRecover {
				return nil, &TurnBusyError{
					TurnID:    t.turn.ID,
					Next:      r.next,
					Origin:    t.view.Origin,
					Duplicate: true,
				}
			}
			return &dashi.RunResult{Turn: t.turn.Clone(), TurnView: t.view}, nil
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
) (t *sessionTurn, skip *dashi.Warning, err error) {
	t, err = s.load(ctx, last)
	if err == nil || !s.Exclusive || ctx.Err() != nil {
		return t, nil, err
	}
	return nil, &dashi.Warning{
		Code: dashi.WarningTurnUnreadable,
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
	opts []dashi.RunOption,
	stream bool,
) (bool, []dashi.Event, error) {
	r := s.refine(ctx, t)
	if busy := r.busy(t, s.Exclusive); busy != nil {
		return false, nil, busy
	}
	if r.next == dashi.NextNone {
		return false, nil, nil
	}
	id := t.turn.ID
	var closing dashi.Command = dashi.Cancel{CommandID: "close:" + id, Reason: CloseReason}
	if t.view.Output != nil {
		// The engine refuses Cancel once structured output is recorded, so
		// Continue completes the turn without a model call, raising a spent
		// model-attempt limit so that it can.
		cont := dashi.Continue{CommandID: "close:" + id}
		if l := t.view.Stop.Limit; l != nil && l.Kind == dashi.LimitModelCalls {
			cont.ModelCalls = int(l.Limit) + 1
		}
		cont.History, _ = s.historyFor(ctx, last, t)
		closing = cont
	}
	var out dashi.RunResult
	var events []dashi.Event
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
	if out.Next() != dashi.NextNone {
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
	out dashi.RunResult,
	err error,
	warnings []dashi.Warning,
	reserved *reservation,
	started bool,
) (dashi.RunResult, error) {
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
	if err == nil && out.Turn.ID == id && out.Next() == dashi.NextNone &&
		out.Origin != dashi.StartCompaction {
		s.moveLeftovers(out.Turn)
	}
	out.SessionWarnings = append(warnings, out.SessionWarnings...)
	out.SessionWarnings = append(out.SessionWarnings, s.queueBlocked()...)
	return s.withMemoryWarningLocked(out), err
}

// runner returns the Runner with the recorder the session uses and the
// session ID as its fallback prompt cache key. The caller holds mu.
func (s *Session) runner() dashi.Runner {
	r := s.Runner
	r.Recorder = s.recorder()
	r.CacheKey = s.ID
	return r
}

// recorder returns Runner.Recorder, or the handle's in-memory recorder.
// The caller holds mu.
func (s *Session) recorder() dashi.Recorder {
	if s.Runner.Recorder != nil {
		return s.Runner.Recorder
	}
	if s.memory == nil {
		s.memory = dashi.NewMemoryRecorder()
	}
	return s.memory
}

func (s *Session) withMemoryWarning(out dashi.RunResult) dashi.RunResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withMemoryWarningLocked(out)
}

func (s *Session) withMemoryWarningLocked(out dashi.RunResult) dashi.RunResult {
	if s.Runner.Recorder == nil && !s.warnedMemory {
		s.warnedMemory = true
		out.SessionWarnings = append(out.SessionWarnings, dashi.Warning{
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
	return canonjson.SameValue(recorded[len(recorded)-len(input):], input)
}

// internalKey reports whether a Start's command ID marks one of the
// session's own turns, a queue or compaction turn. Run refuses these keys
// from callers, so the idempotency window can trust them.
func internalKey(id string) bool {
	return strings.HasPrefix(id, "queue:") || strings.HasPrefix(id, "compact:")
}
