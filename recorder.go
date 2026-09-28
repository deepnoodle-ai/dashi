package dashi

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// CommitRequest proposes one step at an expected turn revision and owner.
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
}

// Recorder is the durable record of turns. The runner commits each step
// before it acts on it, so a step the recorder acknowledged is the truth that
// recovery, accounting, and live presentation rebuild from. A host recorder
// can write its own rows in the same transaction: budget reservations,
// accounting, projections, or an outbox.
//
// Commit must follow these rules, which the runner relies on for recovery:
//
//   - When the host leases turns, first reject a commit whose Owner is not
//     the latest claim's (see CommitRequest.Owner), even a retry of a step
//     already written.
//   - Append only at ExpectedSeq+1, and only when the turn has exactly
//     ExpectedSeq steps. Otherwise reject with ErrRevisionConflict, unless
//     a rule below accepts the commit as a retry.
//   - A commit at an occupied sequence succeeds without appending when the
//     recorded step has the same JSON meaning (Step.Equal), because a
//     commit whose acknowledgement was lost is retried. Any other step there,
//     even one that differs only in its time, is rejected with
//     ErrRevisionConflict: it comes from a worker holding an old snapshot.
//   - A step whose CommandID is already recorded in the turn succeeds without
//     appending when its Kind and CommandDigest match the recorded step, and
//     is rejected with ErrCommandConflict when they differ. Check this before
//     the sequence rules.
//   - Return a *CommitError with PersistenceRejected only when this call
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
// should also implement Loader, which recovery needs.
//
// The dashitest package checks these rules (dashitest.Run) and injects commit
// faults for recovery tests. MemoryRecorder and the filerecorder package are
// reference implementations.
type Recorder interface {
	Commit(context.Context, CommitRequest) error
}

// Loader is an optional Recorder capability that returns the authoritative
// record of a turn. Runner.Run uses it to recognize a command retried with
// an old turn snapshot after a lost acknowledgement, and answers from the
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
// must be checked by loading the turn. A plain error from Commit is treated
// as PersistenceUnknown. Wrap ErrRevisionConflict or ErrCommandConflict in
// Err so callers can tell why a commit was rejected.
type CommitError struct {
	// Persistence states whether this failed call could have committed a step.
	Persistence Persistence
	Err         error
}

func (e *CommitError) Error() string {
	return fmt.Sprintf("dashi: %s commit: %v", e.Persistence, e.Err)
}

// Unwrap exposes the underlying persistence error.
func (e *CommitError) Unwrap() error {
	return e.Err
}

// MemoryRecorder provides a fenced in-memory record for tests and local use.
type MemoryRecorder struct {
	mu     sync.Mutex
	turns  map[string]Turn
	owners map[string]string
}

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

// Commit appends a step only when its expected sequence and owner are valid.
func (r *MemoryRecorder) Commit(ctx context.Context, req CommitRequest) error {
	if _, err := json.Marshal(req.Step); err != nil {
		return &CommitError{Persistence: PersistenceRejected, Err: err}
	}
	if err := ctx.Err(); err != nil {
		return &CommitError{Persistence: PersistenceRejected, Err: err}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner := r.owners[req.Step.TurnID]; owner != "" && owner != req.Owner {
		return &CommitError{
			Persistence: PersistenceRejected,
			Err:         ErrRevisionConflict,
		}
	}
	t := r.turns[req.Step.TurnID]
	if req.Step.CommandID != "" {
		for _, old := range t.Steps {
			if old.CommandID != req.Step.CommandID {
				continue
			}
			if old.Kind == req.Step.Kind &&
				old.CommandDigest == req.Step.CommandDigest {
				return nil
			}
			return &CommitError{
				Persistence: PersistenceRejected,
				Err:         ErrCommandConflict,
			}
		}
	}
	if req.ExpectedSeq < 0 || req.Step.Seq != req.ExpectedSeq+1 {
		return &CommitError{
			Persistence: PersistenceRejected,
			Err:         ErrRevisionConflict,
		}
	}
	if int64(len(t.Steps)) >= req.Step.Seq {
		// A retry of an acknowledged step is a success. Compare by JSON
		// meaning, since the caller may retry a step it reloaded from storage.
		if t.Steps[req.Step.Seq-1].Equal(req.Step) {
			return nil
		}
		return &CommitError{
			Persistence: PersistenceRejected,
			Err:         ErrRevisionConflict,
		}
	}
	if int64(len(t.Steps)) != req.ExpectedSeq {
		return &CommitError{
			Persistence: PersistenceRejected,
			Err:         ErrRevisionConflict,
		}
	}
	if t.ID == "" {
		t.ID = req.Step.TurnID
	}
	if t.ID != req.Step.TurnID {
		return &CommitError{Persistence: PersistenceRejected, Err: ErrInvalidTurn}
	}
	t.Steps = append(t.Steps, cloneStep(req.Step))
	if _, err := replay(t); err != nil {
		return &CommitError{Persistence: PersistenceRejected, Err: err}
	}
	r.turns[t.ID] = t
	return nil
}
