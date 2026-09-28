package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// CommitRequest proposes one step at an expected turn revision and owner.
type CommitRequest struct {
	Step Step
	// ExpectedSeq is the committed sequence immediately before Step.
	ExpectedSeq int64
	// Owner is the host lease token checked by recorders that support fencing.
	Owner string
}

// Recorder acknowledges each fact before dependent execution. A host recorder
// can atomically include budget reservations, accounting, and outbox writes.
// A recorder that can read a turn back should also implement Loader.
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
//
// A recorder should also reject a step at an occupied sequence number that
// differs from the recorded one, even if only in its time or when it repeats
// an earlier step's kind and attempt or call ID. That keeps a worker holding
// an old snapshot from running past the record.
type Loader interface {
	Load(ctx context.Context, turnID string) (Turn, error)
}

// CommitError distinguishes a rejected write from a write whose commit status
// must be checked by loading the turn. A plain error is treated as unknown.
type CommitError struct {
	// Persistence states whether this failed call could have committed a step.
	Persistence Persistence
	Err         error
}

func (e *CommitError) Error() string {
	return fmt.Sprintf("kernel: %s commit: %v", e.Persistence, e.Err)
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
		if sameJSONValue(t.Steps[req.Step.Seq-1], req.Step) {
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
