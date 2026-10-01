package dashitest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/deepnoodle-ai/dashi"
	"github.com/deepnoodle-ai/dashi/llm"
)

// Run checks a recorder against the commit contract Runner.Run relies on.
// newStore must return an empty store for each subtest; subtests do not share
// turns. Every commit in the suite carries an owner token claimed first, so a
// recorder that requires a lease for every write can still be tested.
//
// Each check follows from the runner's use of the recorder:
//
//   - Steps append in sequence, and Load returns each one with the JSON
//     meaning it was committed with. Key order, whitespace, string escaping,
//     and the spelling of numerically equal numbers may change, and a
//     duplicate key resolves to its last value, so a Postgres jsonb column
//     conforms; the runner compares recorded values by canonical JSON. Step
//     times use microsecond precision, which timestamp columns preserve.
//   - Load of a turn with no steps returns an error matching
//     dashi.ErrTurnNotFound or an empty turn with the requested ID. Run
//     treats any other Load error as an unreadable record and stops.
//   - A commit whose ExpectedSeq is not the turn's last step Seq is
//     rejected with dashi.ErrStaleTurn, including a different step
//     at an occupied sequence number. A step that differs only in its time
//     is different: a worker retrying from an old snapshot builds new steps,
//     and this rejection is what stops it before it repeats a model call or
//     tool execution.
//   - A commit from an owner other than the one last claimed is rejected.
//   - A retry of an acknowledged step with the same JSON meaning succeeds
//     without adding a step, so a commit whose acknowledgement was lost can
//     be retried.
//   - A step with an already recorded command ID succeeds without adding a
//     step when its kind and command hash match, and is rejected with
//     dashi.ErrCommandIDReused when they differ. Command IDs are scoped to
//     one turn.
//   - Turns are independent: a step for one turn never depends on another
//     turn's steps.
//   - Of concurrent commits at the same ExpectedSeq, exactly one succeeds.
//
// When the store implements dashi.StepReader, Run also checks the rules
// dashi.Follow relies on; otherwise those subtests are skipped:
//
//   - StepsAfter pages exactly: ascending, contiguous Seq from after+1, each
//     step with its committed JSON meaning, only the requested turn's
//     steps, and the limit honored. A cursor at the last step gives no
//     steps.
//   - A turn without steps gives no steps for cursor zero, and a cursor past
//     the last step gives an error matching dashi.ErrCursorAhead.
//   - Claims never appear as steps, and a reader paging while the owner
//     commits sees every step once, in order.
//   - With dashi.StepWaiter, Wait returns at once when a newer step exists,
//     wakes on a commit, and returns ctx's error when ctx ends.
//
// Run also checks what session.Session relies on:
//
//   - A store that implements session.Index agrees with the turns
//     committed: LastTurn is 0 for a session with none, counts a session's
//     turns by their session.TurnID, never counts another session whose ID
//     shares a prefix, and counts a turn two workers raced to start once.
//   - Without the index, a session of 1, 2, or 3 turns finds its last turn
//     by probing turn IDs in at most 2·log2(n)+2 reads, plus one to load
//     the last turn: through StepReader when the store is one, and through
//     Loader alone. Sessions whose IDs contain dashes find their own turns
//     beside turn IDs that share a prefix ("x-1" beside "x-10", session "x"
//     beside session "x-1").
//
// "Rejected" means a *dashi.CommitError with NothingWritten set,
// after which Load must show the record unchanged. Run does not check
// recorder behavior Dashi does not rely on, such as validating step payloads.
func Run(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()
	tests := []struct {
		name string
		run  func(*testing.T, Store)
	}{
		{"AppendsInOrder", testAppendsInOrder},
		{"LoadWithoutSteps", testLoadWithoutSteps},
		{"RejectsStaleRevision", testRejectsStaleTurn},
		{"RejectsRetimedStep", testRejectsRetimedStep},
		{"RejectsRevisionGap", testRejectsSeqGap},
		{"RejectsStaleOwner", testRejectsStaleOwner},
		{"RetriesIdenticalStep", testRetriesIdenticalStep},
		{"RetriesCommand", testRetriesCommand},
		{"RejectsCommandConflict", testRejectsCommandConflict},
		{"IsolatesTurns", testIsolatesTurns},
		{"SerializesConcurrentCommits", testSerializesConcurrentCommits},
	}
	tests = append(tests, readerTests...)
	tests = append(tests, sessionTests...)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.run(t, newStore(t))
		})
	}
}

const (
	turnID = "dashitest-turn"
	owner  = "dashitest-owner"
)

func testAppendsInOrder(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 3)
	requireRecord(t, s, turnID, steps)
}

