//go:build linux || darwin || freebsd || openbsd || netbsd || android || ios

package filerecorder_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/dashitest"
	"github.com/deepnoodle-ai/dashi/filerecorder"
	"github.com/deepnoodle-ai/dashi/llm"
)

func TestConforms(t *testing.T) {
	dashitest.Run(t, func(t *testing.T) dashitest.Store {
		return newRecorder(t, t.TempDir())
	})
}

func TestConformsWithoutSync(t *testing.T) {
	dashitest.Run(t, func(t *testing.T) dashitest.Store {
		r, err := filerecorder.New(filerecorder.Config{
			Dir:         t.TempDir(),
			DisableSync: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	})
}

func TestNewRequiresDir(t *testing.T) {
	_, err := filerecorder.New(filerecorder.Config{})
	if err == nil || !strings.Contains(err.Error(), "Config.Dir") {
		t.Fatalf(
			"New with no Dir returned %v; want an error naming Config.Dir",
			err,
		)
	}
}

func TestClaimedTurnWithoutStepsIsNotFound(t *testing.T) {
	r := newRecorder(t, t.TempDir())
	if err := r.Claim(t.Context(), "turn", "owner"); err != nil {
		t.Fatal(err)
	}
	_, err := r.Load(t.Context(), "turn")
	if !errors.Is(err, dashi.ErrTurnNotFound) {
		t.Fatalf("load of a claimed turn without steps returned %v", err)
	}
	commit(t, r, step("turn", 1, "first"), "owner")
	requireSteps(t, r, "turn", 1)
}

func TestTornTailIsIgnoredAndReplaced(t *testing.T) {
	r := newRecorder(t, t.TempDir())
	commit(t, r, step("turn", 1, "first"), "")
	// The torn line is longer than the next record, so overwriting it in
	// place without truncating would leave part of it behind.
	torn := `{"step":{"version":1,"turn_id":"turn","seq":2,"x":"` +
		strings.Repeat("x", 4096)
	appendBytes(t, r.Path("turn"), torn)
	requireSteps(t, r, "turn", 1)
	commit(t, r, step("turn", 2, "second"), "")
	requireSteps(t, r, "turn", 2)
	data, err := os.ReadFile(r.Path("turn"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) ||
		bytes.Contains(data, []byte("xxx")) {
		t.Fatalf("torn line survived the repair:\n%s", data)
	}
	if lines := bytes.Count(data, []byte("\n")); lines != 3 {
		t.Fatalf(
			"log has %d lines after repair; want header and two steps",
			lines,
		)
	}
}

// A reader with its own index, as in another process, sees each commit on
// its next read, and a torn tail never appears as a step.
func TestStepsAfterFromAnotherRecorder(t *testing.T) {
	dir := t.TempDir()
	writer, reader := newRecorder(t, dir), newRecorder(t, dir)
	commit(t, writer, step("turn", 1, "first"), "")
	steps, err := reader.StepsAfter(t.Context(), "turn", 0, 100)
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps=%d err=%v", len(steps), err)
	}
	appendBytes(t, writer.Path("turn"), `{"step":{"version":1,"turn_id":"turn","seq":2`)
	if steps, err = reader.StepsAfter(t.Context(), "turn", 1, 100); err != nil || len(steps) != 0 {
		t.Fatalf("torn tail read as steps=%d err=%v", len(steps), err)
	}
	commit(t, writer, step("turn", 2, "second"), "")
	steps, err = reader.StepsAfter(t.Context(), "turn", 1, 100)
	if err != nil || len(steps) != 1 || steps[0].Seq != 2 {
		t.Fatalf("steps=%+v err=%v", steps, err)
	}
	if _, err := reader.StepsAfter(t.Context(), "turn", 3, 100); !errors.Is(err, dashi.ErrCursorAhead) {
		t.Fatalf("cursor ahead err=%v", err)
	}
}

// Wait notices a commit made through another recorder, as by another
// process, within about a second, without that recorder's wake.
func TestWaitSeesAnotherRecordersCommit(t *testing.T) {
	dir := t.TempDir()
	writer, reader := newRecorder(t, dir), newRecorder(t, dir)
	commit(t, writer, step("turn", 1, "first"), "")
	if _, err := reader.StepsAfter(t.Context(), "turn", 0, 100); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	woke := make(chan error, 1)
	go func() { woke <- reader.Wait(ctx, "turn", 1) }()
	time.Sleep(100 * time.Millisecond)
	began := time.Now()
	commit(t, writer, step("turn", 2, "second"), "")
	if err := <-woke; err != nil {
		t.Fatalf("Wait did not see the other recorder's commit: %v", err)
	}
	if waited := time.Since(began); waited > time.Second {
		t.Fatalf("Wait took %v to see another process's commit", waited)
	}
}

func TestTornHeaderIsReplaced(t *testing.T) {
	r := newRecorder(t, t.TempDir())
	path := r.Path("turn")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	appendBytes(t, path, `{"format":1,"tu`)
	_, err := r.Load(t.Context(), "turn")
	if !errors.Is(err, dashi.ErrTurnNotFound) {
		t.Fatalf("load of a torn header returned %v", err)
	}
	commit(t, r, step("turn", 1, "first"), "")
	requireSteps(t, r, "turn", 1)
}

func TestCorruptLogsFail(t *testing.T) {
	tests := map[string]func(t *testing.T, r *filerecorder.Recorder){
		"undecodable line": func(t *testing.T, r *filerecorder.Recorder) {
			commit(t, r, step("turn", 1, "first"), "")
			appendBytes(t, r.Path("turn"), "not json\n")
		},
		"header for another turn": func(t *testing.T, r *filerecorder.Recorder) {
			commit(t, r, step("other", 1, "first"), "")
			copyFile(t, r.Path("other"), r.Path("turn"))
		},
		"step and claim on one line": func(
			t *testing.T,
			r *filerecorder.Recorder,
		) {
			commit(t, r, step("turn", 1, "first"), "")
			appendBytes(
				t,
				r.Path("turn"),
				`{"step":{"turn_id":"turn","seq":2},"claim":{"owner":"x"}}`+"\n",
			)
		},
		"step out of sequence": func(t *testing.T, r *filerecorder.Recorder) {
			commit(t, r, step("turn", 1, "first"), "")
			appendBytes(
				t,
				r.Path("turn"),
				`{"step":{"version":1,"turn_id":"turn","seq":3}}`+"\n",
			)
		},
	}
	for name, damage := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRecorder(t, t.TempDir())
			damage(t, r)
			before, err := os.ReadFile(r.Path("turn"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Load(t.Context(), "turn")
			if !errors.Is(err, filerecorder.ErrCorrupt) {
				t.Fatalf("load returned %v; want ErrCorrupt", err)
			}
			if !strings.Contains(err.Error(), r.Path("turn")) {
				t.Fatalf("error %v does not name the log", err)
			}
			err = r.Commit(t.Context(), dashi.CommitRequest{
				Step:        step("turn", 2, "second"),
				ExpectedSeq: 1,
			})
			requireRejected(t, err, filerecorder.ErrCorrupt)
			after, err := os.ReadFile(r.Path("turn"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("a rejected commit changed a corrupt log")
			}
		})
	}
}

func TestTurnIDsMapToDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(t, dir)
	ids := []string{
		"A",
		"a",
		strings.Repeat("long/", 200),
		"../escape",
		"",
	}
	for i, id := range ids {
		for seq := int64(1); seq <= int64(i+1); seq++ {
			commit(t, r, step(id, seq, id), "")
		}
	}
	for i, id := range ids {
		requireSteps(t, r, id, i+1)
		path := r.Path(id)
		if !strings.HasPrefix(path, dir+string(filepath.Separator)) {
			t.Fatalf("path for %q is outside Dir: %s", id, path)
		}
	}
}

func TestFilePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "turns")
	r := newRecorder(t, dir)
	commit(t, r, step("turn", 1, "first"), "")
	for path, want := range map[string]fs.FileMode{
		dir:                          0o700,
		filepath.Dir(r.Path("turn")): 0o700,
		r.Path("turn"):               0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %v; want %v", path, got, want)
		}
	}
}

