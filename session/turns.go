package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/internal/tooldef"
	"github.com/deepnoodle-ai/dashi/llm"
)

// TurnID returns the ID of turn n (from 1) of a session: the session ID, a
// dash, and n. Session IDs may contain dashes, so a reader parses the
// number from the right.
func TurnID(sessionID string, n int) string {
	return sessionID + "-" + strconv.Itoa(n)
}

// Index is an optional dashi.Recorder capability for sessions. Without it,
// a session finds its last turn by probing turn IDs with dashi.StepReader
// or dashi.Loader, in about 2·log2(n) reads. A host that deletes turns must
// implement it, since probing needs turns numbered from 1 without gaps.
type Index interface {
	// LastTurn returns the highest turn number recorded for the session,
	// or 0 when it has none.
	LastTurn(ctx context.Context, sessionID string) (int, error)
}

// WorkerStatus says whether any worker may still act on a turn.
type WorkerStatus string

const (
	// WorkerStatusUnknown means there is no proof either way, for example an
	// expired lease: its worker may still be inside a provider or tool call.
	WorkerStatusUnknown WorkerStatus = "unknown"
	// WorkerStatusRunning means a worker holds an unexpired claim on the turn.
	WorkerStatusRunning WorkerStatus = "running"
	// WorkerStatusStopped means the host knows no worker still acts on the
	// turn: its process exited, or host fencing guarantees it. It says
	// nothing about effects the worker started.
	WorkerStatusStopped WorkerStatus = "stopped"
)

// WorkerChecker is an optional dashi.Recorder capability for hosts that lease
// turns. A session asks it about a last turn with no recorded stop: live
// refines NextRecover to NextRunning, and stopped lets a new Start close the
// turn. An error counts as unknown, which keeps the turn busy.
type WorkerChecker interface {
	WorkerStatus(ctx context.Context, turnID string) (WorkerStatus, error)
}

// sessionTurn is a loaded turn with its view and, once built, its
// projection. A turn that has ended never changes its projection, so the
// cached projection is final.
type sessionTurn struct {
	turn             dashi.Turn
	view             dashi.TurnView
	messages         []llm.Message
	projected        bool
	contextMessages  []llm.Message
	contextProjected bool
	// checked is set once a compaction turn's digest was compared, and
	// mismatch holds the warning when it differed.
	checked  bool
	mismatch *dashi.Warning
}

func (t *sessionTurn) ended() bool {
	return t.view.Stop.Ended
}

// refined is a turn's Next with the liveness that refined it.
type refined struct {
	next         dashi.Next
	workerStatus WorkerStatus
}

// refine reads a turn's Next and refines NextRecover with liveness: this
// handle running the turn, or the recorder's WorkerChecker. The caller
// holds mu.
func (s *Session) refine(ctx context.Context, t *sessionTurn) refined {
	next := t.view.Next()
	switch {
	case next == dashi.NextNone:
		return refined{next: next, workerStatus: WorkerStatusUnknown}
	case s.running[t.turn.ID] > 0:
		return refined{next: dashi.NextRunning, workerStatus: WorkerStatusRunning}
	case next != dashi.NextRecover:
		return refined{next: next, workerStatus: WorkerStatusUnknown}
	}
	l := WorkerStatusUnknown
	if tl, ok := s.recorder().(WorkerChecker); ok {
		v, err := tl.WorkerStatus(ctx, t.turn.ID)
		if err == nil && (v == WorkerStatusRunning || v == WorkerStatusStopped) {
			l = v
		}
	}
	if l == WorkerStatusRunning {
		next = dashi.NextRunning
	}
	return refined{next: next, workerStatus: l}
}

