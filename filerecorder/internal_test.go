//go:build linux || darwin || freebsd || openbsd || netbsd || android || ios

package filerecorder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	requireNothingWritten(t, err, true)
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
	requireNothingWritten(t, err, true)
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
	requireNothingWritten(t, err, false)
	r.syncFile = func(f *os.File) error {
		return f.Sync()
	}
	err = r.Commit(t.Context(), request(3))
	requireNothingWritten(t, err, true)
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
	first.Step.CommandHash = "d1"
	// The retry is acknowledged from the log and leaves step 1 indexed.
	for range 2 {
		if err := r.Commit(t.Context(), first); err != nil {
			t.Fatal(err)
		}
	}
	other := newTestRecorder(t)
	replaced := request(1)
	replaced.Step.CommandID = "begin"
	replaced.Step.CommandHash = "d1"
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
	retry.Step.CommandHash = "d1"
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

// TestNewSyncsEveryComponentOfANewDir creates Dir three levels below an
// existing directory. Each new component's entry lives in its parent, so New
// must sync Dir and every ancestor, including the existing one that now names
// the topmost new directory.
func TestNewSyncsEveryComponentOfANewDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "a", "b", "c")
	var synced []string
	_, err := newRecorder(Config{Dir: dir}, func(f *os.File) error {
		synced = append(synced, f.Name())
		return f.Sync()
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		dir,
		filepath.Join(base, "a", "b"),
		filepath.Join(base, "a"),
		base,
	}
	if len(synced) < len(want) || !slices.Equal(synced[:len(want)], want) {
		t.Fatalf("New synced %q; want it to start with %q", synced, want)
	}
	if last := synced[len(synced)-1]; last != filepath.Dir(last) {
		t.Fatalf("New stopped syncing at %s, before the root", last)
	}
}

// TestNewRetriesAFailedDirectorySync fails the sync of one new component. A
// second New finds every directory present and must sync them all anyway.
func TestNewRetriesAFailedDirectorySync(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "a", "b")
	failing := filepath.Join(base, "a")
	_, err := newRecorder(Config{Dir: dir}, func(f *os.File) error {
		if f.Name() == failing {
			return errors.New("EIO")
		}
		return f.Sync()
	})
	if err == nil {
		t.Fatal("New succeeded after a failed directory sync")
	}
	var synced []string
	_, err = newRecorder(Config{Dir: dir}, func(f *os.File) error {
		synced = append(synced, f.Name())
		return f.Sync()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{dir, failing, base} {
		if !slices.Contains(synced, want) {
			t.Fatalf("retried New synced %q; want %s among them", synced, want)
		}
	}
}

// TestNewRejectsAnUnreadableAncestor covers a directory the process may
// search and write but not read. New cannot sync it, and it may hold the
// entry that names Dir, so New must fail rather than return a recorder that
// acknowledges commits it cannot make durable.
func TestNewRejectsAnUnreadableAncestor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any directory")
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(locked, 0o700)
	})
	r, err := New(Config{Dir: filepath.Join(locked, "turns")})
	if err == nil || r != nil {
		t.Fatalf("New returned %v, %v; want no recorder and an error", r, err)
	}
	if !strings.Contains(err.Error(), locked) {
		t.Fatalf("error %v does not name %s", err, locked)
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
	requireNothingWritten(t, err, false)
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
	requireNothingWritten(t, err, true)
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

// TestCommitIndexesTheWrittenStep checks that a commit leaves the cached
// index matching the log, as a fresh scan reads it.
func TestCommitIndexesTheWrittenStep(t *testing.T) {
	r := newTestRecorder(t)
	commitStep(t, r, 1)
	commitStep(t, r, 2)
	r.mu.Lock()
	cached := r.index.get("turn")
	r.mu.Unlock()
	f, err := os.Open(r.Path("turn"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fresh, _, err := scan(f, "turn", turnLog{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cached.steps, fresh.steps) ||
		cached.end != fresh.end ||
		cached.size != fresh.size ||
		cached.lines != fresh.lines ||
		!cached.hasHeader ||
		!cached.dirsSynced {
		t.Fatalf("cached index %+v; want %+v with directories synced", cached, fresh)
	}
	// With the index current, Wait answers from it without reading.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := r.Wait(ctx, "turn", 1); err != nil {
		t.Fatalf("Wait after seq 1: %v", err)
	}
}

// TestOlderIndexDoesNotReplaceANewerOne puts an index scanned before a
// commit after that commit, as a slow reader could.
func TestOlderIndexDoesNotReplaceANewerOne(t *testing.T) {
	r := newTestRecorder(t)
	commitStep(t, r, 1)
	r.mu.Lock()
	older := r.index.get("turn")
	r.mu.Unlock()
	older.dirsSynced = false
	commitStep(t, r, 2)
	r.mu.Lock()
	r.index.put("turn", older)
	cached := r.index.get("turn")
	r.mu.Unlock()
	if len(cached.steps) != 2 || !cached.dirsSynced {
		t.Fatalf(
			"index has %d steps, directories synced %t; want 2 and true",
			len(cached.steps),
			cached.dirsSynced,
		)
	}
}

// TestConcurrentCommitsAndReads runs commits beside readers of the same
// turn; run with -race.
func TestConcurrentCommitsAndReads(t *testing.T) {
	r := newTestRecorder(t)
	commitStep(t, r, 1)
	const steps = 30
	var wg sync.WaitGroup
	wg.Go(func() {
		for seq := int64(2); seq <= steps; seq++ {
			if err := r.Commit(t.Context(), request(seq)); err != nil {
				t.Errorf("commit seq %d: %v", seq, err)
				return
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			for {
				got, err := r.StepsAfter(t.Context(), "turn", 0, steps)
				if err != nil {
					t.Error(err)
					return
				}
				if len(got) == steps {
					return
				}
			}
		})
	}
	wg.Wait()
	requireCount(t, r, steps)
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
			Kind:    dashi.StepMessagesAdded,
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

func requireNothingWritten(t *testing.T, err error, want bool) {
	t.Helper()
	commitErr, ok := errors.AsType[*dashi.CommitError](err)
	if !ok || commitErr.NothingWritten != want {
		t.Fatalf(
			"commit returned %v; want a *dashi.CommitError with NothingWritten %t",
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