func TestLogIsReadableJSONLines(t *testing.T) {
	r := newRecorder(t, t.TempDir())
	if err := r.Claim(t.Context(), "turn", "owner"); err != nil {
		t.Fatal(err)
	}
	commit(t, r, step("turn", 1, "first"), "owner")
	data, err := os.ReadFile(r.Path("turn"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	prefixes := []string{
		`{"format":1,"turn_id":"turn"}`,
		`{"claim":`,
		`{"step":`,
	}
	if len(lines) != len(prefixes) {
		t.Fatalf(
			"log has %d lines; want %d:\n%s",
			len(lines),
			len(prefixes),
			data,
		)
	}
	for i, prefix := range prefixes {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Fatalf("line %d is %s; want prefix %s", i+1, lines[i], prefix)
		}
	}
}

// TestProcessesRaceForOneSequence runs several OS processes that each try to
// commit seq 2 at the same moment. Exactly one must win.
func TestProcessesRaceForOneSequence(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(t, dir)
	commit(t, r, step("turn", 1, "first"), "")
	const writers = 6
	start := time.Now().Add(500 * time.Millisecond)
	results := make([]string, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			results[i] = child(t, map[string]string{
				"MODE":     "commit",
				"DIR":      dir,
				"SEQ":      "2",
				"TEXT":     fmt.Sprintf("writer %d", i),
				"START_AT": strconv.FormatInt(start.UnixNano(), 10),
			})
		})
	}
	wg.Wait()
	wins := 0
	for i, result := range results {
		switch result {
		case "ok":
			wins++
		case "conflict":
		default:
			t.Fatalf("writer %d: %s", i, result)
		}
	}
	if wins != 1 {
		t.Fatalf(
			"%d processes committed seq 2; want exactly 1: %v",
			wins,
			results,
		)
	}
	requireSteps(t, r, "turn", 2)
}

