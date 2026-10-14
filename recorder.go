package dashi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/deepnoodle-ai/dashi/internal/wake"
)

// CommitRequest proposes one step at an expected sequence number and owner.
type CommitRequest struct {
	// Step is the fact to append. Its TurnID names the turn and its Seq is
	// ExpectedSeq+1.
	Step Step
	// ExpectedSeq is the committed sequence immediately before Step: the
	// number of steps the runner believes the turn has.
	ExpectedSeq int64
	// Owner is the lease token passed to Runner.Run with WithOwner, or empty.
	// A recorder that fences writers accepts a commit only when Owner equals
	// the token of the turn's latest claim; a turn never claimed accepts any
	// owner, including an empty one. Claiming is the host's lease operation
	// and is not part of this interface.
	Owner string
	// ModelCall is the call this step starts or finishes, as
	// TurnView.ModelCallCosts will show it: WorstCaseCost on a
	// StepModelCallStarted; Cost, Exact, and UsageSource on a
	// StepModelCallFinished or StepModelCallFinishedByHost. Nil on other
	// steps. A recorder reserves WorstCaseCost on a start and settles Cost
	// on the finish, releasing the hold, without loading the turn. A late
	// finish after the turn ended still carries it, so spend stays right.
	ModelCall *ModelCallCost
	// Delivered lists the inbox commands this step records, each with its
	// kind, so a recorder that shares a database with its inbox marks
	// them in the same transaction, and one that keeps steers apart from
	// other commands knows which is which. Dashi still calls
	// Inbox.Delivered after the commit.
	Delivered []DeliveredCommand
}

// DeliveredCommand names an inbox command a step records: its command ID
// and its kind, such as CommandSteer or CommandResolve.
type DeliveredCommand struct {
	ID   string      `json:"id"`
	Kind CommandKind `json:"kind"`
}

// Recorder is the durable record of turns. The runner commits each step
// before it acts on it, so a step the recorder acknowledged is the truth that
// recovery, accounting, and live presentation rebuild from. A host recorder
// can write its own rows in the same transaction: budget reservations,
// accounting, derived views, or an outbox.
//
// Commit must follow these rules, which the runner relies on for recovery:
//
//   - When the host leases turns, first reject a commit whose Owner is not
//     the latest claim's (see CommitRequest.Owner), even a retry of a step
//     already written.
//   - Append only at ExpectedSeq+1, and only when the turn has exactly
//     ExpectedSeq steps. Otherwise reject with ErrStaleTurn, unless
//     a rule below accepts the commit as a retry.
//   - A commit at an occupied sequence succeeds without appending when the
//     recorded step has the same JSON meaning (Step.Equal), because a
//     commit whose acknowledgement was lost is retried. Any other step there,
//     even one that differs only in its time, is rejected with
//     ErrStaleTurn: it comes from a worker holding an old snapshot.
//   - A step whose CommandID is already recorded in the turn succeeds without
//     appending when its Kind and CommandHash match the recorded step, and
//     is rejected with ErrCommandIDReused when they differ. Check this before
//     the sequence rules.
//   - Return a *CommitError with NothingWritten set only when this call
//     certainly wrote nothing. Any other error, including a timeout after
//     the write was sent, is treated as unknown: the runner stops and the
//     host loads the turn to learn whether it committed.
//   - Preserve each step's JSON meaning. Key order, whitespace, and number
//     spelling may change, so a jsonb column works; keep the step's own JSON
//     as the record rather than rebuilding fields such as At from columns.
//   - Be safe for concurrent use; of concurrent commits at one sequence,
//     exactly one succeeds.
//
// Validating that the appended steps form a well-formed turn is optional;
// the runner validates what it loads. A recorder that can read a turn back
// should also implement Loader, which recovery needs, and StepReader, which
// serves live readers through Follow.
//
// The dashitest package checks these rules (dashitest.Run) and injects commit
// faults for recovery tests. MemoryRecorder and the filerecorder package are
// reference implementations.
type Recorder interface {
	Commit(context.Context, CommitRequest) error
}