func testLoadWithoutSteps(t *testing.T, s Store) {
	turn, err := s.Load(t.Context(), "dashitest-missing")
	switch {
	case errors.Is(err, dashi.ErrTurnNotFound):
	case err != nil:
		t.Fatalf(
			"load of a turn without steps returned %v; want "+
				"dashi.ErrTurnNotFound, or Run cannot start the turn",
			err,
		)
	case turn.ID != "dashitest-missing" || len(turn.Steps) != 0:
		t.Fatalf(
			"load of a turn without steps returned id=%q steps=%d; want "+
				"dashi.ErrTurnNotFound or an empty turn with the requested ID",
			turn.ID,
			len(turn.Steps),
		)
	}
}

func testRejectsStaleTurn(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 2)
	stale := contextStep(turnID, 2, "a different second step")
	err := commit(t, s, stale, owner)
	requireRejected(t, err, dashi.ErrStaleTurn)
	requireRecord(t, s, turnID, steps)
}

func testRejectsRetimedStep(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 2)
	retimed := steps[1]
	retimed.At = retimed.At.Add(time.Second)
	err := commit(t, s, retimed, owner)
	requireRejected(t, err, dashi.ErrStaleTurn)
	requireRecord(t, s, turnID, steps)
}

func testRejectsSeqGap(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 2)
	err := commit(t, s, contextStep(turnID, 4, "skips a revision"), owner)
	requireRejected(t, err, dashi.ErrStaleTurn)
	requireRecord(t, s, turnID, steps)
}

func testRejectsStaleOwner(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 1)
	claim(t, s, turnID, "dashitest-takeover")
	next := contextStep(turnID, 2, "from the old owner")
	requireRejected(t, commit(t, s, next, owner), nil)
	requireRecord(t, s, turnID, steps)
	if err := commit(t, s, next, "dashitest-takeover"); err != nil {
		t.Fatalf("new owner could not commit after claim: %v", err)
	}
	requireRecord(t, s, turnID, append(steps, next))
}

func testRetriesIdenticalStep(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 2)
	if err := commit(t, s, steps[1], owner); err != nil {
		t.Fatalf("identical retry of an acknowledged step failed: %v", err)
	}
	requireRecord(t, s, turnID, steps)
}

func testRetriesCommand(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 1)
	retry := steps[0]
	retry.At = retry.At.Add(time.Second)
	if err := commit(t, s, retry, owner); err != nil {
		t.Fatalf("retry of a recorded command failed: %v", err)
	}
	requireRecord(t, s, turnID, steps)
}

func testRejectsCommandConflict(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 1)
	conflict := steps[0]
	conflict.CommandHash = "dashitest-different-digest"
	err := commit(t, s, conflict, owner)
	requireRejected(t, err, dashi.ErrCommandIDReused)
	requireRecord(t, s, turnID, steps)
}

func testIsolatesTurns(t *testing.T, s Store) {
	const other = "dashitest-other-turn"
	steps := seed(t, s, turnID, 2)
	claim(t, s, other, owner)
	err := commit(t, s, contextStep(other, 3, "wrong turn"), owner)
	requireRejected(t, err, dashi.ErrStaleTurn)
	start := startStep(other)
	start.CommandHash = "dashitest-other-digest"
	if err := commit(t, s, start, owner); err != nil {
		t.Fatalf("command ID reused in another turn was not accepted: %v", err)
	}
	requireRecord(t, s, other, []dashi.Step{start})
	requireRecord(t, s, turnID, steps)
}

func testSerializesConcurrentCommits(t *testing.T, s Store) {
	steps := seed(t, s, turnID, 1)
	const writers = 8
	candidates := make([]dashi.Step, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		candidates[i] = contextStep(turnID, 2, fmt.Sprintf("writer %d", i))
		wg.Go(func() {
			errs[i] = commit(t, s, candidates[i], owner)
		})
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		if err != nil {
			continue
		}
		if winner >= 0 {
			t.Fatalf("writers %d and %d both committed seq 2", winner, i)
		}
		winner = i
	}
	if winner < 0 {
		t.Fatalf("no concurrent writer committed seq 2: %v", errs)
	}
	requireRecord(t, s, turnID, append(steps, candidates[winner]))
}

// seed claims the turn and commits n steps: a start followed by context.
func seed(t *testing.T, s Store, id string, n int) []dashi.Step {
	t.Helper()
	claim(t, s, id, owner)
	steps := []dashi.Step{startStep(id)}
	for seq := int64(2); seq <= int64(n); seq++ {
		steps = append(steps, contextStep(id, seq, fmt.Sprintf("step %d", seq)))
	}
	for _, step := range steps {
		if err := commit(t, s, step, owner); err != nil {
			t.Fatalf("commit of seq %d failed: %v", step.Seq, err)
		}
	}
	return steps
}

