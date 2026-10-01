// Package filerecorder is a durable dashi.Recorder that keeps each turn in an
// append-only file, so a turn started by one process can be continued by
// another on the same machine. It is a reference
// implementation of the recorder contract and passes dashitest.Run; a host
// that commits steps alongside its own database rows writes its own recorder
// instead (see design/durable-recorders.md).
//
// Each turn has one JSON Lines log at Dir/ab/<sha256 of turn ID>.jsonl, where
// ab is the hash's first two hex digits. The first line names the turn:
//
//	{"format":1,"turn_id":"email-1"}
//
// and each later line is one committed step or one lease claim:
//
//	{"step":{"version":1,"turn_id":"email-1","seq":1,...}}
//	{"claim":{"owner":"worker-7","at":"2026-09-28T12:00:00Z"}}
//
// Writers in any number of processes serialize per turn on an exclusive
// flock of the log, so commits and lease fencing are exact. A commit is one
// write of a newline-terminated line followed by fsync, so a crash leaves at
// most a torn final line without a newline, which never counts as written
// and is removed by the next commit. Any other damage is reported as
// ErrCorrupt rather than repaired.
//
// Commit and StepsAfter read only the lines appended since this process last
// saw the turn, using an in-process index of recently used turns; Load reads
// the whole log. The Recorder is also a dashi.StepReader, so dashi.Follow
// can serve a turn to live readers, and a dashi.StepWaiter that wakes them
// at once on this process's commits and within a quarter second of another
// process's. As a session.Index it finds a session's last turn by which
// logs exist. Each read takes a shared lock on the log, which a commit waits
// for, so many followers reading one turn add latency to its commits; a
// dashboard with many readers serves them from its own fan-out instead.
//
// The package supports Linux, macOS, iOS, Android, FreeBSD, OpenBSD, and
// NetBSD on local filesystems. Advisory locks on network filesystems are
// unreliable, so do not share Dir over NFS or SMB. On other systems New
// returns an error. It depends only on dashi and the standard library.
package filerecorder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/internal/wake"
	"github.com/deepnoodle-ai/dashi/session"
)

// ErrCorrupt reports a turn log that cannot be read as a sequence of
// complete records for its turn: a complete line that does not decode, a
// header naming another turn, or steps out of sequence. Load and Commit wrap
// it with the log's path and line number. Nothing repairs such a log; an
// operator inspects the file at Recorder.Path.
var ErrCorrupt = errors.New("filerecorder: corrupt turn log")

// errLogFailed reports a log whose earlier write or sync failed in this
// process. After a failed fsync the kernel may have dropped written data
// while still serving it from the machine-wide page cache, so what the file
// reads back may not match the disk. Retiring the log only stops this
// process from appending after a step that may be lost; it does not make
// the log trustworthy again. Reads, and other processes, may still see the
// cached data until the machine restarts, so an operator should inspect the
// log before the turn resumes.
var errLogFailed = errors.New(
	"filerecorder: an earlier write to this log failed",
)

// Config configures a Recorder.
type Config struct {
	// Dir holds the turn logs. It is created if missing, and New syncs it
	// and every ancestor so its name survives a power loss; New fails if any
	// of them cannot be opened and synced. A relative Dir is resolved once,
	// in New. Required.
	Dir string

	// DisableSync skips fsync after each write. Commits are then much faster
	// and survive a process crash, but an operating system crash or power
	// loss can lose steps that were acknowledged. Use it for tests and local
	// development only.
	DisableSync bool
}

// Recorder is a durable, file-backed dashi.Recorder and dashi.Loader. It is
// safe for concurrent use, including by several processes sharing Dir. It
// holds no open files between calls, so it needs no Close.
//
// The host must not delete or edit a turn's log while the turn may still
// run.
type Recorder struct {
	dir  string
	sync bool

	mu     sync.Mutex
	failed map[string]error
	index  *indexCache
	wakes  wake.Set

	// writeAt and syncFile are the file operations whose failures tests
	// inject.
	writeAt  func(f *os.File, b []byte, off int64) (int, error)
	syncFile func(f *os.File) error
}

var (
	_ dashi.Recorder   = (*Recorder)(nil)
	_ dashi.Loader     = (*Recorder)(nil)
	_ dashi.StepReader = (*Recorder)(nil)
	_ dashi.StepWaiter = (*Recorder)(nil)
	_ session.Index    = (*Recorder)(nil)
)

// New returns a recorder for the turn logs in cfg.Dir, creating the
// directory if needed.
func New(cfg Config) (*Recorder, error) {
	return newRecorder(cfg, func(f *os.File) error {
		return f.Sync()
	})
}