// TestLeaseTakeoverAcrossProcesses has another process claim the turn and
// commit; this process's old lease can then no longer commit.
func TestLeaseTakeoverAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(t, dir)
	if err := r.Claim(t.Context(), "turn", "old"); err != nil {
		t.Fatal(err)
	}
	commit(t, r, step("turn", 1, "first"), "old")
	result := child(t, map[string]string{
		"MODE":  "takeover",
		"DIR":   dir,
		"SEQ":   "2",
		"TEXT":  "from the new owner",
		"OWNER": "new",
	})
	if result != "ok" {
		t.Fatalf("takeover process: %s", result)
	}
	err := r.Commit(t.Context(), dashi.CommitRequest{
		Step:        step("turn", 3, "from the old owner"),
		ExpectedSeq: 2,
		Owner:       "old",
	})
	requireRejected(t, err, dashi.ErrRevisionConflict)
	requireSteps(t, r, "turn", 2)
}

// TestChildProcess is the body of the processes the tests above start. It
// does nothing when run directly.
func TestChildProcess(t *testing.T) {
	mode := os.Getenv("FILERECORDER_CHILD_MODE")
	if mode == "" {
		t.Skip("run by other tests as a child process")
	}
	r, err := filerecorder.New(filerecorder.Config{
		Dir: os.Getenv("FILERECORDER_CHILD_DIR"),
	})
	if err != nil {
		fmt.Printf("RESULT:error %v\n", err)
		return
	}
	ctx := context.Background()
	owner := os.Getenv("FILERECORDER_CHILD_OWNER")
	if mode == "takeover" {
		if err := r.Claim(ctx, "turn", owner); err != nil {
			fmt.Printf("RESULT:error %v\n", err)
			return
		}
	}
	if at := os.Getenv("FILERECORDER_CHILD_START_AT"); at != "" {
		nanos, _ := strconv.ParseInt(at, 10, 64)
		time.Sleep(time.Until(time.Unix(0, nanos)))
	}
	seq, _ := strconv.ParseInt(os.Getenv("FILERECORDER_CHILD_SEQ"), 10, 64)
	err = r.Commit(ctx, dashi.CommitRequest{
		Step:        step("turn", seq, os.Getenv("FILERECORDER_CHILD_TEXT")),
		ExpectedSeq: seq - 1,
		Owner:       owner,
	})
	switch {
	case err == nil:
		fmt.Println("RESULT:ok")
	case errors.Is(err, dashi.ErrRevisionConflict):
		fmt.Println("RESULT:conflict")
	default:
		fmt.Printf("RESULT:error %v\n", err)
	}
}

func child(t *testing.T, env map[string]string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildProcess$", "-test.v")
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, "FILERECORDER_CHILD_"+k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("process failed: %v\n%s", err, out)
	}
	for line := range strings.Lines(string(out)) {
		if result, ok := strings.CutPrefix(line, "RESULT:"); ok {
			return strings.TrimSpace(result)
		}
	}
	return fmt.Sprintf("no result:\n%s", out)
}

func newRecorder(t testing.TB, dir string) *filerecorder.Recorder {
	t.Helper()
	r, err := filerecorder.New(filerecorder.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var stepTime = time.Date(2026, 1, 2, 3, 4, 5, 678901234, time.UTC)

func step(turnID string, seq int64, text string) dashi.Step {
	s := dashi.Step{
		Version: 1,
		TurnID:  turnID,
		Seq:     seq,
		At:      stepTime.Add(time.Duration(seq) * time.Millisecond),
		Kind:    dashi.StepDelivered,
		Context: &dashi.ContextFact{
			Items: []dashi.ContextItem{
				{Origin: "user", Message: llm.UserMessage(llm.Text(text))},
			},
		},
	}
	if seq == 1 {
		s.Kind = dashi.StepStarted
		s.CommandID = "start"
		s.CommandDigest = "digest"
	}
	return s
}

func commit(
	t testing.TB,
	r *filerecorder.Recorder,
	s dashi.Step,
	owner string,
) {
	t.Helper()
	err := r.Commit(t.Context(), dashi.CommitRequest{
		Step:        s,
		ExpectedSeq: s.Seq - 1,
		Owner:       owner,
	})
	if err != nil {
		t.Fatalf("commit seq %d of %q: %v", s.Seq, s.TurnID, err)
	}
}

func requireSteps(t *testing.T, r *filerecorder.Recorder, id string, n int) {
	t.Helper()
	turn, err := r.Load(t.Context(), id)
	if err != nil {
		t.Fatalf("load %q: %v", id, err)
	}
	if turn.ID != id || len(turn.Steps) != n {
		t.Fatalf(
			"loaded %q with %d steps; want %q with %d",
			turn.ID,
			len(turn.Steps),
			id,
			n,
		)
	}
}

func requireRejected(t *testing.T, err, target error) {
	t.Helper()
	commitErr, ok := errors.AsType[*dashi.CommitError](err)
	if !ok || commitErr.Persistence != dashi.PersistenceRejected {
		t.Fatalf("commit returned %v; want a rejected *dashi.CommitError", err)
	}
	if !errors.Is(err, target) {
		t.Fatalf("commit error %v does not wrap %v", err, target)
	}
}

func appendBytes(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
