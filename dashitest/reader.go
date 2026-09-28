package dashitest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
)

// readerTests check dashi.StepReader and dashi.StepWaiter. Run includes them
// and skips each one when the store does not implement the interface.
var readerTests = []struct {
	name string
	run  func(*testing.T, Store)
}{
	{"ReadsAfterCursor", testReadsAfterCursor},
	{"ReadsMissingTurn", testReadsMissingTurn},
	{"ReadsPastClaims", testReadsPastClaims},
	{"ReadsOneTurn", testReadsOneTurn},
	{"ReadsWhileWriting", testReadsWhileWriting},
	{"WaitReturnsForNewerStep", testWaitReturnsForNewerStep},
	{"WaitWakesOnCommit", testWaitWakesOnCommit},
	{"WaitEndsWithContext", testWaitEndsWithContext},
}

func reader(t *testing.T, s Store) dashi.StepReader {
	t.Helper()
	r, ok := s.(dashi.StepReader)
	if !ok {
		t.Skip("store does not implement dashi.StepReader")
	}
	return r
}

func waiter(t *testing.T, s Store) dashi.StepWaiter {
	t.Helper()
	reader(t, s)
	w, ok := s.(dashi.StepWaiter)
	if !ok {
		t.Skip("store does not implement dashi.StepWaiter")
	}
	return w
}

func testReadsAfterCursor(t *testing.T, s Store) {
	r := reader(t, s)
	steps := seed(t, s, turnID, 5)
	requirePage(t, r, turnID, 0, 100, steps)
	requirePage(t, r, turnID, 0, 2, steps[:2])
	requirePage(t, r, turnID, 2, 2, steps[2:4])
	requirePage(t, r, turnID, 4, 2, steps[4:])
	requirePage(t, r, turnID, 5, 2, nil)
	_, err := r.StepsAfter(t.Context(), turnID, 6, 1)
	if !errors.Is(err, dashi.ErrCursorAhead) {
		t.Fatalf(
			"StepsAfter past the last step returned %v; want "+
				"dashi.ErrCursorAhead, so a client with a foreign cursor resets",
			err,
		)
	}
}

func testReadsMissingTurn(t *testing.T, s Store) {
	r := reader(t, s)
	requirePage(t, r, "dashitest-missing", 0, 100, nil)
	_, err := r.StepsAfter(t.Context(), "dashitest-missing", 1, 1)
	if !errors.Is(err, dashi.ErrCursorAhead) {
		t.Fatalf(
			"StepsAfter(1) on a turn without steps returned %v; want "+
				"dashi.ErrCursorAhead",
			err,
		)
	}
}

func testReadsPastClaims(t *testing.T, s Store) {
	r := reader(t, s)
	steps := seed(t, s, turnID, 2)
	claim(t, s, turnID, "dashitest-takeover")
	next := contextStep(turnID, 3, "after a takeover")
	if err := commit(t, s, next, "dashitest-takeover"); err != nil {
		t.Fatalf("commit after claim: %v", err)
	}
	claim(t, s, turnID, owner)
	requirePage(t, r, turnID, 0, 100, append(steps, next))
	requirePage(t, r, turnID, 2, 100, []dashi.Step{next})
}

func testReadsOneTurn(t *testing.T, s Store) {
	r := reader(t, s)
	const other = "dashitest-other-turn"
	first := seed(t, s, turnID, 3)
	steps := seed(t, s, other, 2)
	// Read both, so a store that ignores the turn ID fails either way.
	requirePage(t, r, other, 0, 100, steps)
	requirePage(t, r, turnID, 0, 100, first)
	requirePage(t, r, turnID, 2, 100, first[2:])
	if _, err := r.StepsAfter(t.Context(), other, 3, 1); !errors.Is(err, dashi.ErrCursorAhead) {
		t.Fatalf("another turn's length leaked into a cursor check: %v", err)
	}
}

// A reader paging while the owner commits sees every step once, in order.
func testReadsWhileWriting(t *testing.T, s Store) {
	r := reader(t, s)
	const total = 30
	steps := seed(t, s, turnID, 1)
	for seq := int64(2); seq <= total; seq++ {
		steps = append(steps, contextStep(turnID, seq, fmt.Sprintf("step %d", seq)))
	}
	done := make(chan error, 1)
	go func() {
		for _, step := range steps[1:] {
			if err := commit(t, s, step, owner); err != nil {
				done <- fmt.Errorf("commit of seq %d: %w", step.Seq, err)
				return
			}
		}
		done <- nil
	}()
	var got []dashi.Step
	deadline := time.Now().Add(10 * time.Second)
	for int64(len(got)) < total {
		if time.Now().After(deadline) {
			t.Fatalf("reader saw %d of %d steps", len(got), total)
		}
		page, err := r.StepsAfter(t.Context(), turnID, int64(len(got)), 3)
		if err != nil {
			t.Fatalf("StepsAfter(%d) while writing: %v", len(got), err)
		}
		got = append(got, page...)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requireSteps(t, got, steps)
}

func testWaitReturnsForNewerStep(t *testing.T, s Store) {
	w := waiter(t, s)
	seed(t, s, turnID, 2)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := w.Wait(ctx, turnID, 1); err != nil || ctx.Err() != nil {
		t.Fatalf(
			"Wait(1) with seq 2 committed returned %v after ctx %v; want "+
				"an immediate nil, or a follower misses a committed step",
			err,
			ctx.Err(),
		)
	}
}

func testWaitWakesOnCommit(t *testing.T, s Store) {
	w := waiter(t, s)
	seed(t, s, turnID, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	woke := make(chan error, 1)
	go func() { woke <- w.Wait(ctx, turnID, 1) }()
	time.Sleep(20 * time.Millisecond)
	if err := commit(t, s, contextStep(turnID, 2, "wake"), owner); err != nil {
		t.Fatal(err)
	}
	if err := <-woke; err != nil {
		t.Fatalf("Wait did not wake on a commit: %v", err)
	}
}

func testWaitEndsWithContext(t *testing.T, s Store) {
	w := waiter(t, s)
	seed(t, s, turnID, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := w.Wait(ctx, turnID, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait with no new step returned %v; want ctx's error", err)
	}
}

func requirePage(
	t *testing.T,
	r dashi.StepReader,
	id string,
	after int64,
	limit int,
	want []dashi.Step,
) {
	t.Helper()
	got, err := r.StepsAfter(t.Context(), id, after, limit)
	if err != nil {
		t.Fatalf("StepsAfter(%q, %d, %d): %v", id, after, limit, err)
	}
	requireSteps(t, got, want)
}

// requireSteps checks count, order, and JSON meaning.
func requireSteps(t *testing.T, got, want []dashi.Step) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("read %d steps; want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].TurnID != want[i].TurnID || got[i].Seq != want[i].Seq {
			t.Fatalf(
				"step %d is seq %d of %q; want seq %d of %q",
				i,
				got[i].Seq,
				got[i].TurnID,
				want[i].Seq,
				want[i].TurnID,
			)
		}
		a, err := json.Marshal(got[i])
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(want[i])
		if err != nil {
			t.Fatal(err)
		}
		if same, err := sameJSON(a, b); err != nil || !same {
			t.Fatalf("read seq %d differs from the committed step:\n got %s\nwant %s", want[i].Seq, a, b)
		}
	}
}