// busy applies the last-turn rules: it returns the error for a turn a new
// Start must not close, or nil for a turn that has ended or may be closed.
func (r refined) busy(t *sessionTurn, exclusive bool) *TurnBusyError {
	if r.next == dashi.NextNone {
		return nil
	}
	e := &TurnBusyError{TurnID: t.turn.ID, Next: r.next, Origin: t.view.Origin}
	// A worker is known to run the turn, so its calls are in progress, not
	// uncertain: the caller waits or steers, and settles nothing.
	if r.next == dashi.NextRunning {
		return e
	}
	// A stopped worker is not a stopped effect, so an uncertain call keeps
	// the turn busy whatever its liveness, even under Exclusive.
	if e.CallID = resultUnknownCall(t); e.CallID != "" {
		return e
	}
	switch r.next {
	case dashi.NextRunning, dashi.NextResolvePending:
		return e
	case dashi.NextRecover:
		if r.workerStatus == WorkerStatusStopped || exclusive {
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
func resultUnknownCall(t *sessionTurn) string {
	for _, p := range t.view.Pending {
		switch {
		case p.Kind == dashi.PendingTool && p.Tool != nil &&
			(p.Tool.Status == dashi.ToolResultUnknown ||
				p.Tool.Status == dashi.ToolStarted && !tooldef.SafeRepeat(p.Tool.Definition)):
			return p.ID
		case p.Kind == dashi.PendingModel && offeredUnsafeProviderTool(t.turn, p.ID):
			return p.ID
		}
	}
	return ""
}

func offeredUnsafeProviderTool(turn dashi.Turn, modelCallID string) bool {
	for _, step := range turn.Steps {
		if step.Kind == dashi.StepModelCallStarted && step.Model != nil &&
			step.Model.ModelCallID == modelCallID {
			return tooldef.UnsafeProviderTool(step.Model.Tools)
		}
	}
	return false
}

// findLast returns the session's last turn number: from a Index, by
// probing, or, for a recorder that can read neither, the turns this handle
// ran. The caller holds mu.
func (s *Session) findLast(ctx context.Context) (int, error) {
	s.ready()
	rec := s.recorder()
	if idx, ok := rec.(Index); ok {
		n, err := idx.LastTurn(ctx, s.ID)
		if err != nil {
			return 0, err
		}
		s.last = max(s.last, n)
		return n, nil
	}
	_, reads := rec.(dashi.StepReader)
	_, loads := rec.(dashi.Loader)
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
				"session: %s: no missing turn after %d; the recorder "+
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
	var steps []dashi.Step
	var err error
	if r, ok := rec.(dashi.StepReader); ok {
		steps, err = r.StepsAfter(ctx, id, 0, 1)
	} else {
		var t dashi.Turn
		t, err = rec.(dashi.Loader).Load(ctx, id)
		steps = t.Steps
	}
	switch {
	case errors.Is(err, dashi.ErrTurnNotFound):
		return false, nil
	case err != nil:
		return false, err
	case len(steps) > 0 && steps[0].TurnID != id:
		return false, fmt.Errorf(
			"session: the recorder returned a step of turn %q for turn %q",
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
	loader, ok := s.recorder().(dashi.Loader)
	if !ok {
		if t := s.turns[n]; t != nil {
			return t, nil
		}
		return nil, fmt.Errorf("session: turn %s: the recorder cannot load turns", id)
	}
	turn, err := loader.Load(ctx, id)
	if err == nil && len(turn.Steps) == 0 {
		err = dashi.ErrTurnNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("session: turn %s: %w", id, err)
	}
	if err := s.store(n, turn); err != nil {
		return nil, fmt.Errorf("session: turn %s: %w", id, err)
	}
	return s.turns[n], nil
}

// store caches a turn's record. The caller holds mu.
func (s *Session) store(n int, turn dashi.Turn) error {
	s.ready()
	if len(turn.Steps) == 0 {
		return nil
	}
	view, err := turn.View()
	if err != nil {
		delete(s.turns, n)
		return err
	}
	s.turns[n] = &sessionTurn{turn: turn, view: view}
	s.last = max(s.last, n)
	return nil
}

// history returns the projected history of turns 1 through n by the rule
// of dashi.BuildHistory. It walks back from turn n, so a fresh handle
// loads turns back to the newest completed compaction turn only, and it
// keeps each ended turn's projection in the cache. A turn that fails to load
// or project is left out with a dashi.WarningTurnUnreadable. The caller
// holds mu.
func (s *Session) history(ctx context.Context, n int) ([]llm.Message, []dashi.Warning) {
	var parts [][]llm.Message    // newest first
	var warnings []dashi.Warning // newest first
	for i := n; i >= 1; i-- {
		t, err := s.load(ctx, i)
		if err == nil && t.view.Origin == dashi.StartCompaction {
			msgs, ok := t.view.ReplacementHistory()
			if !ok {
				continue // a failed compaction replaces nothing
			}
			if w := s.checkHash(ctx, t); w != nil {
				warnings = append(warnings, *w)
			}
			parts = append(parts, msgs)
			break
		}
		var msgs []llm.Message
		if err == nil {
			msgs, err = s.modelMessages(t)
		}
		if err != nil {
			warnings = append(warnings, dashi.Warning{
				Code: dashi.WarningTurnUnreadable,
				Message: fmt.Sprintf(
					"turn %s was left out of the history: %v",
					TurnID(s.ID, i),
					err,
				),
			})
			continue
		}
		parts = append(parts, msgs)
	}
	var out []llm.Message
	for i := len(parts) - 1; i >= 0; i-- {
		out = append(out, parts[i]...)
	}
	slices.Reverse(warnings)
	return out, warnings
}

// conversation returns the conversation history of turns 1 through n:
// every message of every turn but compaction turns. A turn that cannot be
// read is left out. The caller holds mu.
func (s *Session) conversation(ctx context.Context, n int) []llm.Message {
	var out []llm.Message
	for i := 1; i <= n; i++ {
		t, err := s.load(ctx, i)
		if err != nil || t.view.Origin == dashi.StartCompaction {
			continue
		}
		msgs, err := s.projection(t)
		if err == nil {
			out = append(out, msgs...)
		}
	}
	return out
}

// historyFor returns the Continue history of turn n: the projected history
// of the turns before it, or none for a compaction turn, which started with
// none. The caller holds mu.
func (s *Session) historyFor(
	ctx context.Context,
	n int,
	t *sessionTurn,
) ([]llm.Message, []dashi.Warning) {
	if t.view.Origin == dashi.StartCompaction {
		return nil, nil
	}
	return s.history(ctx, n-1)
}

// projection returns a turn's own messages. The caller holds mu.
func (s *Session) projection(t *sessionTurn) ([]llm.Message, error) {
	if t.projected {
		return t.messages, nil
	}
	msgs, err := dashi.TurnMessagesFull(t.turn)
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

func (s *Session) modelMessages(t *sessionTurn) ([]llm.Message, error) {
	if t.contextProjected {
		return t.contextMessages, nil
	}
	msgs, err := dashi.TurnMessages(t.turn)
	if err != nil {
		return nil, err
	}
	if t.ended() {
		t.contextMessages, t.contextProjected = msgs, true
	}
	return msgs, nil
}