// newRecorder is New with the sync operation tests inject failures into.
func newRecorder(
	cfg Config,
	syncFile func(*os.File) error,
) (*Recorder, error) {
	if !supported {
		return nil, fmt.Errorf(
			"filerecorder: not supported on %s",
			runtime.GOOS,
		)
	}
	if cfg.Dir == "" {
		return nil, errors.New("filerecorder: Config.Dir is required")
	}
	abs, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("filerecorder: resolve Dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("filerecorder: create Dir: %w", err)
	}
	r := &Recorder{
		dir:    abs,
		sync:   !cfg.DisableSync,
		failed: map[string]error{},
		index:  newIndexCache(),
		writeAt: func(f *os.File, b []byte, off int64) (int, error) {
			return f.WriteAt(b, off)
		},
		syncFile: syncFile,
	}
	if err := r.syncPath(abs); err != nil {
		return nil, err
	}
	return r, nil
}

// syncPath makes every directory entry on the path to dir durable by syncing
// dir and each ancestor up to the root. It does not depend on whether this
// call created them: MkdirAll may create several components, and after a
// partial earlier attempt a directory created then looks as if it already
// existed, so no ancestor can safely be skipped. Any failure is an error.
// It runs once per New.
func (r *Recorder) syncPath(dir string) error {
	for p := dir; ; p = filepath.Dir(p) {
		if err := r.syncDir(p); err != nil {
			return fmt.Errorf(
				"filerecorder: cannot guarantee that Dir survives a power "+
					"loss, because %s could not be synced; choose a Dir "+
					"whose ancestors are readable and writable: %w",
				p,
				err,
			)
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

// Path returns the file that holds turnID's log, whether or not it exists.
// Operators use it to inspect a turn; the host must not change the file
// while the turn may run.
func (r *Recorder) Path(turnID string) string {
	name := logName(turnID)
	return filepath.Join(r.dir, name[:2], name+".jsonl")
}

// LastTurn returns the highest n for which turn n of the session
// (session.TurnID) has a log with content, or 0 when it has none. It
// implements session.Index by which files exist, without reading them,
// so a damaged log does not hide the turns before it: a session.Session then
// reports that turn as unreadable. It doubles n until a log is missing,
// then searches between, so it looks at about 2·log2(n) files. An empty
// log, left by a rejected first write, is no turn; a log that holds only a
// lease claim (Claim) counts as one, since only a read could tell.
func (r *Recorder) LastTurn(ctx context.Context, sessionID string) (int, error) {
	return lastTurn(ctx, sessionID, r.hasLog)
}

// hasLog reports whether turnID's log exists with content.
func (r *Recorder) hasLog(turnID string) (bool, error) {
	info, err := os.Stat(r.Path(turnID))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("filerecorder: %w", err)
	}
	return info.Size() > 0, nil
}

// Load returns the turn's committed steps, or an error matching
// dashi.ErrTurnNotFound when it has none. A damaged log gives an error
// matching ErrCorrupt. It waits for a writer holding the log until ctx ends.
func (r *Recorder) Load(
	ctx context.Context,
	turnID string,
) (dashi.Turn, error) {
	path := r.Path(turnID)
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return dashi.Turn{}, dashi.ErrTurnNotFound
	}
	if err != nil {
		return dashi.Turn{}, fmt.Errorf("filerecorder: open log: %w", err)
	}
	defer f.Close()
	return loadLog(ctx, f, turnID)
}

// Claim makes owner the only token whose commits the turn accepts, as a lease
// takeover does. It may be called before the turn has any steps, and it is
// recorded in the log. Lease policy (who may claim, and when) belongs to the
// host. Its errors are plain errors; a failed Claim may or may not have been
// recorded, and a later Claim by the same owner is harmless.
func (r *Recorder) Claim(ctx context.Context, turnID, owner string) error {
	if err := r.usable(turnID); err != nil {
		return err
	}
	record, err := json.Marshal(struct {
		Claim claimRecord `json:"claim"`
	}{
		Claim: claimRecord{Owner: owner, At: time.Now().UTC()},
	})
	if err != nil {
		return fmt.Errorf("filerecorder: encode claim: %w", err)
	}
	f, err := r.openForWrite(turnID)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lock(ctx, f, true); err != nil {
		return fmt.Errorf("filerecorder: lock log: %w", err)
	}
	// Check again under the lock: another goroutine may have retired the
	// log while this one waited.
	if err := r.usable(turnID); err != nil {
		return err
	}
	log, err := r.scan(f, turnID)
	if err != nil {
		return err
	}
	if err := r.append(f, turnID, log, record); err != nil {
		if commitErr, ok := errors.AsType[*dashi.CommitError](err); ok {
			return commitErr.Err
		}
		return err
	}
	return nil
}

// Commit appends a step when it extends the turn at the expected sequence and
// owner, and acknowledges a retried step or command without appending it. It
// follows the rules on dashi.Recorder. A rejected commit leaves the log
// unchanged. If the lock is not acquired before ctx ends, the commit is
// rejected.
func (r *Recorder) Commit(ctx context.Context, req dashi.CommitRequest) error {
	if err := ctx.Err(); err != nil {
		return rejected(err)
	}
	turnID := req.Step.TurnID
	if err := r.usable(turnID); err != nil {
		return rejected(err)
	}
	record, err := json.Marshal(struct {
		Step dashi.Step `json:"step"`
	}{
		Step: req.Step,
	})
	if err != nil {
		return rejected(fmt.Errorf("filerecorder: encode step: %w", err))
	}
	f, err := r.openForWrite(turnID)
	if err != nil {
		return rejected(err)
	}
	defer f.Close()
	if err := lock(ctx, f, true); err != nil {
		return rejected(fmt.Errorf("filerecorder: lock log: %w", err))
	}
	// Check again under the lock: another goroutine may have retired the
	// log while this one waited, and this step must not follow a line whose
	// sync failed.
	if err := r.usable(turnID); err != nil {
		return rejected(err)
	}
	log, err := r.scan(f, turnID)
	if err != nil {
		return rejected(err)
	}
	if log.claimed && log.owner != req.Owner {
		return rejected(dashi.ErrRevisionConflict)
	}
	if req.Step.CommandID != "" {
		for _, old := range log.steps {
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
	n := int64(len(log.steps))
	switch {
	case req.ExpectedSeq < 0 || req.Step.Seq != req.ExpectedSeq+1:
		return rejected(dashi.ErrRevisionConflict)
	case req.ExpectedSeq < n:
		// A retry after a lost acknowledgement matches the recorded step;
		// any other step at an occupied sequence comes from a stale worker.
		old, err := log.steps[req.ExpectedSeq].decode(f, nil, 0)
		if err != nil {
			return rejected(err)
		}
		if old.Equal(req.Step) {
			return nil
		}
		return rejected(dashi.ErrRevisionConflict)
	case req.ExpectedSeq > n:
		return rejected(dashi.ErrRevisionConflict)
	}
	if err := r.append(f, turnID, log, record); err != nil {
		return err
	}
	r.wakes.Notify(turnID)
	return nil
}

// StepsAfter returns up to limit committed steps after the cursor, following
// the dashi.StepReader rules. It holds a shared lock on the log while it
// reads, so it never sees a partly written line, and waits for a writer
// holding the log until ctx ends. A damaged log gives an error matching
// ErrCorrupt.
func (r *Recorder) StepsAfter(
	ctx context.Context,
	turnID string,
	after int64,
	limit int,
) ([]dashi.Step, error) {
	limit = max(limit, 1)
	after = max(after, 0)
	ahead := func(n int64) error {
		return fmt.Errorf(
			"%w: turn %q has %d steps; cursor %d",
			dashi.ErrCursorAhead,
			turnID,
			n,
			after,
		)
	}
	f, err := os.Open(r.Path(turnID))
	if errors.Is(err, fs.ErrNotExist) {
		if after > 0 {
			return nil, ahead(0)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("filerecorder: open log: %w", err)
	}
	defer f.Close()
	if err := lock(ctx, f, false); err != nil {
		return nil, fmt.Errorf("filerecorder: lock log: %w", err)
	}
	r.mu.Lock()
	cached := r.index.get(turnID)
	r.mu.Unlock()
	log, data, err := scan(f, turnID, cached)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.index.put(turnID, log)
	r.mu.Unlock()
	n := int64(len(log.steps))
	if after > n {
		return nil, ahead(n)
	}
	base := log.size - int64(len(data))
	page := log.steps[after:min(n, after+int64(limit))]
	steps := make([]dashi.Step, len(page))
	for i, entry := range page {
		if steps[i], err = entry.decode(f, data, base); err != nil {
			return nil, err
		}
	}
	return steps, nil
}

// waitCheck is how often Wait looks for commits from other processes.
var waitCheck = 250 * time.Millisecond

// Wait returns when the turn may have a step after the cursor: at once when
// one exists, on this process's next commit to the turn, or within
// waitCheck of another process's commit, which it notices by the log's size.
// It returns ctx's error when ctx ends first. It reads the log only when
// this process's index is behind it; a false wake costs one read.
func (r *Recorder) Wait(ctx context.Context, turnID string, after int64) error {
	woken, done := r.wakes.Watch(turnID)
	defer done()
	size := r.logSize(turnID)
	r.mu.Lock()
	cached := r.index.get(turnID)
	r.mu.Unlock()
	if int64(len(cached.steps)) > after {
		return nil
	}
	if size != cached.size {
		// The index is behind the log (a commit updates it only before
		// appending), so read to learn whether a newer step exists. A read
		// error is for the caller's next read to report.
		steps, err := r.StepsAfter(ctx, turnID, after, 1)
		if err != nil || len(steps) > 0 {
			return nil
		}
	}
	tick := time.NewTicker(waitCheck)
	defer tick.Stop()
	for {
		select {
		case <-woken:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if r.logSize(turnID) != size {
				return nil
			}
		}
	}
}

// logSize returns the size of the turn's log, or 0 when it has none. An
// unreadable log reports -1, which differs from any size Wait compares it
// to, so the caller reads and learns the error.
func (r *Recorder) logSize(turnID string) int64 {
	info, err := os.Stat(r.Path(turnID))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0
	case err != nil:
		return -1
	}
	return info.Size()
}

// scan updates the turn's cached index from its log. The caller holds the
// exclusive lock, so no other writer extends the log meanwhile.
func (r *Recorder) scan(f *os.File, turnID string) (turnLog, error) {
	r.mu.Lock()
	cached := r.index.get(turnID)
	r.mu.Unlock()
	log, _, err := scan(f, turnID, cached)
	if err != nil {
		return turnLog{}, err
	}
	r.mu.Lock()
	r.index.put(turnID, log)
	r.mu.Unlock()
	return log, nil
}

// usable reports whether an earlier failure retired the turn's log.
func (r *Recorder) usable(turnID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failed[turnID]; err != nil {
		return fmt.Errorf("%w: %w", errLogFailed, err)
	}
	return nil
}

func (r *Recorder) retire(turnID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed[turnID] = err
}

// openForWrite opens the turn's log for reading and writing, creating it and
// its shard directory if needed. It does not sync the new directory entries;
// append does that before acknowledging the first write this process makes
// to the log, whichever process created it.
func (r *Recorder) openForWrite(turnID string) (*os.File, error) {
	path := r.Path(turnID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("filerecorder: create shard: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filerecorder: open log: %w", err)
	}
	return f, nil
}

// syncDirs makes the log's name durable: its entry in the shard directory
// and the shard's entry in Dir. A file fsync does not cover them, and the
// process that created them may have crashed before syncing them, so every
// process syncs them before its first acknowledged write to a log.
func (r *Recorder) syncDirs(path string) error {
	if err := r.syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	return r.syncDir(r.dir)
}

func (r *Recorder) syncDir(dir string) error {
	if !r.sync {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("filerecorder: sync directory: %w", err)
	}
	defer d.Close()
	if err := r.syncFile(d); err != nil {
		return fmt.Errorf("filerecorder: sync directory: %w", err)
	}
	return nil
}

// append writes record as the log's next line, after the header when the log
// has none, replacing any torn tail. The caller holds the exclusive lock.
func (r *Recorder) append(
	f *os.File,
	turnID string,
	log turnLog,
	record []byte,
) error {
	var buf bytes.Buffer
	if !log.hasHeader {
		header, err := json.Marshal(headerRecord{
			Format: logFormat,
			TurnID: turnID,
		})
		if err != nil {
			return rejected(fmt.Errorf("filerecorder: encode header: %w", err))
		}
		buf.Write(header)
		buf.WriteByte('\n')
	}
	buf.Write(record)
	buf.WriteByte('\n')
	if log.size > log.end {
		// A torn line from a crashed writer was never acknowledged.
		if err := f.Truncate(log.end); err != nil {
			return rejected(fmt.Errorf("filerecorder: remove torn line: %w", err))
		}
	}
	if _, err := r.writeAt(f, buf.Bytes(), log.end); err != nil {
		if truncErr := f.Truncate(log.end); truncErr != nil {
			err = errors.Join(err, truncErr)
			r.retire(turnID, err)
			return unknown(fmt.Errorf("filerecorder: write: %w", err))
		}
		return rejected(fmt.Errorf("filerecorder: write: %w", err))
	}
	if !r.sync {
		return nil
	}
	if err := r.syncFile(f); err != nil {
		// After a failed fsync the kernel may have discarded the written
		// pages while still serving them from cache, so the outcome is
		// unknown and this process stops trusting the log.
		r.retire(turnID, err)
		return unknown(fmt.Errorf("filerecorder: sync: %w", err))
	}
	if !log.dirsSynced {
		if err := r.syncDirs(f.Name()); err != nil {
			r.retire(turnID, err)
			return unknown(err)
		}
		r.mu.Lock()
		r.index.markDirsSynced(turnID)
		r.mu.Unlock()
	}
	return nil
}

func corrupt(path string, line int, reason string) error {
	return fmt.Errorf("%w: %s line %d: %s", ErrCorrupt, path, line, reason)
}

func rejected(err error) error {
	return &dashi.CommitError{Persistence: dashi.PersistenceRejected, Err: err}
}

func unknown(err error) error {
	return &dashi.CommitError{Persistence: dashi.PersistenceUnknown, Err: err}
}
