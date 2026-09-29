package dashi

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/deepnoodle-ai/dashi/llm"
)

// TurnID returns the ID of turn n (from 1) of a session: the session ID, a
// dash, and n. Session IDs may contain dashes, so a reader parses the
// number from the right.
func TurnID(sessionID string, n int) string {
	return sessionID + "-" + strconv.Itoa(n)
}

// SessionIndex is an optional Recorder capability for sessions. Without it,
// a session finds its last turn by probing turn IDs with StepReader or
// Loader, in about 2·log2(n) reads. A host that deletes turns must
// implement it, since probing needs turns numbered from 1 without gaps.
type SessionIndex interface {
	// LastTurn returns the highest turn number recorded for the session,
	// or 0 when it has none.
	LastTurn(ctx context.Context, sessionID string) (int, error)
}

// Liveness says whether any worker may still act on a turn.
type Liveness string

const (
	// LivenessUnknown means there is no proof either way, for example an
	// expired lease: its worker may still be inside a provider or tool call.
	LivenessUnknown Liveness = "unknown"
	// LivenessLive means a worker holds an unexpired claim on the turn.
	LivenessLive Liveness = "live"
	// LivenessStopped means the host knows no worker still acts on the
	// turn: its process exited, or host fencing guarantees it. It says
	// nothing about effects the worker started.
	LivenessStopped Liveness = "stopped"
)

// TurnLiveness is an optional Recorder capability for hosts that lease
// turns. A session asks it about a last turn with no recorded stop: live
// refines NextResume to NextRunning, and stopped lets a new Start close the
// turn. An error counts as unknown, which keeps the turn busy.
type TurnLiveness interface {
	Liveness(ctx context.Context, turnID string) (Liveness, error)
}

// sessionTurn is a loaded turn with its view and, once built, its
// projection. A turn that has ended never changes its projection, so the
// cached projection is final.
type sessionTurn struct {
	turn      Turn
	view      TurnView
	messages  []llm.Message
	projected bool
}

func (t *sessionTurn) ended() bool {
	return t.view.Stop.Terminal
}

// refined is a turn's Next with the liveness that refined it.
type refined struct {
	next     Next
	liveness Liveness
}

// refine reads a turn's Next and refines NextResume with liveness: this
// handle running the turn, or the recorder's TurnLiveness. The caller
// holds mu.
func (s *Session) refine(ctx context.Context, t *sessionTurn) refined {
	next := t.view.Next()
	switch {
	case next == NextNone:
		return refined{next: next, liveness: LivenessUnknown}
	case s.running[t.turn.ID] > 0:
		return refined{next: NextRunning, liveness: LivenessLive}
	case next != NextResume:
		return refined{next: next, liveness: LivenessUnknown}
	}
	l := LivenessUnknown
	if tl, ok := s.recorder().(TurnLiveness); ok {
		v, err := tl.Liveness(ctx, t.turn.ID)
		if err == nil && (v == LivenessLive || v == LivenessStopped) {
			l = v
		}
	}
	if l == LivenessLive {
		next = NextRunning
	}
	return refined{next: next, liveness: l}
}

// busy applies the last-turn rules: it returns the error for a turn a new
// Start must not close, or nil for a turn that has ended or may be closed.
func (r refined) busy(t *sessionTurn, exclusive bool) *TurnBusyError {
	if r.next == NextNone {
		return nil
	}
	e := &TurnBusyError{TurnID: t.turn.ID, Next: r.next, Origin: t.view.Origin}
	// A stopped worker is not a stopped effect, so an uncertain call keeps
	// the turn busy whatever its liveness, even under Exclusive.
	if e.CallID = uncertainCall(t); e.CallID != "" {
		return e
	}
	switch r.next {
	case NextRunning, NextAnswer:
		return e
	case NextResume:
		if r.liveness == LivenessStopped || exclusive {
			return nil
		}
		return e
	}
	return nil
}

// uncertainCall returns the first unsettled call whose effect may still
// happen or may have happened: a started local call that is neither
// read-only nor idempotent, one already marked uncertain, or a model
// attempt that offered a provider-hosted tool unsafe to repeat.
func uncertainCall(t *sessionTurn) string {
	for _, p := range t.view.Pending {
		switch {
		case p.Kind == PendingTool && p.Tool != nil &&
			(p.Tool.Status == ToolUncertain ||
				p.Tool.Status == ToolStarted && !safeRepeat(p.Tool.Definition)):
			return p.ID
		case p.Kind == PendingModel && offeredUnsafeHosted(t.turn, p.ID):
			return p.ID
		}
	}
	return ""
}

func offeredUnsafeHosted(turn Turn, attemptID string) bool {
	for _, step := range turn.Steps {
		if step.Kind == StepModelIntent && step.Model != nil &&
			step.Model.AttemptID == attemptID {
			return unsafeHosted(step.Model.Tools)
		}
	}
	return false
}