// Loader is an optional Recorder capability that returns the authoritative
// record of a turn. Runner.Run uses it to recognize a command retried with
// an old turn snapshot after a lost acknowledgement, and returns the result
// from the
// stored record instead of applying the command again.
//
// Load reports a turn with no committed steps as an error matching
// ErrTurnNotFound (errors.Is), or as an empty turn with the requested ID;
// Run then proceeds with the caller's snapshot as a new turn. Any other
// error means the record could not be read, and Run returns it without
// committing, because the caller's snapshot may be stale and acting on it
// could repeat a model call or tool execution.
type Loader interface {
	Load(ctx context.Context, turnID string) (Turn, error)
}

// CommitError distinguishes a rejected write from a write whose commit status
// must be checked by loading the turn. A recorder returns it without Step; a
// plain error from Commit is treated as a write whose status is unknown. Wrap
// ErrStaleTurn or ErrCommandIDReused in Err so callers can tell why a
// commit was rejected.
//
// Runner.Run returns a CommitError, with Step set, whenever a step write is
// not acknowledged. The run did execute: the RunResult returned with it holds
// the steps this run knows were saved, and recovery treats the failure as a
// crash after them. Check errors.Is(err, ErrStaleTurn) first, since
// another worker may own the turn.
type CommitError struct {
	// NothingWritten means the recorder confirmed that this call committed
	// nothing. When false, the write may have committed: load the turn to
	// find out.
	NothingWritten bool
	// Step is the step Run tried to record. A failed model_call_finished step
	// carries the call's usage, which the RunResult's spend does not include.
	Step *Step
	Err  error
}

func (e *CommitError) Error() string {
	var inner *CommitError
	if e.Step != nil && errors.As(e.Err, &inner) {
		// The recorder's own CommitError already names the persistence.
		return fmt.Sprintf(
			"dashi: step %d (%s) not recorded: %v",
			e.Step.Seq,
			e.Step.Kind,
			e.Err,
		)
	}
	if e.Step != nil {
		return fmt.Sprintf(
			"dashi: %s commit of step %d (%s): %v",
			e.status(),
			e.Step.Seq,
			e.Step.Kind,
			e.Err,
		)
	}
	return fmt.Sprintf("dashi: %s commit: %v", e.status(), e.Err)
}

// status names what is known about the failed write, for Error.
func (e *CommitError) status() string {
	if e.NothingWritten {
		return "rejected"
	}
	return "unknown"
}

// Unwrap exposes the underlying persistence error.
func (e *CommitError) Unwrap() error {
	return e.Err
}

// MemoryRecorder provides a fenced in-memory record for tests and local use.
// It is also a StepReader and StepWaiter, so Follow can serve its turns to
// readers in the same process.
type MemoryRecorder struct {
	mu     sync.Mutex
	turns  map[string]Turn
	owners map[string]string
	wakes  wake.Set
	// replays checks each commit against only the steps added since the
	// turn's last commit. A turn's entry is dropped once the turn ends.
	replays map[string]*replayCache
}

var (
	_ StepReader = (*MemoryRecorder)(nil)
	_ StepWaiter = (*MemoryRecorder)(nil)
)

// NewMemoryRecorder creates an empty in-memory recorder.
func NewMemoryRecorder() *MemoryRecorder {
	return &MemoryRecorder{turns: map[string]Turn{}, owners: map[string]string{}}
}

// newMemoryRecorderForTurn lets an unbound call extend its supplied record.
// The returned Turn is the only copy the caller can carry to another call.
func newMemoryRecorderForTurn(turn Turn) *MemoryRecorder {
	recorder := NewMemoryRecorder()
	if len(turn.Steps) > 0 {
		recorder.turns[turn.ID] = cloneTurn(turn)
	}
	return recorder
}

// Claim changes the in-memory owner token. Production lease policy belongs to
// the host; this method provides a deterministic recorder fixture. It never
// fails; the error result matches the shape of a host lease takeover.
func (r *MemoryRecorder) Claim(_ context.Context, turnID, owner string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owners[turnID] = owner
	return nil
}