func claim(t *testing.T, s Store, id, by string) {
	t.Helper()
	if err := s.Claim(t.Context(), id, by); err != nil {
		t.Fatalf("claim %q for %q: %v", id, by, err)
	}
}

func commit(t *testing.T, s Store, step dashi.Step, by string) error {
	return s.Commit(
		t.Context(),
		dashi.CommitRequest{
			Step:        step,
			ExpectedSeq: step.Seq - 1,
			Owner:       by,
		},
	)
}

// stepTime has microsecond precision, which timestamp columns preserve.
var stepTime = time.Date(2026, 1, 2, 3, 4, 5, 678901000, time.UTC)

func startStep(id string) dashi.Step {
	def := dashi.Agent{
		ID:       "dashitest",
		Version:  "r1",
		Provider: "test",
		API:      "test.chat",
		Model:    "fixture",
	}
	return dashi.Step{
		Version:     1,
		TurnID:      id,
		Seq:         1,
		At:          stepTime,
		Kind:        dashi.StepTurnStarted,
		CommandID:   "start",
		CommandHash: "dashitest-digest",
		Started: &dashi.StartFact{
			Agent: def,
			// Raw JSON that a meaning-preserving store may respell: keys out
			// of jsonb order, extra whitespace, and an exponent number.
			OutputSchema: json.RawMessage(
				`{"required": ["z"], "type": "object", ` +
					`"properties": {"z": {"maxLength": 1e2}}}`,
			),
			Input: []llm.Message{llm.UserMessage(llm.Text("begin <now> & go"))},
		},
	}
}

func contextStep(id string, seq int64, text string) dashi.Step {
	return dashi.Step{
		Version: 1,
		TurnID:  id,
		Seq:     seq,
		At:      stepTime.Add(time.Duration(seq) * time.Millisecond),
		Kind:    dashi.StepMessagesAdded,
		Context: &dashi.ContextFact{
			Items: []dashi.ContextItem{
				{Origin: dashi.ContextUser, Message: llm.UserMessage(llm.Text(text))},
			},
		},
	}
}

func requireRejected(t *testing.T, err error, target error) {
	t.Helper()
	commitErr, ok := errors.AsType[*dashi.CommitError](err)
	switch {
	case err == nil:
		t.Fatal("commit succeeded; want a rejected commit")
	case !ok:
		t.Fatalf("commit error %v is not a *dashi.CommitError", err)
	case !commitErr.NothingWritten:
		t.Fatal("commit error does not say nothing was written")
	case target != nil && !errors.Is(err, target):
		t.Fatalf("commit error %v does not wrap %v", err, target)
	}
}

func requireRecord(t *testing.T, s Store, id string, want []dashi.Step) {
	t.Helper()
	turn, err := s.Load(t.Context(), id)
	if err != nil {
		t.Fatalf("load %q: %v", id, err)
	}
	if turn.ID != id {
		t.Fatalf("loaded turn ID %q; want %q", turn.ID, id)
	}
	if len(turn.Steps) != len(want) {
		t.Fatalf("loaded %d steps; want %d", len(turn.Steps), len(want))
	}
	for i := range want {
		got, err := json.Marshal(turn.Steps[i])
		if err != nil {
			t.Fatalf("encode loaded seq %d: %v", i+1, err)
		}
		expected, err := json.Marshal(want[i])
		if err != nil {
			t.Fatalf("encode committed seq %d: %v", i+1, err)
		}
		same, err := sameJSON(got, expected)
		if err != nil {
			t.Fatalf("decode seq %d: %v", i+1, err)
		}
		if !same {
			t.Fatalf(
				"loaded seq %d differs in meaning from the committed step:\n got %s\nwant %s",
				i+1,
				got,
				expected,
			)
		}
	}
}

// sameJSON reports whether a and b have the same JSON meaning: the same
// structure, names, strings, and literals, with numerically equal numbers.
// It matches Dashi's canonical JSON comparison without importing it.
func sameJSON(a, b []byte) (bool, error) {
	x, err := decodeJSON(a)
	if err != nil {
		return false, err
	}
	y, err := decodeJSON(b)
	if err != nil {
		return false, err
	}
	return sameValue(x, y), nil
}

// decodeJSON keeps numbers as their literal text so no precision is lost;
// Go's decoder already resolves a duplicate name to its last value.
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func sameValue(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !sameValue(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !sameValue(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		p, okP := new(big.Rat).SetString(string(x))
		q, okQ := new(big.Rat).SetString(string(y))
		return okP && okQ && p.Cmp(q) == 0
	default:
		return a == b
	}
}
