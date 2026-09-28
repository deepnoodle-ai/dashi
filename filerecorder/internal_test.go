//go:build linux || darwin || freebsd || openbsd || netbsd || android || ios

package filerecorder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	err := r.Commit(t.Context(), request(2))
	requirePersistence(t, err, dashi.PersistenceRejected)
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
	err := r.Commit(t.Context(), request(2))
	requirePersistence(t, err, dashi.PersistenceUnknown)
	r.syncFile = func(f *os.File) error {
		return f.Sync()
	}
	err = r.Commit(t.Context(), request(3))
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

// TestReplacedLogResetsTheIndex swaps the log for another file whose first
// step differs only in its command ID, so the byte before the indexed offset
// is still a newline and only the file identity check notices the swap.
func TestReplacedLogResetsTheIndex(t *testing.T) {
	r := newTestRecorder(t)
	first := request(1)
	first.Step.CommandID = "start"
	first.Step.CommandDigest = "d1"
	// The retry is acknowledged from the log and leaves step 1 indexed.
	for range 2 {
		if err := r.Commit(t.Context(), first); err != nil {
			t.Fatal(err)
		}
	}
	other := newTestRecorder(t)
	replaced := request(1)
	replaced.Step.CommandID = "begin"
	replaced.Step.CommandDigest = "d1"
	if err := other.Commit(t.Context(), replaced); err != nil {
		t.Fatal(err)
	}
	commitStep(t, other, 2)
	commitStep(t, other, 3)
	data, err := os.ReadFile(other.Path("turn"))
	if err != nil {
		t.Fatal(err)
	}
	temp := r.Path("turn") + ".new"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, r.Path("turn")); err != nil {
		t.Fatal(err)
	}
	// A retry of the replacement's recorded command appends nothing.
	retry := request(4)
	retry.Step.CommandID = "begin"
	retry.Step.CommandDigest = "d1"
	if err := r.Commit(t.Context(), retry); err != nil {
		t.Fatal(err)
	}
	requireCount(t, r, 3)
}

// TestShrunkLogResetsTheIndex truncates the log in place below the indexed
// offset, as restoring a shorter copy into the same file would.
func TestShrunkLogResetsTheIndex(t *testing.T) {
	r := newTestRecorder(t)
	commitStep(t, r, 1)
	info, err := os.Stat(r.Path("turn"))
	if err != nil {
		t.Fatal(err)
	}
	commitStep(t, r, 2)
	commitStep(t, r, 3)
	if err := os.Truncate(r.Path("turn"), info.Size()); err != nil {
		t.Fatal(err)
	}
	commitStep(t, r, 2)
	requireCount(t, r, 2)
}

// TestRewrittenLogResetsTheIndex rewrites the log in place with a longer one
// whose byte before the indexed offset is not a newline.
func TestRewrittenLogResetsTheIndex(t *testing.T) {
	r := newTestRecorder(t)
	commitStep(t, r, 1)
	commitStep(t, r, 2)
	info, err := os.Stat(r.Path("turn"))
	if err != nil {
		t.Fatal(err)
	}
	other := newTestRecorder(t)
	long := request(1)
	long.Step.CommandID = "a-much-longer-command-id-that-shifts-the-lines"
	if err := other.Commit(t.Context(), long); err != nil {
		t.Fatal(err)
	}
	commitStep(t, other, 2)
	data, err := os.ReadFile(other.Path("turn"))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) < info.Size() || data[info.Size()-1] == '\n' {
		t.Fatalf("fixture does not cover the old end mid-line")
	}
	if err := os.WriteFile(r.Path("turn"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The rewritten log holds two steps; a stale index would misread it.
	commitStep(t, r, 3)
	requireCount(t, r, 3)
}

// TestFirstWriteSyncsDirectories covers a process that writes to a log
// another process created and may have crashed before syncing: it must sync
// the directories that name the log before acknowledging, once.
func TestFirstWriteSyncsDirectories(t *testing.T) {
	dir := t.TempDir()
	creator, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	commitStep(t, creator, 1)
	r, err := New(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	var synced []string
	r.syncFile = func(f *os.File) error {
		synced = append(synced, f.Name())
		return f.Sync()
	}
	commitStep(t, r, 2)
	shard := filepath.Dir(r.Path("turn"))
	want := []string{r.Path("turn"), shard, dir}
	if !slices.Equal(synced, want) {
		t.Fatalf("first write synced %q; want %q", synced, want)
	}
	synced = nil
	commitStep(t, r, 3)
	if want := []string{r.Path("turn")}; !slices.Equal(synced, want) {
		t.Fatalf("second write synced %q; want %q", synced, want)
	}
}

func TestFailedDirectorySyncIsUnknown(t *testing.T) {
	r := newTestRecorder(t)
	r.syncFile = func(f *os.File) error {
		if f.Name() == r.Path("turn") {
			return f.Sync()
		}
		return errors.New("EIO")
	}
	err := r.Commit(t.Context(), request(1))
	requirePersistence(t, err, dashi.PersistenceUnknown)
	err = r.Commit(t.Context(), request(2))
	if !errors.Is(err, errLogFailed) {
		t.Fatalf("commit after a failed directory sync returned %v", err)
	}
}

// TestRetiredWhileWaitingForLock retires the log while a commit waits for
// the lock; the commit must not append after the failed write.
func TestRetiredWhileWaitingForLock(t *testing.T) {
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
	done := make(chan error)
	go func() {
		done <- r.Commit(t.Context(), request(2))
	}()
	time.Sleep(50 * time.Millisecond)
	r.retire("turn", errors.New("EIO"))
	holder.Close()
	err = <-done
	requirePersistence(t, err, dashi.PersistenceRejected)
	if !errors.Is(err, errLogFailed) {
		t.Fatalf("commit returned %v; want errLogFailed", err)
	}
	requireCount(t, r, 1)
}

func TestClaimReturnsPlainErrors(t *testing.T) {
	r := newTestRecorder(t)
	r.writeAt = func(*os.File, []byte, int64) (int, error) {
		return 0, errors.New("disk full")
	}
	err := r.Claim(t.Context(), "turn", "owner")
	if err == nil {
		t.Fatal("claim succeeded after a failed write")
	}
	if _, ok := errors.AsType[*dashi.CommitError](err); ok {
		t.Fatalf("claim returned a *dashi.CommitError: %v", err)
	}
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
		t.Fatalf(
			"commit returned %v; want a *dashi.CommitError that is %s",
			err,
			want,
		)
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
