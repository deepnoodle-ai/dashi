package dashitest

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/session"
)

// sessionTests check what session.Session relies on: a SessionIndex that
// agrees with the turns committed, and turn IDs that probing finds in few
// reads.
var sessionTests = []struct {
	name string
	run  func(*testing.T, Store)
}{
	{"SessionIndexAgrees", testSessionIndexAgrees},
	{"SessionProbing", testSessionProbing},
}

const sessionID = "dashitest-session"

func testSessionIndexAgrees(t *testing.T, s Store) {
	idx, ok := s.(session.Index)
	if !ok {
		t.Skip("store does not implement session.Index")
	}
	requireLast := func(id string, want int) {
		t.Helper()
		got, err := idx.LastTurn(t.Context(), id)
		if err != nil {
			t.Fatalf("LastTurn(%q): %v", id, err)
		}
		if got != want {
			t.Fatalf("LastTurn(%q) = %d; want %d", id, got, want)
		}
	}
	requireLast(sessionID, 0)
	for n := 1; n <= 3; n++ {
		seed(t, s, session.TurnID(sessionID, n), n)
	}
	// Session IDs may contain dashes: turn 1 of "dashitest-session-1" is
	// not a turn of "dashitest-session".
	seed(t, s, session.TurnID(sessionID+"-1", 1), 1)
	requireLast(sessionID, 3)
	requireLast(sessionID+"-1", 1)

	// Two workers race to start turn 4 with different commands; the index
	// counts the turn the winner committed, once.
	next := session.TurnID(sessionID, 4)
	claim(t, s, next, owner)
	var wg sync.WaitGroup
	var wins atomic.Int32
	for _, cmd := range []string{"start-a", "start-b"} {
		step := startStep(next)
		step.CommandID = cmd
		step.CommandHash = "dashitest-digest-" + cmd
		wg.Go(func() {
			if commit(t, s, step, owner) == nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d racing starts of turn 4 committed; want 1", wins.Load())
	}
	requireLast(sessionID, 4)
}

// testSessionProbing finds the last turn of sessions of 1, 2, and 3 turns
// without a SessionIndex, through StepReader when the store is one and
// through Loader alone, in at most 2·log2(n)+2 reads, plus one to load the
// last turn. Session IDs may contain dashes, so it also opens sessions whose
// turn IDs share a prefix: "x" (turn "x-1") beside a lone record "x-10", and
// session "x-1" (turns "x-1-1" and "x-1-2"). A store that matches turn IDs
// by prefix returns the wrong turn there. The read count is a property of
// the session's search, not of the store, so the root tests check it at
// larger sizes.
func testSessionProbing(t *testing.T, s Store) {
	_, reads := s.(dashi.StepReader)
	open := func(id string, n int, steps int64) {
		t.Helper()
		limit := int(2*math.Log2(float64(n))+2) + 1
		probe := func(rec dashi.Recorder, counter *atomic.Int64) {
			t.Helper()
			handle := &session.Session{ID: id, Runner: dashi.Runner{Recorder: rec}}
			st, err := handle.Open(t.Context())
			if err != nil {
				t.Fatalf("open session %q of %d turns: %v", id, n, err)
			}
			if st.Turns != n {
				t.Fatalf("session %q of %d turns opened with %d", id, n, st.Turns)
			}
			if want := session.TurnID(id, n); st.Last.ID != want ||
				st.Last.LastSeq != steps {
				t.Fatalf(
					"session %q: last turn %s at revision %d; want %s at %d",
					id,
					st.Last.ID,
					st.Last.LastSeq,
					want,
					steps,
				)
			}
			if got := counter.Load(); got > int64(limit) {
				t.Fatalf(
					"session %q: finding turn %d took %d reads; want at most %d",
					id,
					n,
					got,
					limit,
				)
			}
		}
		loads := &countingLoader{store: s}
		probe(loads, &loads.reads)
		if reads {
			steps := &countingReader{countingLoader: &countingLoader{store: s}}
			probe(steps, &steps.reads)
		}
	}
	for _, n := range []int{1, 2, 3} {
		id := fmt.Sprintf("%s-probe-%d", sessionID, n)
		for i := 1; i <= n; i++ {
			seed(t, s, session.TurnID(id, i), 1)
		}
		open(id, n, 1)
	}

	// Each record has its own step count, so a store that answers for the
	// wrong turn shows up in the last turn's step count.
	x := sessionID + "-x"
	seed(t, s, session.TurnID(x, 1), 1)
	seed(t, s, x+"-10", 3)
	seed(t, s, session.TurnID(x+"-1", 1), 2)
	seed(t, s, session.TurnID(x+"-1", 2), 2)
	open(x, 1, 1)
	open(x+"-1", 2, 2)
}

// countingLoader hides every capability of a store but Loader, and counts
// reads.
type countingLoader struct {
	store Store
	reads atomic.Int64
}

func (c *countingLoader) Commit(ctx context.Context, req dashi.CommitRequest) error {
	return c.store.Commit(ctx, req)
}

func (c *countingLoader) Load(ctx context.Context, id string) (dashi.Turn, error) {
	c.reads.Add(1)
	return c.store.Load(ctx, id)
}

// countingReader adds the store's StepReader, whose reads it also counts.
type countingReader struct {
	*countingLoader
}

func (c *countingReader) StepsAfter(
	ctx context.Context,
	id string,
	after int64,
	limit int,
) ([]dashi.Step, error) {
	c.reads.Add(1)
	return c.store.(dashi.StepReader).StepsAfter(ctx, id, after, limit)
}
