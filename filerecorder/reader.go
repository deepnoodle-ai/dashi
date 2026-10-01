package filerecorder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"github.com/deepnoodle-ai/dashi"
)

// Reader loads recorded Turns without creating, syncing, claiming, or repairing
// storage. It implements Loader and SessionIndex only. Close releases its pinned
// directory handle; calls after Close fail. It is safe for concurrent reads.
// The caller must not delete or edit logs while their Turns may run.
type Reader struct{ root *os.Root }

var (
	_ dashi.Loader       = (*Reader)(nil)
	_ dashi.SessionIndex = (*Reader)(nil)
)

// OpenReader opens an existing log directory. Missing storage wraps
// fs.ErrNotExist. The directory, shards, and logs must not be symlinks.
// Its parent is a host-selected storage boundary; no execution lease is taken.
func OpenReader(dir string) (*Reader, error) {
	if !supported {
		return nil, fmt.Errorf("filerecorder: file locking is not supported")
	}
	if dir == "" {
		return nil, errors.New("filerecorder: reader directory is required")
	}
	full, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	parent, err := os.OpenRoot(filepath.Dir(full))
	if err != nil {
		return nil, fmt.Errorf("filerecorder: open reader parent: %w", err)
	}
	defer parent.Close()
	root, err := readDirectory(parent, filepath.Base(full))
	if err != nil {
		return nil, fmt.Errorf("filerecorder: open reader: %w", err)
	}
	return &Reader{root: root}, nil
}

// Close releases the reader's directory handle. It does not write to storage.
func (r *Reader) Close() error { return r.root.Close() }

func readDirectory(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, errors.New("filerecorder: log directory must be a directory, not a symlink")
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, statErr := root.Stat(".")
	after, afterErr := parent.Lstat(name)
	if statErr != nil || afterErr != nil || !after.IsDir() || !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		root.Close()
		return nil, errors.New("filerecorder: log directory changed during open")
	}
	return root, nil
}

func logName(turnID string) string {
	sum := sha256.Sum256([]byte(turnID))
	return hex.EncodeToString(sum[:])
}

func (r *Reader) openLog(turnID string) (*os.File, error) {
	name := logName(turnID)
	shard, err := readDirectory(r.root, name[:2])
	if err != nil {
		return nil, err
	}
	defer shard.Close()
	file, err := shard.OpenFile(name+".jsonl", os.O_RDONLY|readerFileFlags(), 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err == nil {
			err = errors.New("filerecorder: log must be a regular file")
		}
		return nil, err
	}
	return file, nil
}

// Load reads a snapshot of acknowledged complete lines under the log's shared
// lock. A torn final line is ignored without truncation. Cancellation interrupts
// lock waits, chunk reads, and between-line decoding.
func (r *Reader) Load(ctx context.Context, turnID string) (dashi.Turn, error) {
	if err := ctx.Err(); err != nil {
		return dashi.Turn{}, err
	}
	file, err := r.openLog(turnID)
	if errors.Is(err, fs.ErrNotExist) {
		return dashi.Turn{}, dashi.ErrTurnNotFound
	}
	if err != nil {
		return dashi.Turn{}, fmt.Errorf("filerecorder: open log: %w", err)
	}
	defer file.Close()
	return loadLog(ctx, file, turnID)
}

func loadLog(ctx context.Context, file *os.File, turnID string) (dashi.Turn, error) {
	if err := ctx.Err(); err != nil {
		return dashi.Turn{}, err
	}
	if err := lock(ctx, file, false); err != nil {
		return dashi.Turn{}, fmt.Errorf("filerecorder: lock log: %w", err)
	}
	log, data, err := scanContext(ctx, file, turnID, turnLog{})
	if err != nil {
		return dashi.Turn{}, err
	}
	if len(log.steps) == 0 {
		return dashi.Turn{}, dashi.ErrTurnNotFound
	}
	turn := dashi.NewTurn(turnID)
	turn.Steps = make([]dashi.Step, len(log.steps))
	for i, entry := range log.steps {
		if err := ctx.Err(); err != nil {
			return dashi.Turn{}, err
		}
		step, err := entry.decode(file, data, 0)
		if err != nil {
			return dashi.Turn{}, err
		}
		turn.Steps[i] = step
	}
	return turn, ctx.Err()
}

// LastTurn finds the highest sequential numbered log with content, without
// interpreting its facts. Corrupt and claim-only logs remain discoverable.
func (r *Reader) LastTurn(ctx context.Context, sessionID string) (int, error) {
	return lastTurn(ctx, sessionID, func(id string) (bool, error) {
		f, err := r.openLog(id)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		defer f.Close()
		info, err := f.Stat()
		return err == nil && info.Size() > 0, err
	})
}

func lastTurn(ctx context.Context, sessionID string, hasLog func(string) (bool, error)) (int, error) {
	lo, hi := 0, 0
	for n := 1; hi == 0; {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		ok, err := hasLog(dashi.TurnID(sessionID, n))
		if err != nil {
			return 0, err
		}
		if !ok {
			hi = n
			break
		}
		lo = n
		if n == math.MaxInt {
			return n, nil
		}
		if n > math.MaxInt/2 {
			n = math.MaxInt
		} else {
			n *= 2
		}
	}
	for hi-lo > 1 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		mid := lo + (hi-lo)/2
		ok, err := hasLog(dashi.TurnID(sessionID, mid))
		if err != nil {
			return 0, err
		}
		if ok {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, ctx.Err()
}
