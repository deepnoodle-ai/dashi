//go:build linux || darwin || freebsd || openbsd || netbsd || android || ios

package filerecorder

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
)

func TestLockWaitEndsWithContext(t *testing.T) {
	r := newTestRecorder(t)
	commitStep(t, r, 1)
	holder, err := os.Open(r.Path("turn"))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := lock(t.Context(), holder, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	began := time.Now()
	err = r.Commit(ctx, request(2))
	if waited := time.Since(began); waited > time.Second {
		t.Fatalf("commit waited %v for the lock after its context ended", waited)
	}
	requirePersistence(t, err, dashi.PersistenceRejected)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("commit error %v does not report the context", err)
	}
	holder.Close()
	requireCount(t, r, 1)
}

func TestFailedWriteIsRejectedAndRolledBack(t *testing.T) {
	r := newTestRecorder(t)
	commitStep(t, r, 1)
	r.writeAt = func(f *os.File, b []byte, off int64) (int, error) {
		n, _ := f.WriteAt(b[:len(b)/2], off)
		return n, errors.New("disk full")
	}
	requirePersistence(t, r.Commit(t.Context(), request(2)), dashi.PersistenceRejected)
	requireCount(t, r, 1)
	info, err := os.Stat(r.Path("turn"))
	if err != nil {
		t.Fatal(err)
	}
	r.writeAt = func(f *os.File, b []byte, off int64) (int, error) {
		return f.WriteAt(b, off)
	}
	commitStep(t, r, 2)
	after, err := os.Stat(r.Path("turn"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() <= info.Size() {
		t.Fatal("the log did not grow after the write recovered")
	}
}

func TestFailedSyncIsUnknownAndRetiresTheLog(t *testing.T) {
	r := newTestRecorder(t)
	commitStep(t, r, 1)
	r.syncFile = func(*os.File) error {
		return errors.New("EIO")
	}
	requirePersistence(t, r.Commit(t.Context(), request(2)), dashi.PersistenceUnknown)
	r.syncFile = func(f *os.File) error {
		return f.Sync()
	}
	err := r.Commit(t.Context(), request(3))
	requirePersistence(t, err, dashi.PersistenceRejected)
	if !errors.Is(err, errLogFailed) {
		t.Fatalf("commit after a failed sync returned %v; want errLogFailed", err)
	}
	other := request(1)
	other.Step.TurnID = "other"
	if err := r.Commit(t.Context(), other); err != nil {
		t.Fatalf("a failed sync retired another turn's log: %v", err)
	}
}

// TestReplacedLogResetsTheIndex covers a log swapped for another file, such
// as a restore from backup while no turn runs: the cached index must not
// outlive the file it described.
func TestReplacedLogResetsTheIndex(t *testing.T) {
	r := newTestRecorder(t)
	commitStep(t, r, 1)
	backup, err := os.ReadFile(r.Path("turn"))
	if err != nil {
		t.Fatal(err)
	}
	commitStep(t, r, 2)
	commitStep(t, r, 3)
	if err := os.Remove(r.Path("turn")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.Path("turn"), backup, 0o600); err != nil {
		t.Fatal(err)
	}
	commitStep(t, r, 2)
	requireCount(t, r, 2)
}

func newTestRecorder(t *testing.T) *Recorder {
	t.Helper()
	r, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func request(seq int64) dashi.CommitRequest {
	return dashi.CommitRequest{
		Step: dashi.Step{
			Version: 1,
			TurnID:  "turn",
			Seq:     seq,
			At:      time.Date(2026, 1, 2, 3, 4, 5, int(seq), time.UTC),
			Kind:    dashi.StepDelivered,
		},
		ExpectedSeq: seq - 1,
	}
}

func commitStep(t *testing.T, r *Recorder, seq int64) {
	t.Helper()
	if err := r.Commit(t.Context(), request(seq)); err != nil {
		t.Fatalf("commit seq %d: %v", seq, err)
	}
}

func requirePersistence(t *testing.T, err error, want dashi.Persistence) {
	t.Helper()
	commitErr, ok := errors.AsType[*dashi.CommitError](err)
	if !ok || commitErr.Persistence != want {
		t.Fatalf("commit returned %v; want a *dashi.CommitError that is %s", err, want)
	}
}

func requireCount(t *testing.T, r *Recorder, n int) {
	t.Helper()
	turn, err := r.Load(t.Context(), "turn")
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Steps) != n {
		t.Fatalf("loaded %d steps; want %d", len(turn.Steps), n)
	}
}
