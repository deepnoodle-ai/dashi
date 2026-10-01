//go:build linux || darwin || freebsd || openbsd || netbsd || android || ios

package filerecorder

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
)

func TestReaderMissingStorageDoesNotCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing", "turns")
	if _, err := OpenReader(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing reader: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("created storage: %v", err)
	}
}

func TestReaderSnapshotsNoWritesOrLeaseAndTornTail(t *testing.T) {
	r := newTestRecorder(t)
	commitStep(t, r, 1)
	file, err := os.OpenFile(r.Path("turn"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(`{"step":{"seq":2`); err != nil {
		t.Fatal(err)
	}
	file.Close()
	before, err := os.ReadFile(r.Path("turn"))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, ok := any(reader).(dashi.Recorder); ok {
		t.Fatal("reader exposes Commit")
	}
	if _, ok := any(reader).(interface {
		Claim(context.Context, string, string) error
	}); ok {
		t.Fatal("reader exposes Claim")
	}
	got, err := reader.Load(t.Context(), "turn")
	if err != nil || len(got.Steps) != 1 {
		t.Fatalf("load=%+v %v", got, err)
	}
	want, err := r.Load(t.Context(), "turn")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("shared decoder differs: %v", err)
	}
	after, _ := os.ReadFile(r.Path("turn"))
	if !bytes.Equal(before, after) {
		t.Fatal("read repaired the torn tail")
	}
	commitStep(t, r, 2)
	got, err = reader.Load(t.Context(), "turn")
	if err != nil || len(got.Steps) != 2 {
		t.Fatalf("new snapshot=%+v %v", got, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Load(t.Context(), "turn"); err == nil {
		t.Fatal("closed reader still reads")
	}
}

func TestReaderIndexKeepsCorruptAndClaimOnlyTurns(t *testing.T) {
	r := newTestRecorder(t)
	for _, id := range []string{"s-1", "s-2"} {
		if err := r.Claim(t.Context(), id, "owner"); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(r.Path("s-2"), []byte("damaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if n, err := reader.LastTurn(t.Context(), "s"); n != 2 || err != nil {
		t.Fatalf("index=%d %v", n, err)
	}
	if _, err := reader.Load(t.Context(), "s-1"); !errors.Is(err, dashi.ErrTurnNotFound) {
		t.Fatalf("claim-only load: %v", err)
	}
	if _, err := reader.Load(t.Context(), "s-2"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt load: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reader.LastTurn(ctx, "s"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled index: %v", err)
	}
}

func TestReaderLockWaitCancellation(t *testing.T) {
	r := newTestRecorder(t)
	commitStep(t, r, 1)
	f, err := os.Open(r.Path("turn"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := lock(t.Context(), f, true); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := reader.Load(ctx, "turn"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait: %v", err)
	}
}

func TestReaderRejectsSymlinkDirectoryShardAndLog(t *testing.T) {
	for _, part := range []string{"directory", "shard", "log"} {
		t.Run(part, func(t *testing.T) {
			r := newTestRecorder(t)
			commitStep(t, r, 1)
			readerDir := r.dir
			path := r.Path("turn")
			switch part {
			case "directory":
				readerDir = filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(r.dir, readerDir); err != nil {
					t.Fatal(err)
				}
			case "shard":
				path = filepath.Dir(path)
			}
			if part != "directory" {
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-original", path); err != nil {
					t.Fatal(err)
				}
			}
			reader, err := OpenReader(readerDir)
			if part == "directory" {
				if err == nil {
					reader.Close()
					t.Fatal("followed directory link")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			if _, err := reader.Load(t.Context(), "turn"); err == nil {
				t.Fatalf("followed %s link", part)
			}
		})
	}
}