// Load returns a copy of the turn's committed steps.
func (r *MemoryRecorder) Load(_ context.Context, turnID string) (Turn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.turns[turnID]
	if !ok {
		return Turn{}, ErrTurnNotFound
	}
	return cloneTurn(t), nil
}

// StepsAfter returns copies of up to limit committed steps after the
// cursor, following the StepReader rules.
func (r *MemoryRecorder) StepsAfter(
	_ context.Context,
	turnID string,
	after int64,
	limit int,
) ([]Step, error) {
	limit = max(limit, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	steps := r.turns[turnID].Steps
	after = max(after, 0)
	if after > int64(len(steps)) {
		return nil, fmt.Errorf(
			"%w: turn %q has %d steps; cursor %d",
			ErrCursorAhead,
			turnID,
			len(steps),
			after,
		)
	}
	page := steps[after:min(int64(len(steps)), after+int64(limit))]
	out := make([]Step, len(page))
	for i, step := range page {
		out[i] = cloneStep(step)
	}
	return out, nil
}

// Wait returns once the turn has a step after the cursor, or when ctx ends.
func (r *MemoryRecorder) Wait(
	ctx context.Context,
	turnID string,
	after int64,
) error {
	woken, done := r.wakes.Watch(turnID)
	defer done()
	r.mu.Lock()
	n := int64(len(r.turns[turnID].Steps))
	r.mu.Unlock()
	if n > after {
		return nil
	}
	select {
	case <-woken:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Commit appends a step only when its expected sequence and owner are valid.
func (r *MemoryRecorder) Commit(ctx context.Context, req CommitRequest) error {
	if _, err := json.Marshal(req.Step); err != nil {
		return &CommitError{NothingWritten: true, Err: err}
	}
	if err := ctx.Err(); err != nil {
		return &CommitError{NothingWritten: true, Err: err}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner := r.owners[req.Step.TurnID]; owner != "" && owner != req.Owner {
		return &CommitError{
			NothingWritten: true,
			Err:            ErrStaleTurn,
		}
	}
	t := r.turns[req.Step.TurnID]
	if req.Step.CommandID != "" {
		for _, old := range t.Steps {
			if old.CommandID != req.Step.CommandID {
				continue
			}
			if old.Kind == req.Step.Kind &&
				old.CommandHash == req.Step.CommandHash {
				return nil
			}
			return &CommitError{
				NothingWritten: true,
				Err:            ErrCommandIDReused,
			}
		}
	}
	if req.ExpectedSeq < 0 || req.Step.Seq != req.ExpectedSeq+1 {
		return &CommitError{
			NothingWritten: true,
			Err:            ErrStaleTurn,
		}
	}
	if int64(len(t.Steps)) >= req.Step.Seq {
		// A retry of an acknowledged step is a success. Compare by JSON
		// meaning, since the caller may retry a step it reloaded from storage.
		if t.Steps[req.Step.Seq-1].Equal(req.Step) {
			return nil
		}
		return &CommitError{
			NothingWritten: true,
			Err:            ErrStaleTurn,
		}
	}
	if int64(len(t.Steps)) != req.ExpectedSeq {
		return &CommitError{
			NothingWritten: true,
			Err:            ErrStaleTurn,
		}
	}
	if t.ID == "" {
		t.ID = req.Step.TurnID
	}
	if t.ID != req.Step.TurnID {
		return &CommitError{NothingWritten: true, Err: ErrInvalidTurn}
	}
	t.Steps = append(t.Steps, cloneStep(req.Step))
	if r.replays == nil {
		r.replays = map[string]*replayCache{}
	}
	cache := r.replays[t.ID]
	if cache == nil {
		cache = &replayCache{}
		r.replays[t.ID] = cache
	}
	state, err := cache.replay(t)
	if err != nil {
		return &CommitError{NothingWritten: true, Err: err}
	}
	if state.Stop.Ended {
		// Only rare late model results follow the end; they replay in full
		// rather than keep a cache for every turn the recorder has seen.
		delete(r.replays, t.ID)
	}
	r.turns[t.ID] = t
	r.wakes.Notify(t.ID)
	return nil
}
