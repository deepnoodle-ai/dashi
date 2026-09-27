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
type Recorder interface {
	Commit(context.Context, CommitRequest) error
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

// Claim changes the in-memory owner token. Production lease policy belongs to
// the host; this method provides a deterministic recorder fixture.
func (r *MemoryRecorder) Claim(turnID, owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owners[turnID] = owner
}
func (r *MemoryRecorder) Load(turnID string) (Turn, error) {
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
	if req.ExpectedSeq < 0 || req.Step.Seq != req.ExpectedSeq+1 {
		return &CommitError{Persistence: PersistenceRejected, Err: ErrRevisionConflict}
	}
	if int64(len(t.Steps)) >= req.Step.Seq {
		old := t.Steps[req.Step.Seq-1]
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(req.Step)
		if string(a) == string(b) {
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
