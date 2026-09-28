package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

type CommitRequest struct {
	Step        Step
	ExpectedSeq int64
	Owner       string
}

// Recorder acknowledges each fact before dependent execution. A host recorder
// can atomically include budget reservations, accounting, and outbox writes.
// A recorder that can read a turn back should also implement Loader.
type Recorder interface {
	Commit(context.Context, CommitRequest) error
}

// Loader is an optional Recorder capability that returns the authoritative
// record of a turn. Engine.Apply uses it to recognize a command retried with
// an old turn snapshot after a lost acknowledgement, and answers from the
// stored record instead of applying the command again. When Load fails for a
// command the snapshot does not contain, Apply proceeds with the caller's
// snapshot. That is safe when the record has moved on: the recorder
// acknowledges the duplicate command step, but the next step Apply commits
// sits at an occupied sequence number with different content, so it is
// rejected before any model call or tool execution depends on it. A recorder
// must therefore reject such a step even when it repeats an earlier step's
// kind and attempt or call ID. A turn with no steps may be reported as an
// error or as an empty turn with the requested ID.
type Loader interface {
	Load(ctx context.Context, turnID string) (Turn, error)
}

// CommitError distinguishes a rejected write from a write whose commit status
// must be checked by loading the turn. A plain error is treated as unknown.
type CommitError struct {
	Persistence Persistence
	Err         error
}

func (e *CommitError) Error() string {
	return fmt.Sprintf("kernel: %s commit: %v", e.Persistence, e.Err)
}
func (e *CommitError) Unwrap() error { return e.Err }

type MemoryRecorder struct {
	mu     sync.Mutex
	turns  map[string]Turn
	owners map[string]string
}

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
		return Turn{}, errors.New("kernel: turn not found")
	}
	return cloneTurn(t), nil
}
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
		return &CommitError{Persistence: PersistenceRejected, Err: ErrRevisionConflict}
	}
	t := r.turns[req.Step.TurnID]
	if req.Step.CommandID != "" {
		for _, old := range t.Steps {
			if old.CommandID != req.Step.CommandID {
				continue
			}
			if old.Kind == req.Step.Kind && old.CommandDigest == req.Step.CommandDigest {
				return nil
			}
			return &CommitError{Persistence: PersistenceRejected, Err: ErrCommandConflict}
		}
	}
	if req.ExpectedSeq < 0 || req.Step.Seq != req.ExpectedSeq+1 {
		return &CommitError{Persistence: PersistenceRejected, Err: ErrRevisionConflict}
	}
	if int64(len(t.Steps)) >= req.Step.Seq {
		// A retry of an acknowledged step is a success. Compare by JSON
		// meaning, since the caller may retry a step it reloaded from storage.
		if sameJSONValue(t.Steps[req.Step.Seq-1], req.Step) {
			return nil
		}
		return &CommitError{Persistence: PersistenceRejected, Err: ErrRevisionConflict}
	}
	if int64(len(t.Steps)) != req.ExpectedSeq {
		return &CommitError{Persistence: PersistenceRejected, Err: ErrRevisionConflict}
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
