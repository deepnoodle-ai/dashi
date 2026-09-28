package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/deepnoodle-ai/dashi"
)

// fileRecorder keeps each turn in one JSON file, so a turn started by one
// process can be continued by another. It is a minimal durable recorder: it
// serializes commits within one process only, and its lease owners live in
// memory. A production host would commit each step in a database transaction
// alongside its own rows.
type fileRecorder struct {
	dir    string
	mu     sync.Mutex
	owners map[string]string
}

var _ dashi.Loader = (*fileRecorder)(nil)

func newFileRecorder(dir string) (*fileRecorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &fileRecorder{dir: dir, owners: map[string]string{}}, nil
}

// Load returns the turn's committed steps, or dashi.ErrTurnNotFound.
func (r *fileRecorder) Load(
	_ context.Context,
	turnID string,
) (dashi.Turn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.read(turnID)
}

// Claim makes owner the only token whose commits the turn accepts.
func (r *fileRecorder) Claim(_ context.Context, turnID, owner string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owners[turnID] = owner
	return nil
}

// Commit appends a step when it extends the turn at the expected sequence.
// It acknowledges a retried step or command again without appending it.
func (r *fileRecorder) Commit(
	ctx context.Context,
	req dashi.CommitRequest,
) error {
	if err := ctx.Err(); err != nil {
		return rejected(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id := req.Step.TurnID
	if owner := r.owners[id]; owner != "" && owner != req.Owner {
		return rejected(dashi.ErrRevisionConflict)
	}
	turn, err := r.read(id)
	if errors.Is(err, dashi.ErrTurnNotFound) {
		turn, err = dashi.NewTurn(id), nil
	}
	if err != nil {
		return rejected(err)
	}
	if req.Step.CommandID != "" {
		for _, old := range turn.Steps {
			if old.CommandID != req.Step.CommandID {
				continue
			}
			if old.Kind == req.Step.Kind &&
				old.CommandDigest == req.Step.CommandDigest {
				return nil
			}
			return rejected(dashi.ErrCommandConflict)
		}
	}
	n := int64(len(turn.Steps))
	switch {
	case req.ExpectedSeq < 0 || req.Step.Seq != req.ExpectedSeq+1:
		return rejected(dashi.ErrRevisionConflict)
	case req.ExpectedSeq < n:
		// A retry after a lost acknowledgement matches the recorded step;
		// any other step at an occupied sequence comes from a stale worker.
		if sameStep(turn.Steps[req.ExpectedSeq], req.Step) {
			return nil
		}
		return rejected(dashi.ErrRevisionConflict)
	case req.ExpectedSeq > n:
		return rejected(dashi.ErrRevisionConflict)
	}
	turn.Steps = append(turn.Steps, req.Step)
	return r.write(turn)
}

func (r *fileRecorder) path(turnID string) string {
	return filepath.Join(r.dir, url.PathEscape(turnID)+".json")
}

func (r *fileRecorder) read(turnID string) (dashi.Turn, error) {
	data, err := os.ReadFile(r.path(turnID))
	if errors.Is(err, fs.ErrNotExist) {
		return dashi.Turn{}, dashi.ErrTurnNotFound
	}
	if err != nil {
		return dashi.Turn{}, err
	}
	var turn dashi.Turn
	if err := json.Unmarshal(data, &turn); err != nil {
		return dashi.Turn{}, fmt.Errorf("resume: decode turn %s: %w", turnID, err)
	}
	return turn, nil
}

// write replaces the turn's file by renaming a synced temporary file, so a
// crash leaves either the old record or the new one. A failure before the
// rename certainly did not commit; the rename itself might have.
func (r *fileRecorder) write(turn dashi.Turn) error {
	data, err := json.Marshal(turn)
	if err != nil {
		return rejected(err)
	}
	f, err := os.CreateTemp(r.dir, "turn-*.tmp")
	if err != nil {
		return rejected(err)
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return rejected(err)
	}
	// A plain error tells the runner the write may have committed.
	return os.Rename(f.Name(), r.path(turn.ID))
}

func rejected(err error) error {
	return &dashi.CommitError{Persistence: dashi.PersistenceRejected, Err: err}
}

// sameStep compares steps by their JSON encoding. Both sides are encoded by
// Go from a dashi.Step, so equal meaning gives equal bytes here.
func sameStep(a, b dashi.Step) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}