// findLast returns the session's last turn number: from a SessionIndex, by
// probing, or, for a recorder that can read neither, the turns this handle
// ran. The caller holds mu.
func (s *Session) findLast(ctx context.Context) (int, error) {
	s.ready()
	rec := s.recorder()
	if idx, ok := rec.(SessionIndex); ok {
		n, err := idx.LastTurn(ctx, s.ID)
		if err != nil {
			return 0, err
		}
		s.last = max(s.last, n)
		return n, nil
	}
	_, reads := rec.(StepReader)
	_, loads := rec.(Loader)
	if !reads && !loads {
		return s.last, nil
	}
	n, err := s.probe(ctx, s.last)
	if err != nil {
		return 0, err
	}
	s.last = n
	return n, nil
}

// probe finds the last turn after base, a turn known to exist (or 0), by
// doubling the distance from base until a turn is missing, then searching
// between the last turn found and it. A session of n turns takes about
// 2·log2(n) reads from a fresh handle, and one read when no turn follows
// base.
func (s *Session) probe(ctx context.Context, base int) (int, error) {
	lo, hi := base, 0
	for step := 1; hi == 0; step *= 2 {
		if step > maxProbe {
			return 0, fmt.Errorf(
				"dashi: session %s: no missing turn after %d; the recorder "+
					"reports turns that were never started",
				s.ID,
				base+maxProbe,
			)
		}
		ok, err := s.exists(ctx, base+step)
		if err != nil {
			return 0, err
		}
		if ok {
			lo = base + step
		} else {
			hi = base + step
		}
	}
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		ok, err := s.exists(ctx, mid)
		if err != nil {
			return 0, err
		}
		if ok {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, nil
}

// maxProbe bounds the search, so a recorder that reports steps for turns
// never started fails instead of probing forever.
const maxProbe = 1 << 30

// exists reads one step of turn n, or loads it: a turn with no steps is
// missing. A step of another turn is an error, not a turn.
func (s *Session) exists(ctx context.Context, n int) (bool, error) {
	id := TurnID(s.ID, n)
	rec := s.recorder()
	var steps []Step
	var err error
	if r, ok := rec.(StepReader); ok {
		steps, err = r.StepsAfter(ctx, id, 0, 1)
	} else {
		var t Turn
		t, err = rec.(Loader).Load(ctx, id)
		steps = t.Steps
	}
	switch {
	case errors.Is(err, ErrTurnNotFound):
		return false, nil
	case err != nil:
		return false, err
	case len(steps) > 0 && steps[0].TurnID != id:
		return false, fmt.Errorf(
			"dashi: the recorder returned a step of turn %q for turn %q",
			steps[0].TurnID,
			id,
		)
	}
	return len(steps) > 0, nil
}

// load returns turn n. An ended turn comes from the cache; any other is
// reloaded, since another worker may have moved it on. A recorder that
// cannot load serves only the turns this handle ran. The caller holds mu.
func (s *Session) load(ctx context.Context, n int) (*sessionTurn, error) {
	s.ready()
	if t := s.turns[n]; t != nil && t.ended() {
		return t, nil
	}
	id := TurnID(s.ID, n)
	loader, ok := s.recorder().(Loader)
	if !ok {
		if t := s.turns[n]; t != nil {
			return t, nil
		}
		return nil, fmt.Errorf("dashi: turn %s: the recorder cannot load turns", id)
	}
	turn, err := loader.Load(ctx, id)
	if err == nil && len(turn.Steps) == 0 {
		err = ErrTurnNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("dashi: turn %s: %w", id, err)
	}
	if err := s.store(n, turn); err != nil {
		return nil, fmt.Errorf("dashi: turn %s: %w", id, err)
	}
	return s.turns[n], nil
}

// store caches a turn's record. The caller holds mu.
func (s *Session) store(n int, turn Turn) error {
	s.ready()
	if len(turn.Steps) == 0 {
		return nil
	}
	view, err := Replay(turn)
	if err != nil {
		delete(s.turns, n)
		return err
	}
	s.turns[n] = &sessionTurn{turn: turn, view: view}
	s.last = max(s.last, n)
	return nil
}

// history returns the projected history of turns 1 through n. A turn that
// fails to load or project is skipped with a WarningTurnUnreadable, as a
// history that loses one old turn is still worth sending. The caller holds
// mu.
func (s *Session) history(ctx context.Context, n int) ([]llm.Message, []Warning) {
	var out []llm.Message
	var warnings []Warning
	for i := 1; i <= n; i++ {
		msgs, err := s.projection(ctx, i)
		if err != nil {
			warnings = append(warnings, Warning{
				Code: WarningTurnUnreadable,
				Message: fmt.Sprintf(
					"turn %s was left out of the history: %v",
					TurnID(s.ID, i),
					err,
				),
			})
			continue
		}
		out = append(out, msgs...)
	}
	return out, warnings
}

// projection returns turn n's own messages. The caller holds mu.
func (s *Session) projection(ctx context.Context, n int) ([]llm.Message, error) {
	t, err := s.load(ctx, n)
	if err != nil {
		return nil, err
	}
	if t.projected {
		return t.messages, nil
	}
	msgs, err := Project(t.turn)
	if err != nil {
		return nil, err
	}
	if t.ended() {
		t.messages, t.projected = msgs, true
	}
	return msgs, nil
}

// ready makes the handle's maps. The caller holds mu.
func (s *Session) ready() {
	if s.turns == nil {
		s.turns = map[int]*sessionTurn{}
		s.running = map[string]int{}
	}
}
